package stopservices

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

func (Module) PhaseID() string { return "stop-services" }
func (Module) Dependencies() []string { return []string{"admin-kubeconfig"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	// Drain only when KUBE_TAG actually changed — not just because IS_UPGRADE
	// is true (it stays true permanently and never resets in heat-params).
	// On a fresh node (no previous state), PreviousKubeTag is empty so we
	// skip drain (nothing to upgrade from).
	kubeTagChanged := req.PreviousKubeTag != "" && req.PreviousKubeTag != cfg.Shared.KubeTag
	if !kubeTagChanged {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	kubeconfig := "/etc/kubernetes/admin.conf"
	kubectl := "/srv/magnum/bin/kubectl"

	// Check if already cordoned/drained before acting.
	if req.Apply {
		out, _ := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig,
			"get", "node", cfg.Shared.InstanceName, "-o", "jsonpath={.spec.unschedulable}")
		alreadyCordoned := out == "true"

		if !alreadyCordoned {
			if shouldDrain(cfg, executor, kubectl, kubeconfig) {
				_ = executor.Run(kubectl, "--kubeconfig="+kubeconfig,
					"drain", cfg.Shared.InstanceName,
					"--ignore-daemonsets", "--delete-emptydir-data", "--force")
			} else {
				_ = executor.Run(kubectl, "--kubeconfig="+kubeconfig,
					"cordon", cfg.Shared.InstanceName)
			}
			changes = append(changes, host.Change{Action: host.ActionOther, Summary: fmt.Sprintf("drain/cordon node %s", cfg.Shared.InstanceName)})
		}
	} else {
		changes = append(changes, host.Change{Action: host.ActionOther, Summary: fmt.Sprintf("drain/cordon node %s (planned)", cfg.Shared.InstanceName)})
	}

	if cfg.Shared.UsePodman {
		// List running kube services, stop them, remove containers and images.
		serviceList, _ := executor.RunCapture("podman", "ps", "-f", "name=kube", "--format", "{{.Names}}")
		for _, svc := range strings.Fields(serviceList) {
			if executor.SystemctlIsActive(svc) {
				_ = executor.Run("systemctl", "stop", svc)
				// Remove container and image so podman pulls fresh on restart.
				containerID, _ := executor.RunCapture("podman", "ps", "--filter", "name="+svc, "-a", "-q")
				if containerID != "" {
					_ = executor.Run("podman", "rm", containerID)
				}
				imageID, _ := executor.RunCapture("podman", "images", "--filter", "reference=*"+svc+"*", "-a", "-q")
				if imageID != "" {
					_ = executor.Run("podman", "rmi", imageID)
				}
				changes = append(changes, host.Change{Action: host.ActionOther, Summary: fmt.Sprintf("stop and clean %s", svc)})
			}
		}

		if executor.SystemctlIsActive("kubelet") {
			_ = executor.Run("systemctl", "stop", "kubelet")
			changes = append(changes, host.Change{Action: host.ActionOther, Summary: "stop kubelet"})
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"operation": cfg.Operation().String()},
	}, nil
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
