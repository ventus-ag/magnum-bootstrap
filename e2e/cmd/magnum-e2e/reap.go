package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clusters"
)

const (
	e2eClusterPrefix = "recon-e2e-"
	// keepLabel marks a cluster kept on purpose (KEEP_CLUSTER=1); never reaped.
	keepLabel = "e2e_keep"
	// staleAfter exceeds the e2e job timeout (720 min), so no live job can
	// still own an older cluster.
	staleAfter = 13 * time.Hour
)

// staleE2ECluster reports whether c is an e2e cluster no running job can own:
// a cancelled run skips its always() teardown, and every leaked cluster keeps
// two Nova server groups (quota 10) until creates start failing.
func staleE2ECluster(c clusters.Cluster, now time.Time) bool {
	if !strings.HasPrefix(c.Name, e2eClusterPrefix) || c.Labels[keepLabel] == "true" {
		return false
	}
	if c.Status == "DELETE_IN_PROGRESS" {
		return false
	}
	return now.Sub(c.CreatedAt) > staleAfter
}

// reapStale deletes stale e2e clusters (plus their keypair and staged binary)
// in parallel and waits for them to go. Best-effort: errors are logged.
func (r *runner) reapStale(ctx context.Context) {
	pages, err := clusters.ListDetail(r.magnum, nil).AllPages(ctx)
	if err != nil {
		r.err("reap-stale: list clusters: %v", err)
		return
	}
	all, err := clusters.ExtractClusters(pages)
	if err != nil {
		r.err("reap-stale: extract clusters: %v", err)
		return
	}
	now := time.Now()
	var wg sync.WaitGroup
	for _, c := range all {
		if !staleE2ECluster(c, now) {
			continue
		}
		r.log("reap-stale: deleting %s (status %s, created %s ago)", c.Name, c.Status, now.Sub(c.CreatedAt).Round(time.Minute))
		sr := *r
		sr.cfg.clusterName = c.Name
		sr.stackNameCache = ""
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sr.deleteCluster(ctx); err != nil {
				sr.err("reap-stale: %s: %v", sr.cfg.clusterName, err)
			}
		}()
	}
	wg.Wait()
}
