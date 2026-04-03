package containerruntime

import (
	"context"
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "container-runtime" }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if cfg.IsPureCARotation() {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	if cfg.Shared.ContainerRuntime == "containerd" {
		cs, err := reconcileContainerd(ctx, cfg, executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
	} else {
		cs, err := reconcileDocker(cfg, executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
	}

	// Signal restart for the container runtime only if config changed.
	if len(changes) > 0 && req.Restarts != nil {
		if cfg.Shared.ContainerRuntime == "containerd" {
			req.Restarts.Add("containerd", "container-runtime config changed")
		} else {
			req.Restarts.Add("docker", "container-runtime config changed")
		}
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"containerRuntime": cfg.Shared.ContainerRuntime,
			"cgroupDriver":     cfg.Shared.CgroupDriver,
		},
	}, nil
}

func reconcileContainerd(ctx context.Context, cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change
	configChanged := false

	// Only stop/disable docker if it's actually running and we want containerd.
	if executor.SystemctlIsActive("docker") {
		_ = executor.Run("systemctl", "stop", "docker")
		_ = executor.Run("systemctl", "disable", "docker")
		changes = append(changes, host.Change{Action: host.ActionOther, Summary: "disable docker in favour of containerd"})
	}

	// Determine tarball URL.
	tarballURL := cfg.Shared.ContainerdTarballURL
	if tarballURL == "" && cfg.Shared.ContainerdVersion != "" {
		tarballURL = fmt.Sprintf(
			"https://github.com/containerd/containerd/releases/download/v%s/cri-containerd-cni-%s-linux-amd64.tar.gz",
			cfg.Shared.ContainerdVersion, cfg.Shared.ContainerdVersion,
		)
	}

	if tarballURL != "" {
		dl, err := executor.DownloadFileWithRetry(ctx, tarballURL, "/srv/magnum/cri-containerd-cni.tar.gz", 0o644, 5)
		if err != nil {
			return nil, fmt.Errorf("download containerd tarball: %w", err)
		}
		if dl.Change != nil {
			changes = append(changes, *dl.Change)
		}
		if dl.Changed && executor.Apply {
			if err := executor.Run("tar", "xzf", "/srv/magnum/cri-containerd-cni.tar.gz",
				"-C", "/",
				"--no-same-owner", "--touch", "--no-same-permissions",
				"--exclude=etc/cni/net.d",
				"--exclude=etc/containerd/config.toml",
				"--exclude=opt/cni/bin",
				"--exclude=*.txt",
				"--exclude=opt/containerd/cluster/gce",
			); err != nil {
				return nil, fmt.Errorf("extract containerd tarball: %w", err)
			}
			configChanged = true
		}
	}

	// Ensure directories.
	for _, dir := range []string{"/etc/containerd", "/opt/cni/bin"} {
		change, err := executor.EnsureDir(dir, 0o755)
		if err != nil {
			return nil, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Write containerd config.toml — EnsureFile is idempotent.
	change, err := executor.EnsureFile("/etc/containerd/config.toml", []byte(containerdConfig()), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
		configChanged = true
	}

	// Only daemon-reload if config files changed.
	if configChanged {
		_ = executor.Run("systemctl", "daemon-reload")
	}

	// Enable is idempotent — always safe to run.
	_ = executor.Run("systemctl", "enable", "containerd")

	// Detect drift: if containerd should be running but isn't, start it.
	if !executor.SystemctlIsActive("containerd") {
		if err := executor.Run("systemctl", "start", "containerd"); err != nil {
			return nil, err
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: "start containerd (was not running)"})
	}

	return changes, nil
}

func reconcileDocker(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	cgroupDriver := cfg.Shared.CgroupDriver
	if cgroupDriver == "" {
		cgroupDriver = "systemd"
	}

	dropinDir := "/etc/systemd/system/docker.service.d"
	change, err := executor.EnsureDir(dropinDir, 0o755)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	content := fmt.Sprintf("[Service]\nExecStart=\nExecStart=/usr/bin/dockerd --exec-opt native.cgroupdriver=%s\n", cgroupDriver)
	change, err = executor.EnsureFile(dropinDir+"/cgroupdriver.conf", []byte(content), 0o644)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
		_ = executor.Run("systemctl", "daemon-reload")
	}

	_ = executor.Run("systemctl", "enable", "docker")

	// Detect drift: if docker should be running but isn't, start it.
	if !executor.SystemctlIsActive("docker") {
		if err := executor.Run("systemctl", "start", "docker"); err != nil {
			return nil, err
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: "start docker (was not running)"})
	}

	return changes, nil
}

func containerdConfig() string {
	return `version = 2
root = "/var/lib/containerd"
state = "/run/containerd"
oom_score = 0

[grpc]
  address = "/run/containerd/containerd.sock"
  max_recv_message_size = 16777216
  max_send_message_size = 16777216

[debug]
  level = "info"

[metrics]
  address = ""
  grpc_histogram = false

[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    sandbox_image = "registry.k8s.io/pause:3.9"
    max_container_log_line_size = 16384
    enable_unprivileged_ports = true
    enable_unprivileged_icmp = true
    [plugins."io.containerd.grpc.v1.cri".cni]
      bin_dir = "/opt/cni/bin"
      conf_dir = "/etc/cni/net.d"
    [plugins."io.containerd.grpc.v1.cri".containerd]
      default_runtime_name = "runc"
      snapshotter = "overlayfs"
    [plugins."io.containerd.grpc.v1.cri".registry]
      [plugins."io.containerd.grpc.v1.cri".registry.mirrors]
        [plugins."io.containerd.grpc.v1.cri".registry.mirrors."docker.io"]
          endpoint = ["https://registry-1.docker.io"]
  [plugins."io.containerd.internal.v1.opt"]
    path = "/var/lib/containerd/opt"
`
}

// fileExists is a small helper that avoids importing os in the caller.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:ContainerRuntime", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"containerRuntime":  pulumi.String(cfg.Shared.ContainerRuntime),
		"containerdVersion": pulumi.String(cfg.Shared.ContainerdVersion),
		"cgroupDriver":      pulumi.String(cfg.Shared.CgroupDriver),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
