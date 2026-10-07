package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/orchestration/v1/stackresources"
	"github.com/gophercloud/gophercloud/v2/openstack/orchestration/v1/stacks"
)

const heatSettleTimeout = 30 * time.Minute

// waitHeatSettled waits until no resource or per-node member stack in the
// cluster's Heat tree is in progress. Label reconfigure and CA rotation update
// the member stacks directly while Magnum follows the root stack, so it can
// report UPDATE_COMPLETE before any node has the change. A FAILED member fails
// the op: Magnum stops polling once it reports COMPLETE and never shows it.
func (r *runner) waitHeatSettled(ctx context.Context) error {
	c, err := r.getCluster(ctx)
	if err != nil || c.StackID == "" {
		return nil
	}
	orch, err := r.orchClient()
	if err != nil {
		r.log("heat-settle: skipped (%v)", err)
		return nil
	}
	stackName, err := r.resolveStackName(ctx, c.StackID)
	if err != nil {
		r.log("heat-settle: skipped (%v)", err)
		return nil
	}
	deadline := time.Now().Add(heatSettleTimeout)
	logged := false
	for {
		busy, err := heatTreeBusy(ctx, orch, stackName, c.StackID)
		switch {
		case err != nil:
			return err
		case busy == "":
			if logged {
				r.log("heat-settle: member stacks settled")
			}
			return nil
		case !logged:
			r.log("heat-settle: Magnum reports %s but %s — waiting for the nodes", c.Status, busy)
			logged = true
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("heat-settle: still in progress after %s: %s", heatSettleTimeout, busy)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// heatTreeBusy describes the first in-progress resource or member stack, or
// returns "" when the tree is settled. Transient API errors count as busy.
func heatTreeBusy(ctx context.Context, orch *gophercloud.ServiceClient, stackName, stackID string) (string, error) {
	pages, err := stackresources.List(orch, stackName, stackID, stackresources.ListOpts{Depth: 2}).AllPages(ctx)
	if err != nil {
		return "Heat resource list failed: " + err.Error(), nil
	}
	res, err := stackresources.ExtractResources(pages)
	if err != nil {
		return "Heat resource list unreadable: " + err.Error(), nil
	}
	busy, members := heatProgress(res)
	if busy != "" {
		return busy, nil
	}
	for _, id := range members {
		s, err := stacks.Find(ctx, orch, id).Extract()
		if err != nil {
			return "member stack " + id + " unreadable: " + err.Error(), nil
		}
		switch {
		case strings.HasSuffix(s.Status, "_FAILED"):
			return "", fmt.Errorf("member stack %s is %s: %s", s.Name, s.Status, s.StatusReason)
		case strings.HasSuffix(s.Status, "_IN_PROGRESS"):
			return "member stack " + s.Name + " is " + s.Status, nil
		}
	}
	return "", nil
}

// heatProgress scans a depth-2 resource list (root → node group → member):
// the first in-progress resource, else the member stack IDs to check.
func heatProgress(res []stackresources.Resource) (string, []string) {
	var members []string
	for _, rs := range res {
		if strings.HasSuffix(rs.Status, "_IN_PROGRESS") {
			return rs.Name + " is " + rs.Status, nil
		}
		if _, err := strconv.Atoi(rs.Name); err == nil && rs.PhysicalID != "" && !strings.HasPrefix(rs.Type, "OS::") {
			members = append(members, rs.PhysicalID)
		}
	}
	return "", members
}
