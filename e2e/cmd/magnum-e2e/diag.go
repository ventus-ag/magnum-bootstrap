package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/orchestration/v1/stackresources"
)

// resolveStackName returns the cluster's real Heat stack NAME. Magnum does NOT
// name the stack after the cluster — it truncates the cluster name and appends a
// stack short-id (e.g. cluster "…-27885457659" → stack "…-27885-ymdcavbxi2o2"),
// and that stack name is also the prefix of every Nova server name. Heat list
// calls need {name}+{id}, so this GETs /stacks/{id} (Heat 302-redirects to the
// canonical /stacks/{name}/{id}, same host so the auth header is preserved) and
// reads stack_name. Cached for the run.
func (r *runner) resolveStackName(ctx context.Context, stackID string) (string, error) {
	if r.stackNameCache != "" {
		return r.stackNameCache, nil
	}
	orch, err := r.orchClient()
	if err != nil {
		return "", err
	}
	var resp struct {
		Stack struct {
			Name string `json:"stack_name"`
		} `json:"stack"`
	}
	if _, err := orch.Get(ctx, orch.ServiceURL("stacks", stackID), &resp, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		return "", err
	}
	if resp.Stack.Name == "" {
		return "", fmt.Errorf("stack %s has no stack_name", stackID)
	}
	r.stackNameCache = resp.Stack.Name
	return resp.Stack.Name, nil
}

// orchClient returns a lazily-built, cached Heat (orchestration v1) client.
func (r *runner) orchClient() (*gophercloud.ServiceClient, error) {
	if r.heat != nil {
		return r.heat, nil
	}
	c, err := openstack.NewOrchestrationV1(r.provider, gophercloud.EndpointOpts{Region: r.cfg.region})
	if err != nil {
		return nil, fmt.Errorf("locating Heat (orchestration) endpoint: %w", err)
	}
	r.heat = c
	return c, nil
}

