package services

import (
	"context"
	"fmt"
	"os"
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

func (Module) PhaseID() string { return "services" }
func (Module) Dependencies() []string {
	return []string{"kube-master-config", "kube-worker-config", "storage"}
}

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	if err := executor.Run("systemctl", "daemon-reload"); err != nil {
		return moduleapi.Result{}, err
	}

	// Wait for CA key if cert-manager API is enabled (controller-manager needs it).
	if cfg.Role() == config.RoleMaster && cfg.Shared.CertManagerAPI && req.Apply {
		caKeyPath := "/etc/kubernetes/certs/ca.key"
		for i := 0; i < 30; i++ {
			if _, err := os.Stat(caKeyPath); err == nil {
				break
			}
			if req.Logger != nil {
				req.Logger.Infof("waiting for CA key at %s for cert-manager API", caKeyPath)
			}
			time.Sleep(2 * time.Second)
		}
	}

	var serviceList []string
	if cfg.Role() == config.RoleMaster {
		runtimeService := "docker"
		if cfg.Shared.ContainerRuntime == "containerd" {
			runtimeService = "containerd"
		}
		serviceList = []string{
			"etcd",
			runtimeService,
			"kube-apiserver",
			"kube-controller-manager",
			"kube-scheduler",
			"kubelet",
			"kube-proxy",
		}
	} else {
		// Stop docker before re-enabling (flannel subnet pickup). Matches bash.
		if cfg.Shared.ContainerRuntime != "containerd" {
			_ = executor.Run("systemctl", "stop", "docker")
		}
		runtimeService := "docker"
		if cfg.Shared.ContainerRuntime == "containerd" {
			runtimeService = "containerd"
		}
		serviceList = []string{
			runtimeService,
			"kubelet",
			"kube-proxy",
		}
	}

	for _, svc := range serviceList {
		// Always enable so services come back after reboot.
		if err := executor.Run("systemctl", "enable", svc); err != nil {
			return moduleapi.Result{}, fmt.Errorf("enable %s: %w", svc, err)
		}

		needsRestart := req.Restarts != nil && req.Restarts.NeedsRestart(svc)
		isActive := executor.SystemctlIsActive(svc)

		switch {
		case needsRestart:
			// Config changed for this service — restart it.
			if err := executor.Run("systemctl", "restart", svc); err != nil {
				return moduleapi.Result{}, fmt.Errorf("restart %s: %w", svc, err)
			}
			reason := ""
			if req.Restarts != nil {
				if all := req.Restarts.All(); all[svc] != "" {
					reason = " (" + all[svc] + ")"
				}
			}
			changes = append(changes, host.Change{
				Action:  host.ActionRestart,
				Summary: fmt.Sprintf("restart %s%s", svc, reason),
			})
			if req.Apply && !executor.WaitForSystemctlActive(svc, serviceReadyTimeout(svc), 2*time.Second) {
				return moduleapi.Result{}, fmt.Errorf("service %s did not become active after restart", svc)
			}

		case !isActive:
			// Service not running — start it.
			if err := executor.Run("systemctl", "start", svc); err != nil {
				return moduleapi.Result{}, fmt.Errorf("start %s: %w", svc, err)
			}
			changes = append(changes, host.Change{
				Action:  host.ActionCreate,
				Summary: fmt.Sprintf("start %s", svc),
			})
			if req.Apply && !executor.WaitForSystemctlActive(svc, serviceReadyTimeout(svc), 2*time.Second) {
				return moduleapi.Result{}, fmt.Errorf("service %s did not become active after start", svc)
			}

		default:
			// Already running and no changes — skip.
		}
	}

	// Wait for API server to be functionally healthy before labeling.
	// SystemctlIsActive only checks the process — the API may not be
	// serving yet (etcd quorum forming, post-start hooks running).
	if cfg.Role() == config.RoleMaster && req.Apply {
		for i := 0; i < 60; i++ {
			err := executor.Run("kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "get", "--raw=/healthz")
			if err == nil {
				break
			}
			time.Sleep(5 * time.Second)
		}
	}

	// Label control-plane node if needed.
	if cfg.Role() == config.RoleMaster && cfg.Shared.LeadNodeRoleName == "control-plane" {
		_ = executor.Run("kubectl",
			"--kubeconfig=/etc/kubernetes/admin.conf",
			"patch", "node", cfg.Shared.InstanceName,
			"--patch", `{"metadata": {"labels": {"node-role.kubernetes.io/control-plane": ""}}}`,
		)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"role": cfg.Role().String()},
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Services", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"role":             pulumi.String(heat.Cfg.Role().String()),
		"containerRuntime": pulumi.String(heat.Cfg.Shared.ContainerRuntime),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func serviceReadyTimeout(service string) time.Duration {
	switch service {
	case "etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet":
		return 60 * time.Second
	default:
		return 30 * time.Second
	}
}
