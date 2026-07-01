package clusterhealth

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "cluster-health" }
func (Module) Dependencies() []string {
	return monitoredAddonPhases()
}

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	kubectl := "/srv/magnum/bin/kubectl"
	kubeconfig := "/etc/kubernetes/admin.conf"

	pods, warnScan := managedCrashLoopPods(executor, kubectl, kubeconfig, req.Logger)

	var deleted []string
	warnings := warnScan

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

// managedNamespaces are the namespaces the cluster addon modules deploy into.
// The healing scan is limited to them: this module manages addon health, and a
// cluster-wide sweep would delete users' crashlooping pods — a bare debug pod
// would be gone permanently, and controller pods would be delete-churned in a
// way that resets CrashLoopBackOff and hides the failure from their owner.
func managedNamespaces() []string {
	return []string{"kube-system", "kube-flannel", "kubernetes-dashboard", "gpu-operator"}
}

// managedCrashLoopPods scans the managed addon namespaces for crashlooping
// pods that are safe to heal by deletion. Scan errors are reported as
// warnings, never as failures.
func managedCrashLoopPods(executor *host.Executor, kubectl, kubeconfig string, logger *logging.Logger) ([]podRef, []string) {
	var pods []podRef
	var warnings []string
	for _, ns := range managedNamespaces() {
		out, err := executor.RunCapture(
			kubectl, "--kubeconfig="+kubeconfig,
			"get", "pods", "-n", ns,
			"--field-selector=status.phase!=Succeeded",
			"-o", `jsonpath={range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.ownerReferences[*].kind}{"\t"}{range .status.containerStatuses[*]}{.state.waiting.reason}{"|"}{.lastState.terminated.reason}{"|"}{.restartCount}{" "}{end}{"\n"}{end}`,
		)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to scan namespace %s for crashlooping pods: %v", ns, err))
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			parts := strings.SplitN(line, "\t", 4)
			if len(parts) != 4 {
				continue
			}
			namespace := parts[0]
			name := parts[1]
			ownerKinds := strings.TrimSpace(parts[2])
			containers := parts[3]
			if !isCrashLooping(containers) {
				continue
			}
			// Only controller-owned pods are recreated after deletion. A pod
			// with no owner reference would just be gone — leave it alone.
			if ownerKinds == "" {
				if logger != nil {
					logger.Infof("cluster-health: skipping crashlooping pod %s/%s (no controller owner, deletion would not recreate it)", namespace, name)
				}
				continue
			}
			pods = append(pods, podRef{namespace: namespace, name: name})
		}
	}
	return pods, warnings
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
	addonPhases := make(pulumi.StringArray, 0, len(monitoredAddonPhases()))
	for _, phase := range monitoredAddonPhases() {
		addonPhases = append(addonPhases, pulumi.String(phase))
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"scope":              pulumi.String("all-namespaces"),
		"monitoredAddons":    addonPhases,
		"detectsCrashLoops":  pulumi.Bool(true),
		"deletesPodsOnApply": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func monitoredAddonPhases() []string {
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
