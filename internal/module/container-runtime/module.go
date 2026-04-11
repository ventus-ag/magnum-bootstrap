package containerruntime

import (
	"context"
	"fmt"
	"os"
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

func (Module) PhaseID() string { return "container-runtime" }
func (Module) Dependencies() []string { return []string{"prereq-validation"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	configChanged := false
	if cfg.Shared.ContainerRuntime == "containerd" {
		cs, changed, err := reconcileContainerd(ctx, cfg, executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
		configChanged = changed
	} else {
		cs, err := reconcileDocker(cfg, executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
		configChanged = len(cs) > 0
	}

	// Signal restart only if the runtime config actually changed (not just
	// Docker being stopped). The configChanged flag is set by file writes
	// in reconcileContainerd/reconcileDocker, not by service state changes.
	if configChanged && req.Restarts != nil {
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

func reconcileContainerd(ctx context.Context, cfg config.Config, executor *host.Executor) ([]host.Change, bool, error) {
	var changes []host.Change
	configChanged := false
	tarballExtracted := false

	// Stop, disable, and mask docker if it's running. Mask prevents
	// socket-activation or dependency-based starts on subsequent boots,
	// so this block only fires once.
	if executor.SystemctlIsActive("docker") {
		_ = executor.Run("systemctl", "stop", "docker")
		_ = executor.Run("systemctl", "disable", "docker")
		_ = executor.Run("systemctl", "mask", "docker")
		_ = executor.Run("systemctl", "mask", "docker.socket")
		changes = append(changes, host.Change{Action: host.ActionOther, Summary: "disable docker in favour of containerd"})
	}

	// Determine tarball URL and install layout.
	//
	// containerd 1.x published "cri-containerd-cni-VERSION-linux-amd64.tar.gz"
	// which extracts to "/" and includes binaries, CNI, runc, systemd unit, and
	// a default config.toml.
	//
	// containerd 2.x dropped the cri-containerd-cni bundle. It publishes
	// "containerd-VERSION-linux-amd64.tar.gz" which contains only the containerd
	// binaries under "bin/" and must be extracted to "/usr/local". CNI plugins
	// and runc are installed separately (CNI is handled by the network driver
	// module).
	tarballURL := cfg.Shared.ContainerdTarballURL
	useV2Layout := false

	// Detect containerd major version from CONTAINERD_VERSION if available.
	if cfg.Shared.ContainerdVersion != "" {
		if major, _, ok := parseContainerdMajor(cfg.Shared.ContainerdVersion); ok && major >= 2 {
			useV2Layout = true
		}
	}

	if tarballURL == "" && cfg.Shared.ContainerdVersion != "" {
		if useV2Layout {
			tarballURL = fmt.Sprintf(
				"https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz",
				cfg.Shared.ContainerdVersion, cfg.Shared.ContainerdVersion,
			)
		} else {
			tarballURL = fmt.Sprintf(
				"https://github.com/containerd/containerd/releases/download/v%s/cri-containerd-cni-%s-linux-amd64.tar.gz",
				cfg.Shared.ContainerdVersion, cfg.Shared.ContainerdVersion,
			)
		}
	}

	if tarballURL != "" {
		localPath := "/srv/magnum/containerd.tar.gz"
		dl, err := executor.DownloadFileWithRetry(ctx, tarballURL, localPath, 0o644, 5)
		if err != nil {
			return nil, false, fmt.Errorf("download containerd tarball: %w", err)
		}
		if dl.Change != nil {
			changes = append(changes, *dl.Change)
		}
		if dl.Changed && executor.Apply {
			if useV2Layout {
				// containerd 2.x: tarball has bin/ directory, extract to /usr/local.
				if err := executor.Run("tar", "xzf", localPath,
					"-C", "/usr/local",
					"--no-same-owner", "--touch", "--no-same-permissions",
				); err != nil {
					return nil, false, fmt.Errorf("extract containerd tarball: %w", err)
				}
			} else {
				// containerd 1.x cri-containerd-cni bundle: extract to /.
				if err := executor.Run("tar", "xzf", localPath,
					"-C", "/",
					"--no-same-owner", "--touch", "--no-same-permissions",
					"--exclude=etc/cni/net.d",
					"--exclude=etc/containerd/config.toml",
					"--exclude=opt/cni/bin",
					"--exclude=*.txt",
					"--exclude=opt/containerd/cluster/gce",
				); err != nil {
					return nil, false, fmt.Errorf("extract containerd tarball: %w", err)
				}
			}
			tarballExtracted = true
		}
	}

	// Ensure directories.
	for _, dir := range []string{"/etc/containerd", "/etc/containerd/certs.d", "/opt/cni/bin"} {
		change, err := executor.EnsureDir(dir, 0o755)
		if err != nil {
			return nil, false, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}

	// Write containerd config.toml — EnsureFile is idempotent.
	// Config format is driven by containerd version (not K8s version):
	// containerd 2.x requires version=3 config with new CRI plugin paths.
	pause := pauseImage(cfg.Shared.KubeTag)
	var configContent string
	if useV2Layout {
		configContent = containerdV3Config(pause)
	} else {
		configContent = containerdV2Config(pause)
	}
	change, err := executor.EnsureFile("/etc/containerd/config.toml", []byte(configContent), 0o644)
	if err != nil {
		return nil, false, err
	}
	if change != nil {
		changes = append(changes, *change)
		configChanged = true
	}

	// Write Docker Hub registry host config (containerd 2.x registry config_path).
	// containerd 2.x uses config_path-based registry config instead of inline
	// registry.mirrors in config.toml.
	if useV2Layout {
		dockerHubHost := `server = "https://registry-1.docker.io"

[host."https://registry-1.docker.io"]
  capabilities = ["pull", "resolve"]
`
		change, err = executor.EnsureDir("/etc/containerd/certs.d/docker.io", 0o755)
		if err != nil {
			return nil, false, err
		}
		if change != nil {
			changes = append(changes, *change)
		}
		change, err = executor.EnsureFile("/etc/containerd/certs.d/docker.io/hosts.toml", []byte(dockerHubHost), 0o644)
		if err != nil {
			return nil, false, err
		}
		if change != nil {
			changes = append(changes, *change)
			configChanged = true
		}
	}

	// Daemon-reload when tarball was extracted (may contain updated service
	// files) or when config.toml changed.
	if tarballExtracted || configChanged {
		_ = executor.Run("systemctl", "daemon-reload")
	}

	// Enable is idempotent — always safe to run.
	_ = executor.Run("systemctl", "enable", "containerd")

	// Detect drift: if containerd should be running but isn't, start it.
	if !executor.SystemctlIsActive("containerd") {
		if err := executor.Run("systemctl", "start", "containerd"); err != nil {
			return nil, false, err
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: "start containerd (was not running)"})
	}

	return changes, configChanged, nil
}

func reconcileDocker(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	var changes []host.Change

	cgroupDriver := cfg.ResolveCgroupDriver()

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

// pauseImage returns the appropriate pause container image for the given K8s version.
var pauseImageVersions = map[string]string{
	"1.35": "3.10",
	"1.34": "3.10",
	"1.33": "3.10",
	"1.32": "3.10",
	"1.31": "3.9",
	"1.30": "3.9",
	"1.29": "3.9",
	"1.28": "3.9",
	"1.27": "3.9",
	"1.26": "3.9",
	"1.25": "3.8",
	"1.24": "3.7",
	"1.23": "3.6",
	"1.22": "3.5",
	"1.21": "3.4.1",
	"1.20": "3.2",
}

func pauseImage(kubeTag string) string {
	v := config.LookupByKubeVersion(pauseImageVersions, kubeTag)
	if v == "" {
		v = "3.10"
	}
	return "registry.k8s.io/pause:" + v
}

// containerdV3Config returns a containerd 2.x (config version 3) config.
func containerdV3Config(pause string) string {
	return fmt.Sprintf(`version = 3

[plugins]
  [plugins."io.containerd.cri.v1.images"]
    sandbox_image = "%s"

  [plugins."io.containerd.cri.v1.images".pinned_images]
    sandbox = "%s"

  [plugins."io.containerd.cri.v1.images".registry]
    config_path = "/etc/containerd/certs.d"

  [plugins."io.containerd.cri.v1.runtime"]
    max_container_log_line_size = 16384
    enable_unprivileged_ports = true
    enable_unprivileged_icmp = true

  [plugins."io.containerd.cri.v1.cni"]
    bin_dir = "/opt/cni/bin"
    conf_dir = "/etc/cni/net.d"

  [plugins."io.containerd.snapshotter.v1.overlayfs"]

  [plugins."io.containerd.runtime.v2.task"]

[debug]
  level = "info"
`, pause, pause)
}

// containerdV2Config returns a containerd 1.x (config version 2) config.
func containerdV2Config(pause string) string {
	return fmt.Sprintf(`version = 2
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
    sandbox_image = "%s"
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
`, pause)
}

// parseContainerdMajor extracts the major version from a containerd version
// string like "2.2.2" or "1.7.27". Returns (major, rest, ok).
func parseContainerdMajor(version string) (int, string, bool) {
	dot := strings.IndexByte(version, '.')
	if dot < 1 {
		return 0, "", false
	}
	n := 0
	for _, ch := range version[:dot] {
		if ch < '0' || ch > '9' {
			return 0, "", false
		}
		n = n*10 + int(ch-'0')
	}
	return n, version[dot+1:], true
}

// Destroy stops container runtime services and removes runtime data.
func (Module) Destroy(_ context.Context, cfg config.Config, req moduleapi.Request) error {
	executor := host.NewExecutor(req.Apply, req.Logger)

	if req.Logger != nil {
		req.Logger.Infof("container-runtime destroy: stopping containerd and docker services")
	}
	_ = executor.Run("systemctl", "stop", "containerd")
	_ = executor.Run("systemctl", "disable", "containerd")
	_ = executor.Run("systemctl", "stop", "docker")
	_ = executor.Run("systemctl", "disable", "docker")

	if req.Logger != nil {
		req.Logger.Infof("container-runtime destroy: removing config and data")
	}
	_ = os.Remove("/etc/containerd/config.toml")
	_ = os.RemoveAll("/var/lib/containerd")

	return nil
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
