package health

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/internal/paths"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState

	Checks pulumi.StringArrayOutput `pulumi:"checks"`
}

func (Module) PhaseID() string {
	return "health"
}
func (Module) Dependencies() []string { return []string{"heat-container-agent", "proxy-env"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	checks := []string{
		"role=" + cfg.Role().String(),
		"operation=" + cfg.Operation().String(),
		"instance=" + cfg.Shared.InstanceName,
	}

	var failures []string

	for _, path := range requiredPaths(cfg, req) {
		if _, err := os.Stat(path); err == nil {
			checks = append(checks, "exists="+path)
		} else {
			failures = append(failures, "missing required path "+path)
		}
	}

	for _, service := range requiredServices(cfg) {
		if executor.SystemctlIsEnabled(service) {
			checks = append(checks, "enabled="+service)
		} else {
			failures = append(failures, "service not enabled "+service)
		}
		// Retry active check: services may still be stabilising after a
		// fresh create or CA rotation (e.g. kube-apiserver crash-looping
		// while etcd quorum forms across multiple masters).
		active := executor.SystemctlIsActive(service)
		if !active && req.Apply {
			for i := 0; i < 30; i++ {
				time.Sleep(2 * time.Second)
				if executor.SystemctlIsActive(service) {
					active = true
					break
				}
			}
		}
		if active {
			checks = append(checks, "active="+service)
		} else {
			failures = append(failures, "service not active "+service)
		}
	}

	for _, mount := range requiredMounts(cfg) {
		if executor.IsMountpoint(mount) {
			checks = append(checks, "mounted="+mount)
		} else {
			failures = append(failures, "required mount missing "+mount)
		}
	}

	if cfg.Role() == config.RoleMaster {
		if err := verifyMasterAPI(executor, cfg); err != nil {
			failures = append(failures, err.Error())
		} else {
			checks = append(checks, "apiserver=ready", "node-registered="+cfg.Shared.InstanceName)
		}
	}

	res := moduleapi.Result{
		Outputs: map[string]string{
			"checks": strings.Join(checks, ","),
		},
	}
	if len(failures) > 0 {
		res.Warnings = failures
		if !req.Apply {
			return res, nil
		}
		return res, fmt.Errorf("health checks failed: %s", strings.Join(failures, "; "))
	}

	return res, nil
}

func requiredPaths(cfg config.Config, req moduleapi.Request) []string {
	paths := []string{"/etc/kubernetes", "/usr/local/bin/kubelet", "/usr/local/bin/kubectl", "/srv/magnum/bin/kubectl"}
	if req.Paths.HeatParamsFile != "" {
		paths = append(paths, req.Paths.HeatParamsFile)
	}

	switch cfg.Role() {
	case config.RoleMaster:
		paths = append(paths,
			"/etc/kubernetes/admin.conf",
			"/etc/kubernetes/kubelet.conf",
			"/etc/kubernetes/kubelet-config.yaml",
			"/etc/kubernetes/controller-kubeconfig.yaml",
			"/etc/kubernetes/scheduler-kubeconfig.yaml",
			"/etc/kubernetes/proxy-kubeconfig.yaml",
			"/etc/kubernetes/apiserver",
			"/etc/kubernetes/controller-manager",
			"/etc/kubernetes/scheduler",
			"/etc/kubernetes/proxy",
		)
		if cfg.Shared.UsePodman {
			paths = append(paths,
				"/etc/systemd/system/etcd.service",
				"/etc/systemd/system/kubelet.service",
				"/etc/systemd/system/kube-apiserver.service",
				"/etc/systemd/system/kube-controller-manager.service",
				"/etc/systemd/system/kube-scheduler.service",
				"/etc/systemd/system/kube-proxy.service",
			)
		}
	case config.RoleWorker:
		paths = append(paths,
			"/etc/kubernetes/kubelet.conf",
			"/etc/kubernetes/kubelet-config.yaml",
			"/etc/kubernetes/proxy-config.yaml",
			"/etc/kubernetes/proxy",
			"/etc/kubernetes/config",
		)
		if cfg.Shared.UsePodman {
			paths = append(paths,
				"/etc/systemd/system/kubelet.service",
				"/etc/systemd/system/kube-proxy.service",
			)
		}
	}

	if !cfg.Shared.TLSDisabled {
		paths = append(paths, requiredCertPaths(cfg.Role())...)
	}

	return paths
}

func requiredServices(cfg config.Config) []string {
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
	return []string{
		runtimeService,
		"kubelet",
		"kube-proxy",
	}
}

func requiredCertPaths(role config.Role) []string {
	paths := []string{"/etc/kubernetes/certs/ca.crt", "/etc/kubernetes/certs/kubelet.crt", "/etc/kubernetes/certs/kubelet.key", "/etc/kubernetes/certs/proxy.crt", "/etc/kubernetes/certs/proxy.key"}
	if role == config.RoleMaster {
		paths = append(paths,
			"/etc/kubernetes/certs/server.crt",
			"/etc/kubernetes/certs/server.key",
			"/etc/kubernetes/certs/admin.crt",
			"/etc/kubernetes/certs/admin.key",
		)
	}
	return paths
}

func requiredMounts(cfg config.Config) []string {
	var mounts []string
	if cfg.Shared.DockerVolumeSize > 0 {
		if cfg.Shared.ContainerRuntime == "containerd" {
			mounts = append(mounts, "/var/lib/containerd")
		} else {
			mounts = append(mounts, "/var/lib/docker")
		}
	}
	if cfg.Role() == config.RoleMaster && cfg.Master != nil && cfg.Master.EtcdVolumeSize > 0 {
		mounts = append(mounts, "/var/lib/etcd")
	}
	return mounts
}

func verifyMasterAPI(executor *host.Executor, cfg config.Config) error {
	kubectl := "/srv/magnum/bin/kubectl"
	kubeconfig := "/etc/kubernetes/admin.conf"

	var lastErr error
	attempts := 30
	if !executor.Apply {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if _, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig, "get", "--raw=/readyz"); err != nil {
			lastErr = fmt.Errorf("apiserver not ready via %s: %w", kubeconfig, err)
		} else if _, err := executor.RunCapture(kubectl, "--kubeconfig="+kubeconfig, "get", "node", cfg.Shared.InstanceName, "-o", "name"); err != nil {
			lastErr = fmt.Errorf("node %s not registered: %w", cfg.Shared.InstanceName, err)
		} else {
			return nil
		}
		if attempt+1 < attempts {
			time.Sleep(2 * time.Second)
		}
	}
	return lastErr
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Health", name, res, opts...); err != nil {
		return nil, err
	}
	healthReq := moduleapi.Request{Paths: paths.Paths{HeatParamsFile: "/etc/sysconfig/heat-params"}}
	pathsList := requiredPaths(cfg, healthReq)
	requiredPathsArray := make(pulumi.StringArray, 0, len(pathsList))
	for _, path := range pathsList {
		requiredPathsArray = append(requiredPathsArray, pulumi.String(path))
	}
	requiredServicesArray := make(pulumi.StringArray, 0, len(requiredServices(cfg)))
	for _, service := range requiredServices(cfg) {
		requiredServicesArray = append(requiredServicesArray, pulumi.String(service))
	}
	requiredMountsArray := make(pulumi.StringArray, 0, len(requiredMounts(cfg)))
	for _, mount := range requiredMounts(cfg) {
		requiredMountsArray = append(requiredMountsArray, pulumi.String(mount))
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"checks": pulumi.StringArray{
			pulumi.String("role=" + cfg.Role().String()),
			pulumi.String("operation=" + cfg.Operation().String()),
			pulumi.String("instance=" + cfg.Shared.InstanceName),
		},
		"requiredPaths":     requiredPathsArray,
		"requiredServices":  requiredServicesArray,
		"requiredMounts":    requiredMountsArray,
		"verifiesMasterAPI": pulumi.Bool(cfg.Role() == config.RoleMaster),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
