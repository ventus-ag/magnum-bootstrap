package clusterhealth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string      { return "cluster-health" }
func (Module) Dependencies() []string { return []string{"cluster-autoscaler"} }

// namespacesToCheck lists the namespaces scanned for crashlooping pods.
var namespacesToCheck = []string{"kube-system", "kube-flannel"}

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	kubectl := "/srv/magnum/bin/kubectl"
	kubeconfig := "/etc/kubernetes/admin.conf"

	var deleted []string
	var warnings []string

	for _, ns := range namespacesToCheck {
		pods, err := crashLoopPods(executor, kubectl, kubeconfig, ns)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to list pods in %s: %v", ns, err))
			continue
		}
		for _, pod := range pods {
			if req.Apply {
				if err := executor.Run(kubectl, "--kubeconfig="+kubeconfig, "delete", "pod", pod, "-n", ns); err != nil {
					warnings = append(warnings, fmt.Sprintf("failed to delete pod %s/%s: %v", ns, pod, err))
					continue
				}
			}
			deleted = append(deleted, ns+"/"+pod)
			if req.Logger != nil {
				req.Logger.Infof("cluster-health: deleted crashlooping pod %s/%s", ns, pod)
			}
		}
	}

	// Brief wait for deleted pods to be recreated, then verify no crashloops remain.
	if req.Apply && len(deleted) > 0 {
		time.Sleep(15 * time.Second)
		for _, ns := range namespacesToCheck {
			remaining, _ := crashLoopPods(executor, kubectl, kubeconfig, ns)
			for _, pod := range remaining {
				warnings = append(warnings, fmt.Sprintf("pod %s/%s still crashlooping after restart", ns, pod))
			}
		}
	}

	res := moduleapi.Result{
		Warnings: warnings,
		Outputs:  map[string]string{"deletedPods": strings.Join(deleted, ",")},
	}
	if len(deleted) > 0 {
		res.Changes = []host.Change{{
			Action:  host.ActionRestart,
			Summary: fmt.Sprintf("deleted %d crashlooping pod(s): %s", len(deleted), strings.Join(deleted, ", ")),
		}}
	}
	return res, nil
}

// crashLoopPods returns pod names that have crashlooping containers within the
// given namespace.  It detects both pods currently in CrashLoopBackOff/Error
// waiting state AND pods that are momentarily Running between crash cycles
// (high restart count with a recent terminated error).
func crashLoopPods(executor *host.Executor, kubectl, kubeconfig, namespace string) ([]string, error) {
	// For each container we collect: waitingReason, lastTerminatedReason, restartCount.
	out, err := executor.RunCapture(
		kubectl, "--kubeconfig="+kubeconfig,
		"get", "pods", "-n", namespace,
		"--field-selector=status.phase!=Succeeded",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{range .status.containerStatuses[*]}{.state.waiting.reason}{"|"}{.lastState.terminated.reason}{"|"}{.restartCount}{" "}{end}{"\n"}{end}`,
	)
	if err != nil {
		return nil, err
	}

	var pods []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		podName := parts[0]
		containers := parts[1]
		if isCrashLooping(containers) {
			pods = append(pods, podName)
		}
	}
	return pods, nil
}

// isCrashLooping checks per-container status fields to detect crashlooping.
// Each container block is "waitingReason|lastTerminatedReason|restartCount ".
func isCrashLooping(containers string) bool {
	for _, c := range strings.Fields(containers) {
		fields := strings.SplitN(c, "|", 3)
		if len(fields) != 3 {
			continue
		}
		waitReason := fields[0]
		lastTermReason := fields[1]
		restartCount := fields[2]

		// Currently waiting in CrashLoopBackOff or Error.
		if waitReason == "CrashLoopBackOff" || waitReason == "Error" {
			return true
		}
		// Momentarily Running between crash cycles: last termination was an
		// error and the container has restarted multiple times.
		if (lastTermReason == "Error" || lastTermReason == "OOMKilled") && restartCountAbove(restartCount, 2) {
			return true
		}
	}
	return false
}

func restartCountAbove(s string, threshold int) bool {
	n := 0
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		} else {
			return false
		}
	}
	return n > threshold
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() {
		type empty struct{ pulumi.ResourceState }
		res := &empty{}
		if err := ctx.RegisterComponentResource("magnum:cluster:ClusterHealth", name, res, opts...); err != nil {
			return nil, err
		}
		if err := ctx.RegisterResourceOutputs(res, pulumi.Map{"skipped": pulumi.Bool(true)}); err != nil {
			return nil, err
		}
		return res, nil
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:ClusterHealth", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"namespaces": pulumi.String(strings.Join(namespacesToCheck, ",")),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
