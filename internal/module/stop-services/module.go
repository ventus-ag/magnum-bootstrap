package stopservices

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

const (
	drainTimeoutArg               = "--timeout=10m"
	drainSkipWaitDeleteTimeoutArg = "--skip-wait-for-delete-timeout=60"
	maxDrainBlockersToReport      = 8
)

type drainBlocker struct {
	Namespace         string
	Name              string
	Phase             string
	DeletionTimestamp string
	OwnerKind         string
	Finalizers        string
}

func (Module) PhaseID() string        { return "stop-services" }
func (Module) Dependencies() []string { return []string{"admin-kubeconfig", "client-tools"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !moduleapi.DisruptiveServiceCycleNeeded(cfg, req) {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	kubeconfig := "/etc/kubernetes/admin.conf"
	kubectl := "/srv/magnum/bin/kubectl"

	if req.Apply {
		summary, err := reconcileDrainState(cfg, executor, kubectl, kubeconfig)
		if err != nil {
			return moduleapi.Result{}, err
		}
		if summary != "" {
			changes = append(changes, host.Change{Action: host.ActionOther, Summary: summary})
		}
	} else {
		changes = append(changes, host.Change{Action: host.ActionOther, Summary: fmt.Sprintf("drain/cordon node %s (planned)", cfg.Shared.InstanceName)})
	}

	if cfg.Shared.UsePodman {
		// List running kube services, stop them, remove containers and images.
		serviceList, _ := executor.RunCapture("podman", "ps", "-f", "name=kube", "--format", "{{.Names}}")
		for _, svc := range strings.Fields(serviceList) {
			if executor.SystemctlIsActive(svc) {
				stopResult, _ := (hostresource.SystemdServiceSpec{Unit: svc, SkipIfMissing: true, Active: hostresource.BoolPtr(false)}).Apply(executor)
				// Remove container and image so podman pulls fresh on restart.
				containerID, _ := executor.RunCapture("podman", "ps", "--filter", "name="+svc, "-a", "-q")
				if containerID != "" {
					_ = executor.Run("podman", "rm", containerID)
				}
				imageID, _ := executor.RunCapture("podman", "images", "--filter", "reference=*"+svc+"*", "-a", "-q")
				if imageID != "" {
					_ = executor.Run("podman", "rmi", imageID)
				}
				changes = append(changes, stopResult.Changes...)
				changes = append(changes, host.Change{Action: host.ActionOther, Summary: fmt.Sprintf("clean %s container/image", svc)})
			}
		}

		if executor.SystemctlIsActive("kubelet") {
			stopResult, _ := (hostresource.SystemdServiceSpec{Unit: "kubelet", SkipIfMissing: true, Active: hostresource.BoolPtr(false)}).Apply(executor)
			changes = append(changes, stopResult.Changes...)
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"operation": cfg.Operation().String()},
	}, nil
}

func reconcileDrainState(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string) (string, error) {
	if shouldDrain(cfg, executor, kubectl, kubeconfig) {
		if err := drainNode(cfg.Shared.InstanceName, executor, kubectl, kubeconfig); err != nil {
			return "", err
		}
		return fmt.Sprintf("drain node %s", cfg.Shared.InstanceName), nil
	}

	if _, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig, "cordon", cfg.Shared.InstanceName); err != nil {
		return "", fmt.Errorf("cordon node %s: %w", cfg.Shared.InstanceName, err)
	}
	return fmt.Sprintf("cordon node %s", cfg.Shared.InstanceName), nil
}

func drainNode(nodeName string, executor *host.Executor, kubectl, kubeconfig string) error {
	_, err := executor.RunCapture(kubectl,
		"--kubeconfig="+kubeconfig,
		"drain", nodeName,
		"--ignore-daemonsets",
		"--delete-emptydir-data",
		"--force",
		drainTimeoutArg,
		drainSkipWaitDeleteTimeoutArg,
	)
	if err == nil {
		return nil
	}

	blockers, blockersErr := listDrainBlockers(nodeName, executor, kubectl, kubeconfig)
	if blockersErr != nil {
		return fmt.Errorf("drain node %s: %w; failed to inspect remaining pods: %v", nodeName, err, blockersErr)
	}
	if summary := summarizeDrainBlockers(blockers); summary != "" {
		return fmt.Errorf("drain node %s: %w; remaining pods: %s", nodeName, err, summary)
	}
	return fmt.Errorf("drain node %s: %w", nodeName, err)
}

func listDrainBlockers(nodeName string, executor *host.Executor, kubectl, kubeconfig string) ([]drainBlocker, error) {
	out, err := executor.RunCapture(kubectl,
		"--kubeconfig="+kubeconfig,
		"get", "pods",
		"-A",
		"--field-selector=spec.nodeName="+nodeName,
		"-o", `jsonpath={range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"|"}{.status.phase}{"|"}{.metadata.deletionTimestamp}{"|"}{.metadata.ownerReferences[0].kind}{"|"}{.metadata.finalizers[*]}{"\n"}{end}`,
	)
	if err != nil {
		return nil, err
	}
	return parseDrainBlockers(out), nil
}

func parseDrainBlockers(output string) []drainBlocker {
	var blockers []drainBlocker
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "|", 6)
		for len(parts) < 6 {
			parts = append(parts, "")
		}

		blocker := drainBlocker{
			Namespace:         strings.TrimSpace(parts[0]),
			Name:              strings.TrimSpace(parts[1]),
			Phase:             strings.TrimSpace(parts[2]),
			DeletionTimestamp: strings.TrimSpace(parts[3]),
			OwnerKind:         strings.TrimSpace(parts[4]),
			Finalizers:        strings.TrimSpace(parts[5]),
		}
		if blocker.Phase == "Succeeded" || blocker.Phase == "Failed" {
			continue
		}
		if blocker.OwnerKind == "DaemonSet" {
			continue
		}
		if blocker.Phase == "" {
			blocker.Phase = "Unknown"
		}
		if blocker.OwnerKind == "" {
			blocker.OwnerKind = "Pod"
		}
		blockers = append(blockers, blocker)
	}
	return blockers
}

