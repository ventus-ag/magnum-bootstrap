package services

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "services" }
func (Module) Dependencies() []string {
	return []string{"kube-master-config", "kube-worker-config", "proxy-env", "etcd"}
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

	serviceList := desiredServices(cfg)
	if cfg.Role() != config.RoleMaster {
		// Stop docker before re-enabling (flannel subnet pickup). Matches bash.
		if cfg.Shared.ContainerRuntime != "containerd" {
			result, err := (hostresource.SystemdServiceSpec{Unit: "docker", SkipIfMissing: true, Active: hostresource.BoolPtr(false)}).Apply(executor)
			if err != nil {
				return moduleapi.Result{}, err
			}
			changes = append(changes, result.Changes...)
		}
	}

	// Enable all services in parallel.
	{
		errs := make(chan error, len(serviceList))
		var wg sync.WaitGroup
		for _, svc := range serviceList {
			wg.Add(1)
			go func(svc string) {
				defer wg.Done()
				if _, err := (hostresource.SystemdServiceSpec{Unit: svc, Enabled: hostresource.BoolPtr(true)}).Apply(executor); err != nil {
					errs <- fmt.Errorf("enable %s: %w", svc, err)
				}
			}(svc)
		}
		wg.Wait()
		close(errs)
		if err, ok := <-errs; ok {
			return moduleapi.Result{}, err
		}
	}

	// Start/restart services in dependency tiers (parallel within each tier).
	// Master: [etcd, runtime] → [kube-apiserver] → [controller-manager, scheduler, kubelet, kube-proxy]
	// Worker: [runtime] → [kubelet, kube-proxy]
	var tiers [][]string
	if cfg.Role() == config.RoleMaster {
		tiers = [][]string{serviceList[:2], serviceList[2:3], serviceList[3:]}
	} else {
		tiers = [][]string{serviceList[:1], serviceList[1:]}
	}

	var mu sync.Mutex
	startOrRestart := func(svc string) error {
		needsRestart := req.Restarts != nil && req.Restarts.NeedsRestart(svc)
		isActive := executor.SystemctlIsActive(svc)
		reason := ""
		if req.Restarts != nil {
			if all := req.Restarts.All(); all[svc] != "" {
				reason = all[svc]
			}
		}

		switch {
		case needsRestart:
			result, err := (hostresource.SystemdServiceSpec{Unit: svc, Restart: true, RestartReason: reason}).Apply(executor)
			if err != nil {
				return fmt.Errorf("restart %s: %w", svc, err)
			}
			mu.Lock()
			changes = append(changes, result.Changes...)
			mu.Unlock()
			if req.Apply && !executor.WaitForSystemctlActive(svc, serviceReadyTimeout(svc), 2*time.Second) {
				return fmt.Errorf("service %s did not become active after restart", svc)
			}

		case !isActive:
			result, err := (hostresource.SystemdServiceSpec{Unit: svc, Active: hostresource.BoolPtr(true)}).Apply(executor)
			if err != nil {
				return fmt.Errorf("start %s: %w", svc, err)
			}
			mu.Lock()
			changes = append(changes, result.Changes...)
			mu.Unlock()
			if req.Apply && !executor.WaitForSystemctlActive(svc, serviceReadyTimeout(svc), 2*time.Second) {
				return fmt.Errorf("service %s did not become active after start", svc)
			}
		}
		return nil
	}

	for _, tier := range tiers {
		if len(tier) == 1 {
			if err := startOrRestart(tier[0]); err != nil {
				return moduleapi.Result{}, err
			}
			continue
		}
		errs := make(chan error, len(tier))
		var wg sync.WaitGroup
		for _, svc := range tier {
			wg.Add(1)
			go func(svc string) {
				defer wg.Done()
				if err := startOrRestart(svc); err != nil {
					errs <- err
				}
			}(svc)
		}
		wg.Wait()
		close(errs)
		if err, ok := <-errs; ok {
			return moduleapi.Result{}, err
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

	// Label master nodes with the appropriate role label(s).
	// K8s < 1.20:  only "master"
	// K8s 1.20-1.24: both "master" and "control-plane" (transition period)
	// K8s >= 1.25: only "control-plane" ("master" label removed upstream)
	if cfg.Role() == config.RoleMaster {
		kubeTag := cfg.Shared.KubeTag
		kubectl := "kubectl"
		kc := "--kubeconfig=/etc/kubernetes/admin.conf"

		if kubeletconfig.KubeMinorAtLeast(kubeTag, 25) {
			// 1.25+: control-plane only
			_ = executor.Run(kubectl, kc, "label", "node", cfg.Shared.InstanceName,
				"node-role.kubernetes.io/control-plane=", "--overwrite")
		} else if kubeletconfig.KubeMinorAtLeast(kubeTag, 20) {
			// 1.20-1.24: both labels for backward compatibility
			_ = executor.Run(kubectl, kc, "label", "node", cfg.Shared.InstanceName,
				"node-role.kubernetes.io/master=", "--overwrite")
			_ = executor.Run(kubectl, kc, "label", "node", cfg.Shared.InstanceName,
				"node-role.kubernetes.io/control-plane=", "--overwrite")
		} else {
			// < 1.20: master only
			_ = executor.Run(kubectl, kc, "label", "node", cfg.Shared.InstanceName,
				"node-role.kubernetes.io/master=", "--overwrite")
		}
	}

	// Record which labels/taints we applied for observability.
	nodeRole := "master"
	if kubeletconfig.KubeMinorAtLeast(cfg.Shared.KubeTag, 25) {
		nodeRole = "control-plane"
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"role":     cfg.Role().String(),
			"nodeRole": nodeRole,
		},
	}, nil
}

// Destroy stops all kubernetes services on the node.
func (Module) Destroy(_ context.Context, cfg config.Config, req moduleapi.Request) error {
	executor := host.NewExecutor(req.Apply, req.Logger)

	var services []string
	if cfg.Role() == config.RoleMaster {
		services = []string{"kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet", "kube-proxy"}
	} else {
		services = []string{"kubelet", "kube-proxy"}
	}

	var wg sync.WaitGroup
	for _, svc := range services {
		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			if req.Logger != nil {
				req.Logger.Infof("services destroy: stopping %s", svc)
			}
			_, _ = (hostresource.SystemdServiceSpec{Unit: svc, SkipIfMissing: true, Active: hostresource.BoolPtr(false), Enabled: hostresource.BoolPtr(false)}).Apply(executor)
		}(svc)
	}
	wg.Wait()
	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Services", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))
	for _, svc := range desiredServices(heat.Cfg) {
		if _, err := hostsdk.RegisterSystemdServiceSpec(ctx, name+"-"+svc, hostresource.SystemdServiceSpec{Unit: svc, Enabled: hostresource.BoolPtr(true), Active: hostresource.BoolPtr(true)}, childOpts...); err != nil {
			return nil, err
		}
	}
	nodeRole := "master"
	if kubeletconfig.KubeMinorAtLeast(heat.Cfg.Shared.KubeTag, 25) {
		nodeRole = "control-plane"
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"role":             pulumi.String(heat.Cfg.Role().String()),
		"containerRuntime": pulumi.String(heat.Cfg.Shared.ContainerRuntime),
		"nodeRole":         pulumi.String(nodeRole),
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

func desiredServices(cfg config.Config) []string {
	runtimeService := "docker"
	if cfg.Shared.ContainerRuntime == "containerd" {
		runtimeService = "containerd"
	}
	if cfg.Role() == config.RoleMaster {
		return []string{
			"etcd",
			runtimeService,
			"kube-apiserver",
			"kube-controller-manager",
			"kube-scheduler",
			"kubelet",
			"kube-proxy",
		}
	}
	return []string{runtimeService, "kubelet", "kube-proxy"}
}
