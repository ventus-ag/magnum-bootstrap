package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	gophercloud "github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/nodegroups"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// resizeFlavorCycle drives the fork's in-place flavor resize: nodegroup PATCH
// /flavor_id → params-only Heat stack update → Nova resize of every member,
// rolled batch-1 — first on the default worker nodegroup, then the master
// nodegroup (proves etcd survives serial member reboots). Each stage waits for
// UPDATE_COMPLETE and then asserts every backing Nova server actually carries
// the target flavor before the final bundle. Target comes from RESIZE_FLAVOR
// (-resize-flavor); preflight resolves it in Nova so a typo dies before any
// billed resource exists.
func (r *runner) resizeFlavorCycle(ctx context.Context) error {
	target := r.cfg.resizeFlavor
	if target == "" {
		return fmt.Errorf("resize-flavor op requires RESIZE_FLAVOR (-resize-flavor)")
	}
	targetID, err := r.flavorIDByName(ctx, target)
	if err != nil {
		return err
	}
	for _, role := range []string{"worker", "master"} {
		ng, err := r.resolveNodeGroup(ctx, role)
		if err != nil {
			return err
		}
		if ng.FlavorID == target || ng.FlavorID == targetID {
			r.log("nodegroup %q already on flavor %s — skipping patch", ng.Name, target)
		} else if err := r.runMutationNoBundle(ctx, "resize-flavor:"+role+"->"+target, func() error {
			return r.patchNodeGroupFlavor(ctx, ng, target)
		}); err != nil {
			return err
		}
		if err := r.verifyNodeGroupFlavor(ctx, ng, target, targetID); err != nil {
			return err
		}
	}
	return r.verifyBundle(ctx, "resize-flavor", true)
}

// patchNodeGroupFlavor PATCHes the nodegroup's flavor_id via a raw JSON-patch
// (same transport as patchNodepoolMetadata — the typed nodegroups.Update only
// accepts a 202, and Magnum builds may answer 200). "add" upserts per RFC 6902,
// so it also covers a nodegroup whose flavor_id is still null. Flavor is the
// only op in the request: the fork rejects combining it with a node_count
// change.
func (r *runner) patchNodeGroupFlavor(ctx context.Context, ng *nodegroups.NodeGroup, target string) error {
	ops := []map[string]any{{"op": "add", "path": "/flavor_id", "value": target}}
	url := r.magnum.ServiceURL("clusters", r.cfg.clusterName, "nodegroups", ng.UUID)
	if _, err := r.magnum.Patch(ctx, url, ops, nil, &gophercloud.RequestOpts{OkCodes: []int{200, 202}}); err != nil {
		return fmt.Errorf("patch nodegroup %q flavor -> %s: %w", ng.Name, target, err)
	}
	r.log("nodegroup %q flavor patched -> %s", ng.Name, target)
	return nil
}

// flavorIDByName resolves a flavor name (or ID) to its Nova ID.
func (r *runner) flavorIDByName(ctx context.Context, name string) (string, error) {
	nova, err := r.computeClient()
	if err != nil {
		return "", err
	}
	pages, err := flavors.ListDetail(nova, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("list flavors: %w", err)
	}
	fls, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return "", fmt.Errorf("extract flavors: %w", err)
	}
	for _, f := range fls {
		if f.Name == name || f.ID == name {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("flavor %q not found in Nova (%d flavors visible)", name, len(fls))
}

// verifyNodeGroupFlavor asserts the nodegroup records the target flavor AND
// every Nova server backing the nodegroup's k8s Nodes runs it — hard proof the
// in-place resize happened, not just a DB field flip. Nodes are joined to Nova
// via spec.providerID (openstack:///<instance-uuid>, written by the
// reconciler); the nodegroup label is the primary selector with the role label
// as fallback for nodes predating NODEGROUP_NAME.
func (r *runner) verifyNodeGroupFlavor(ctx context.Context, ng *nodegroups.NodeGroup, target, targetID string) error {
	fresh, err := nodegroups.Get(ctx, r.magnum, r.cfg.clusterName, ng.UUID).Extract()
	if err != nil {
		return fmt.Errorf("get nodegroup %q: %w", ng.Name, err)
	}
	if fresh.FlavorID != target && fresh.FlavorID != targetID {
		return fmt.Errorf("nodegroup %q flavor_id=%q, want %q", ng.Name, fresh.FlavorID, target)
	}
	kc, err := r.k8sClient(ctx)
	if err != nil {
		return err
	}
	nova, err := r.computeClient()
	if err != nil {
		return err
	}
	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "magnum.openstack.org/nodegroup=" + ng.Name,
	})
	if err != nil {
		return fmt.Errorf("list %s nodes: %w", ng.Name, err)
	}
	if len(nodes.Items) == 0 {
		nodes, err = kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "magnum.openstack.org/role=" + ng.Role,
		})
		if err != nil {
			return fmt.Errorf("list %s-role nodes: %w", ng.Role, err)
		}
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("no k8s nodes found for nodegroup %s (label + role selectors)", ng.Name)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for _, node := range nodes.Items {
		id := strings.TrimPrefix(node.Spec.ProviderID, "openstack:///")
		if id == "" || id == node.Spec.ProviderID {
			return fmt.Errorf("node %s: unexpected providerID %q", node.Name, node.Spec.ProviderID)
		}
		for {
			srv, gerr := servers.Get(ctx, nova, id).Extract()
			if gerr != nil {
				return fmt.Errorf("nova server %s (node %s): %w", id, node.Name, gerr)
			}
			gotID, _ := srv.Flavor["id"].(string)
			gotName, _ := srv.Flavor["original_name"].(string)
			if gotID == targetID || gotName == target {
				r.log("node %s (server %s) on flavor %s ✅", node.Name, id, target)
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("node %s server %s flavor=%q/%q, want %s (%s)",
					node.Name, id, gotID, gotName, target, targetID)
			}
			time.Sleep(15 * time.Second)
		}
	}
	r.log("nodegroup %q: %d node(s) verified on flavor %s", ng.Name, len(nodes.Items), target)
	return nil
}
