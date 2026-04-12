package clusterhealth

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "cluster-health" }
func (Module) Dependencies() []string {
	return []string{
		"cluster-flannel",
		"cluster-coredns",
		"cluster-occm",
		"cluster-cinder-csi",
		"cluster-manila-csi",
		"cluster-metrics-server",
		"cluster-dashboard",
		"cluster-auto-healer",
		"cluster-autoscaler",
	}
}

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	kubectl := "/srv/magnum/bin/kubectl"
	kubeconfig := "/etc/kubernetes/admin.conf"

	pods, err := allCrashLoopPods(executor, kubectl, kubeconfig)
	if err != nil {
		return moduleapi.Result{
			Warnings: []string{fmt.Sprintf("failed to scan for crashlooping pods: %v", err)},
		}, nil
	}

	var deleted []string
	var warnings []string

	for _, pod := range pods {
		if req.Apply {
			if err := executor.Run(kubectl, "--kubeconfig="+kubeconfig, "delete", "pod", pod.name, "-n", pod.namespace, "--wait=false"); err != nil {
				warnings = append(warnings, fmt.Sprintf("failed to delete pod %s/%s: %v", pod.namespace, pod.name, err))
				continue
			}
		}
		deleted = append(deleted, pod.namespace+"/"+pod.name)
		if req.Logger != nil {
			req.Logger.Infof("cluster-health: deleted crashlooping pod %s/%s", pod.namespace, pod.name)
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

type podRef struct {
	namespace string
	name      string
}

// allCrashLoopPods scans all namespaces for pods with crashlooping containers.
func allCrashLoopPods(executor *host.Executor, kubectl, kubeconfig string) ([]podRef, error) {
	out, err := executor.RunCapture(
		kubectl, "--kubeconfig="+kubeconfig,
		"get", "pods", "--all-namespaces",
		"--field-selector=status.phase!=Succeeded",
		"-o", `jsonpath={range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{range .status.containerStatuses[*]}{.state.waiting.reason}{"|"}{.lastState.terminated.reason}{"|"}{.restartCount}{" "}{end}{"\n"}{end}`,
	)
	if err != nil {
		return nil, err
	}

	var pods []podRef
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		ns := parts[0]
		name := parts[1]
		containers := parts[2]
		if isCrashLooping(containers) {
			pods = append(pods, podRef{namespace: ns, name: name})
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
		"scope": pulumi.String("all-namespaces"),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