func summarizeDrainBlockers(blockers []drainBlocker) string {
	if len(blockers) == 0 {
		return ""
	}

	parts := make([]string, 0, min(len(blockers), maxDrainBlockersToReport))
	for i, blocker := range blockers {
		if i >= maxDrainBlockersToReport {
			break
		}
		desc := fmt.Sprintf("%s/%s(owner=%s,phase=%s", blocker.Namespace, blocker.Name, blocker.OwnerKind, blocker.Phase)
		if blocker.DeletionTimestamp != "" {
			desc += ",deleting=" + blocker.DeletionTimestamp
		}
		if blocker.Finalizers != "" {
			desc += ",finalizers=" + blocker.Finalizers
		}
		desc += ")"
		parts = append(parts, desc)
	}

	summary := strings.Join(parts, "; ")
	if len(blockers) > maxDrainBlockersToReport {
		summary += fmt.Sprintf(" (+%d more)", len(blockers)-maxDrainBlockersToReport)
	}
	return summary
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// shouldDrain returns true if the node should be drained (not just cordoned).
// Single-master or single-worker clusters should only be cordoned.
func shouldDrain(cfg config.Config, executor *host.Executor, kubectl, kubeconfig string) bool {
	if cfg.Role() == config.RoleMaster {
		out, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig,
			"get", "nodes", "--selector=magnum.openstack.org/role=master", "-o", "name")
		if err == nil {
			nodes := strings.Fields(out)
			if len(nodes) <= 1 {
				return false
			}
		}
	} else {
		out, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig,
			"get", "nodes", "--selector=magnum.openstack.org/role!=master", "-o", "name")
		if err == nil {
			nodes := strings.Fields(out)
			if len(nodes) <= 1 {
				return false
			}
		}
	}
	return true
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:StopServices", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"operation": pulumi.String(heat.Cfg.Operation().String()),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