// collectHeatDiagnostics is the no-SSH tier's window into a node-level failure:
// when Heat reports *_FAILED (e.g. a master/worker config SoftwareDeployment
// exited non-zero), the actual reconciler `run-once` stdout/stderr lives in the
// Heat SoftwareDeployment's output_values, NOT in the Kubernetes API. This walks
// the cluster's Heat stack, finds every FAILED resource, and for each failed
// SoftwareDeployment fetches deploy_stdout / deploy_stderr / deploy_status_code —
// the exact output the node's run-once produced. It writes a diagnostics file
// (always, even when the Kubernetes API is unreachable) so the CI artifact upload
// has the root-cause to show. Best-effort: never returns an error.
func (r *runner) collectHeatDiagnostics(ctx context.Context, reason string) {
	c, err := r.getCluster(ctx)
	if err != nil {
		r.err("heat-diag: cannot get cluster: %v", err)
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "==== e2e Heat diagnostics ====\nreason:   %s\nscenario: %s\ncluster:  %s\nstatus:   %s\ntime:     %s\n",
		reason, r.cfg.scenario, r.cfg.clusterName, c.Status, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	if c.StatusReason != "" {
		fmt.Fprintf(&b, "reason:   %s\n", c.StatusReason)
	}
	if len(c.Faults) > 0 {
		fmt.Fprintf(&b, "\n-- cluster faults --\n")
		for k, v := range c.Faults {
			fmt.Fprintf(&b, "  %s: %s\n", k, v)
		}
	}

	if c.StackID == "" {
		fmt.Fprintf(&b, "\n(no stack_id on cluster — cannot walk Heat resources)\n")
		r.writeDiagFile("heat-"+reason, b.String())
		return
	}

	orch, err := r.orchClient()
	if err != nil {
		fmt.Fprintf(&b, "\n(no Heat client: %v)\n", err)
		r.writeDiagFile("heat-"+reason, b.String())
		return
	}

	stackName, err := r.resolveStackName(ctx, c.StackID)
	if err != nil {
		fmt.Fprintf(&b, "\n(resolve stack name failed: %v)\n", err)
		r.writeDiagFile("heat-"+reason, b.String())
		return
	}
	// Depth 5: the master/worker config SoftwareDeployments live a few levels down
	// (cluster stack → kube_masters ResourceGroup → per-node kubemaster stack →
	// master_config_deployment), so a shallow list would miss the one that failed.
	pages, err := stackresources.List(orch, stackName, c.StackID, stackresources.ListOpts{Depth: 5}).AllPages(ctx)
	if err != nil {
		fmt.Fprintf(&b, "\n(list stack resources failed: %v)\n", err)
		r.writeDiagFile("heat-"+reason, b.String())
		return
	}
	res, err := stackresources.ExtractResources(pages)
	if err != nil {
		fmt.Fprintf(&b, "\n(extract stack resources failed: %v)\n", err)
		r.writeDiagFile("heat-"+reason, b.String())
		return
	}

	var failed []stackresources.Resource
	for _, rs := range res {
		if strings.Contains(rs.Status, "FAILED") {
			failed = append(failed, rs)
		}
	}
	fmt.Fprintf(&b, "\n-- FAILED Heat resources (%d of %d) --\n", len(failed), len(res))
	for _, rs := range failed {
		fmt.Fprintf(&b, "  %-34s %-40s %s\n      reason: %s\n",
			rs.Name, rs.Type, rs.Status, strings.TrimSpace(rs.StatusReason))
	}

	// For each failed SoftwareDeployment, fetch the node's run-once output.
	for _, rs := range failed {
		if rs.Type != "OS::Heat::SoftwareDeployment" || rs.PhysicalID == "" {
			continue
		}
		fmt.Fprintf(&b, "\n### SoftwareDeployment %s (%s)\n", rs.Name, rs.PhysicalID)
		stdout, stderr, code, derr := fetchDeployOutput(ctx, orch, rs.PhysicalID)
		if derr != nil {
			fmt.Fprintf(&b, "(fetch deploy output failed: %v)\n", derr)
			continue
		}
		fmt.Fprintf(&b, "deploy_status_code: %s\n", code)
		if s := strings.TrimSpace(stderr); s != "" {
			fmt.Fprintf(&b, "--- deploy_stderr (tail) ---\n%s\n", tail(s, 8000))
		}
		if s := strings.TrimSpace(stdout); s != "" {
			fmt.Fprintf(&b, "--- deploy_stdout (tail) ---\n%s\n", tail(s, 8000))
		}
	}

	// Also surface resources still IN_PROGRESS. When a step times out on
	// UPDATE_IN_PROGRESS with ZERO failed resources, this is the only way to see
	// what Heat is still waiting on. In particular, a SoftwareDeployment that is
	// still in progress but whose node already signalled success
	// (deploy_status_code 0) is the smoking gun for a Heat-side orchestration /
	// signal-transport wedge — the node reconcile finished and returned 0, yet
	// the parent stack never finalized — as opposed to a node fault (which shows
	// up as FAILED above).
	var inProgress []stackresources.Resource
	for _, rs := range res {
		if strings.Contains(rs.Status, "IN_PROGRESS") {
			inProgress = append(inProgress, rs)
		}
	}
	fmt.Fprintf(&b, "\n-- IN_PROGRESS Heat resources (%d of %d) --\n", len(inProgress), len(res))
	for _, rs := range inProgress {
		fmt.Fprintf(&b, "  %-34s %-40s %s\n      reason: %s\n",
			rs.Name, rs.Type, rs.Status, strings.TrimSpace(rs.StatusReason))
	}
	for _, rs := range inProgress {
		if rs.Type != "OS::Heat::SoftwareDeployment" || rs.PhysicalID == "" {
			continue
		}
		fmt.Fprintf(&b, "\n### (in-progress) SoftwareDeployment %s (%s)\n", rs.Name, rs.PhysicalID)
		stdout, stderr, code, derr := fetchDeployOutput(ctx, orch, rs.PhysicalID)
		if derr != nil {
			fmt.Fprintf(&b, "(fetch deploy output failed: %v)\n", derr)
			continue
		}
		fmt.Fprintf(&b, "deploy_status_code: %s  (0 here == node already succeeded; Heat has not finalized)\n", code)
		if s := strings.TrimSpace(stderr); s != "" {
			fmt.Fprintf(&b, "--- deploy_stderr (tail) ---\n%s\n", tail(s, 4000))
		}
		if s := strings.TrimSpace(stdout); s != "" {
			fmt.Fprintf(&b, "--- deploy_stdout (tail) ---\n%s\n", tail(s, 4000))
		}
	}

	r.writeDiagFile("heat-"+reason, b.String())
	r.log("heat-diag: %d failed, %d in-progress Heat resource(s) captured for %q", len(failed), len(inProgress), reason)
}

// fetchDeployOutput reads a Heat SoftwareDeployment's output_values directly
// (gophercloud has no software-deployments package, so this is a raw GET against
// the orchestration endpoint). deploy_stdout/deploy_stderr/deploy_status_code are
// the standard outputs every SoftwareDeployment carries — here, the reconciler's
// run-once result on the node.
func fetchDeployOutput(ctx context.Context, orch *gophercloud.ServiceClient, id string) (stdout, stderr, code string, err error) {
	var res struct {
		SoftwareDeployment struct {
			OutputValues map[string]any `json:"output_values"`
			StatusReason string         `json:"status_reason"`
		} `json:"software_deployment"`
	}
	url := orch.ServiceURL("software_deployments", id)
	if _, err = orch.Get(ctx, url, &res, &gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		return "", "", "", err
	}
	ov := res.SoftwareDeployment.OutputValues
	return asString(ov["deploy_stdout"]), asString(ov["deploy_stderr"]), asString(ov["deploy_status_code"]), nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// tail returns the last n characters of s (prefixed with an elision marker when
// truncated), so a huge deploy log does not blow up the diagnostics file.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…(truncated)…\n" + s[len(s)-n:]
}
