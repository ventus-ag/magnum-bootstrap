package containerruntime

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

// containerdVersions maps Kubernetes minor version to the containerd version.
// Based on containerd.io/releases support matrix:
//   - K8s >= 1.35: containerd 2.2.x (officially supports 1.35+)
//   - K8s 1.32-1.34: containerd 2.1.x (officially supports 1.32-1.35)
//   - K8s < 1.32: containerd 1.7.x LTS (officially supports through 1.35, LTS until Sep 2026)
var containerdVersions = map[string]string{
	"1.35": "2.2.2",
	"1.32": "2.1.6",
	"1.31": "1.7.30",
}

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "container-runtime" }
func (Module) Dependencies() []string { return []string{"ca-rotation"} }

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
		cs, changed, err := reconcileDocker(cfg, executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, cs...)
		configChanged = changed
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

	legacyDockerUnit, err := removeLegacyDockerUnitOverride(executor, "/etc/systemd/system/docker.service")
	if err != nil {
		return nil, false, err
	}
	if legacyDockerUnit != nil {
		changes = append(changes, *legacyDockerUnit)
	}

	for _, unit := range []string{"docker", "docker.socket"} {
		res, err := (hostresource.SystemdServiceSpec{
			Unit:          unit,
			SkipIfMissing: true,
			Enabled:       hostresource.BoolPtr(false),
			Active:        hostresource.BoolPtr(false),
			Masked:        hostresource.BoolPtr(true),
		}).Apply(executor)
		if err != nil {
			return nil, false, err
		}
		changes = append(changes, res.Changes...)
	}

	// Select containerd version by Kubernetes version.
	containerdVersion := config.LookupByKubeVersion(containerdVersions, cfg.Shared.KubeTag)
	useV2Layout := false
	if major, _, ok := parseContainerdMajor(containerdVersion); ok && major >= 2 {
		useV2Layout = true
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
	var tarballURL string
	if useV2Layout {
		tarballURL = fmt.Sprintf(
			"https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz",
			containerdVersion, containerdVersion,
		)
	} else {
		tarballURL = fmt.Sprintf(
			"https://github.com/containerd/containerd/releases/download/v%s/cri-containerd-cni-%s-linux-amd64.tar.gz",
			containerdVersion, containerdVersion,
		)
	}

	// Check if the desired version is already installed. If a different
	// version is running (e.g. OS-bundled 1.6.x), stop it first so we
	// install and start the correct binary cleanly.
	versionOK := containerdVersionMatches(executor, containerdVersion)
	needsInstall := !versionOK

	if needsInstall && executor.SystemctlIsActive("containerd") {
		res, err := (hostresource.SystemdServiceSpec{
			Unit:          "containerd",
			SkipIfMissing: true,
			Active:        hostresource.BoolPtr(false),
		}).Apply(executor)
		if err != nil {
			return nil, false, err
		}
		changes = append(changes, res.Changes...)
	}

	if versionOK {
		// Already on the requested containerd version; skip download.
	} else {
		localPath := "/srv/magnum/containerd.tar.gz"
		download := hostresource.DownloadSpec{URL: tarballURL, Path: localPath, Mode: 0o644, Retries: 5}
		dl, err := download.ApplyContext(ctx, executor)
		if err != nil {
			return nil, false, fmt.Errorf("download containerd tarball: %w", err)
		}
		changes = append(changes, dl.Changes...)
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
		configChanged = true
	}

	// containerd 2.x installs to /usr/local/bin but the OS systemd unit
	// (Fedora CoreOS, /usr/lib/systemd/system/containerd.service) still
	// references /usr/bin/containerd. Override ExecStart via drop-in so
	// systemd starts the correct binary. The /usr tree is immutable on
	// ostree systems, so we cannot replace the binary in-place.
	if useV2Layout {
		dropinDir := "/etc/systemd/system/containerd.service.d"
		dirResult, err := (hostresource.DirectorySpec{Path: dropinDir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, false, err
		}
		changes = append(changes, dirResult.Changes...)
		dropin := "[Service]\nExecStart=\nExecStart=/usr/local/bin/containerd\n"
		fileResult, err := (hostresource.FileSpec{Path: dropinDir + "/10-exec-start.conf", Content: []byte(dropin), Mode: 0o644}).Apply(executor)
		if err != nil {
			return nil, false, err
		}
		changes = append(changes, fileResult.Changes...)
		configChanged = configChanged || fileResult.Changed
	}

	// Ensure directories.
	for _, dir := range []string{"/etc/containerd", "/etc/containerd/certs.d", "/opt/cni/bin"} {
		result, err := (hostresource.DirectorySpec{Path: dir, Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, false, err
		}
		changes = append(changes, result.Changes...)
	}

	// Write containerd config.toml — EnsureFile is idempotent.
	// Config format is driven by containerd version (not K8s version):
	// containerd 2.x requires version=3 config with new CRI plugin paths.
	pause := pauseImage(cfg.Shared.KubeTag)
	systemdCgroup := containerdUsesSystemdCgroup(cfg.ResolveCgroupDriver())
	var configContent string
	if useV2Layout {
		configContent = containerdV3Config(pause, systemdCgroup)
	} else {
		configContent = containerdV2Config(pause, systemdCgroup)
	}
	configResult, err := (hostresource.FileSpec{Path: "/etc/containerd/config.toml", Content: []byte(configContent), Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, false, err
	}
	changes = append(changes, configResult.Changes...)
	configChanged = configChanged || configResult.Changed

	// Write Docker Hub registry host config (containerd 2.x registry config_path).
	if useV2Layout {
		dockerHubHost := `server = "https://registry-1.docker.io"

[host."https://registry-1.docker.io"]
  capabilities = ["pull", "resolve"]
`
		dirResult, err := (hostresource.DirectorySpec{Path: "/etc/containerd/certs.d/docker.io", Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, false, err
		}
		changes = append(changes, dirResult.Changes...)
		hostsResult, err := (hostresource.FileSpec{Path: "/etc/containerd/certs.d/docker.io/hosts.toml", Content: []byte(dockerHubHost), Mode: 0o644}).Apply(executor)
		if err != nil {
			return nil, false, err
		}
		changes = append(changes, hostsResult.Changes...)
		configChanged = configChanged || hostsResult.Changed
	}

	serviceResult, err := (hostresource.SystemdServiceSpec{
		Unit:          "containerd",
		SkipIfMissing: true,
		DaemonReload:  configChanged,
		Enabled:       hostresource.BoolPtr(true),
		Active:        hostresource.BoolPtr(true),
	}).Apply(executor)
	if err != nil {
		return nil, false, err
	}
	changes = append(changes, serviceResult.Changes...)

	return changes, configChanged, nil
}

func containerdVersionMatches(executor *host.Executor, desiredVersion string) bool {
	if desiredVersion == "" {
		return false
	}
	// Check /usr/local/bin first (containerd 2.x install path), then fall
	// back to bare "containerd" for 1.x which installs to /usr/bin via the
	// cri-containerd-cni bundle.
	for _, bin := range []string{"/usr/local/bin/containerd", "containerd"} {
		out, err := executor.RunCapture(bin, "--version")
		if err != nil {
			continue
		}
		if strings.Contains(out, desiredVersion) {
			return true
		}
	}
	return false
}

func reconcileDocker(cfg config.Config, executor *host.Executor) ([]host.Change, bool, error) {
	var changes []host.Change
	configChanged := false

	cgroupDriver := cfg.ResolveCgroupDriver()

	dropinDir := "/etc/systemd/system/docker.service.d"
	dirResult, err := (hostresource.DirectorySpec{Path: dropinDir, Mode: 0o755}).Apply(executor)
	if err != nil {
		return nil, false, err
	}
	changes = append(changes, dirResult.Changes...)

	content := fmt.Sprintf("[Service]\nExecStart=\nExecStart=/usr/bin/dockerd --exec-opt native.cgroupdriver=%s\n", cgroupDriver)
	fileResult, err := (hostresource.FileSpec{Path: dropinDir + "/cgroupdriver.conf", Content: []byte(content), Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, false, err
	}
	changes = append(changes, fileResult.Changes...)
	configChanged = fileResult.Changed

	serviceResult, err := (hostresource.SystemdServiceSpec{
		Unit:          "docker",
		SkipIfMissing: true,
		Masked:        hostresource.BoolPtr(false),
		DaemonReload:  configChanged,
		Enabled:       hostresource.BoolPtr(true),
		Active:        hostresource.BoolPtr(true),
	}).Apply(executor)
	if err != nil {
		return nil, false, err
	}
	changes = append(changes, serviceResult.Changes...)

	return changes, configChanged, nil
}

func containerdTarballURL(version string, useV2Layout bool) string {
	if useV2Layout {
		return fmt.Sprintf(
			"https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz",
			version, version,
		)
	}
	return fmt.Sprintf(
		"https://github.com/containerd/containerd/releases/download/v%s/cri-containerd-cni-%s-linux-amd64.tar.gz",
		version, version,
	)
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
func containerdV3Config(pause string, systemdCgroup bool) string {
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

   [plugins."io.containerd.cri.v1.runtime".runtimes.runc]
     runtime_type = "io.containerd.runc.v2"
     [plugins."io.containerd.cri.v1.runtime".runtimes.runc.options]
       SystemdCgroup = %t

  [plugins."io.containerd.cri.v1.cni"]
    bin_dir = "/opt/cni/bin"
    conf_dir = "/etc/cni/net.d"

  [plugins."io.containerd.snapshotter.v1.overlayfs"]

  [plugins."io.containerd.runtime.v2.task"]

[debug]
  level = "info"
`, pause, pause, systemdCgroup)
}

// containerdV2Config returns a containerd 1.x (config version 2) config.
func containerdV2Config(pause string, systemdCgroup bool) string {
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
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
        runtime_type = "io.containerd.runc.v2"
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
          SystemdCgroup = %t
    [plugins."io.containerd.grpc.v1.cri".registry]
      [plugins."io.containerd.grpc.v1.cri".registry.mirrors]
        [plugins."io.containerd.grpc.v1.cri".registry.mirrors."docker.io"]
          endpoint = ["https://registry-1.docker.io"]
  [plugins."io.containerd.internal.v1.opt"]
    path = "/var/lib/containerd/opt"
`, pause, systemdCgroup)
}

func containerdUsesSystemdCgroup(cgroupDriver string) bool {
	return strings.EqualFold(cgroupDriver, "systemd")
}

func removeLegacyDockerUnitOverride(executor *host.Executor, path string) (*host.Change, error) {
	remove, err := legacyDockerUnitNeedsRemoval(path)
	if err != nil {
		return nil, err
	}
	if !remove {
		return nil, nil
	}
	return executor.EnsureAbsent(path)
}

func legacyDockerUnitNeedsRemoval(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return true, nil
	}
	target, err := os.Readlink(path)
	if err != nil {
		return false, err
	}
	return target != "/dev/null", nil
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
	childOpts := hostresource.ChildResourceOptions(res, opts...)

	containerdVersion := config.LookupByKubeVersion(containerdVersions, cfg.Shared.KubeTag)
	useV2Layout := false
	if major, _, ok := parseContainerdMajor(containerdVersion); ok && major >= 2 {
		useV2Layout = true
	}

	if cfg.Shared.ContainerRuntime == "containerd" {
		var tarballRes pulumi.Resource
		var serviceDeps []pulumi.Resource
		var err error
		for _, unit := range []string{"docker", "docker.socket"} {
			serviceOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, serviceDeps...)
			if _, err := hostsdk.RegisterSystemdServiceSpec(ctx, name+"-disable-"+strings.ReplaceAll(unit, ".", "-"), hostresource.SystemdServiceSpec{
				Unit:          unit,
				SkipIfMissing: true,
				Enabled:       hostresource.BoolPtr(false),
				Active:        hostresource.BoolPtr(false),
				Masked:        hostresource.BoolPtr(true),
			}, serviceOpts...); err != nil {
				return nil, err
			}
		}
		tarballRes, err = hostsdk.RegisterDownloadSpec(ctx, name+"-tarball", hostresource.DownloadSpec{URL: containerdTarballURL(containerdVersion, useV2Layout), Path: "/srv/magnum/containerd.tar.gz", Mode: 0o644, Retries: 5}, childOpts...)
		if err != nil {
			return nil, err
		}
		serviceDeps = append(serviceDeps, tarballRes)
		dirResources := map[string]pulumi.Resource{}
		for _, dir := range []string{"/etc/containerd", "/etc/containerd/certs.d", "/opt/cni/bin"} {
			resDir, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dir-"+strings.ReplaceAll(strings.Trim(dir, "/"), "/", "-"), hostresource.DirectorySpec{Path: dir, Mode: 0o755}, childOpts...)
			if err != nil {
				return nil, err
			}
			dirResources[dir] = resDir
		}
		pause := pauseImage(cfg.Shared.KubeTag)
		systemdCgroup := containerdUsesSystemdCgroup(cfg.ResolveCgroupDriver())
		configContent := containerdV2Config(pause, systemdCgroup)
		var configDeps []pulumi.Resource
		configDeps = append(configDeps, dirResources["/etc/containerd"])
		if useV2Layout {
			configContent = containerdV3Config(pause, systemdCgroup)
			dropinDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-containerd-dropin-dir", hostresource.DirectorySpec{Path: "/etc/systemd/system/containerd.service.d", Mode: 0o755}, childOpts...)
			if err != nil {
				return nil, err
			}
			dropinOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dropinDirRes)
			dropinRes, err := hostsdk.RegisterFileSpec(ctx, name+"-containerd-dropin", hostresource.FileSpec{Path: "/etc/systemd/system/containerd.service.d/10-exec-start.conf", Content: []byte("[Service]\nExecStart=\nExecStart=/usr/local/bin/containerd\n"), Mode: 0o644}, dropinOpts...)
			if err != nil {
				return nil, err
			}
			serviceDeps = append(serviceDeps, dropinRes)
			dockerioDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dockerio-dir", hostresource.DirectorySpec{Path: "/etc/containerd/certs.d/docker.io", Mode: 0o755}, childOpts...)
			if err != nil {
				return nil, err
			}
			hostsOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dockerioDirRes)
			hostsRes, err := hostsdk.RegisterFileSpec(ctx, name+"-dockerio-hosts", hostresource.FileSpec{Path: "/etc/containerd/certs.d/docker.io/hosts.toml", Content: []byte(`server = "https://registry-1.docker.io"

[host."https://registry-1.docker.io"]
  capabilities = ["pull", "resolve"]
`), Mode: 0o644}, hostsOpts...)
			if err != nil {
				return nil, err
			}
			configDeps = append(configDeps, hostsRes)
		}
		configOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, configDeps...)
		configRes, err := hostsdk.RegisterFileSpec(ctx, name+"-config", hostresource.FileSpec{Path: "/etc/containerd/config.toml", Content: []byte(configContent), Mode: 0o644}, configOpts...)
		if err != nil {
			return nil, err
		}
		serviceDeps = append(serviceDeps, configRes)
		serviceOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, serviceDeps...)
		if _, err := hostsdk.RegisterSystemdServiceSpec(ctx, name+"-service", hostresource.SystemdServiceSpec{Unit: "containerd", SkipIfMissing: true, Enabled: hostresource.BoolPtr(true), Active: hostresource.BoolPtr(true)}, serviceOpts...); err != nil {
			return nil, err
		}
	} else {
		dropinDir := "/etc/systemd/system/docker.service.d"
		content := fmt.Sprintf("[Service]\nExecStart=\nExecStart=/usr/bin/dockerd --exec-opt native.cgroupdriver=%s\n", cfg.ResolveCgroupDriver())
		dropinDirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-docker-dropin-dir", hostresource.DirectorySpec{Path: dropinDir, Mode: 0o755}, childOpts...)
		if err != nil {
			return nil, err
		}
		dropinOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dropinDirRes)
		dropinRes, err := hostsdk.RegisterFileSpec(ctx, name+"-docker-dropin", hostresource.FileSpec{Path: dropinDir + "/cgroupdriver.conf", Content: []byte(content), Mode: 0o644}, dropinOpts...)
		if err != nil {
			return nil, err
		}
		serviceOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dropinRes)
		if _, err := hostsdk.RegisterSystemdServiceSpec(ctx, name+"-docker-service", hostresource.SystemdServiceSpec{Unit: "docker", SkipIfMissing: true, Masked: hostresource.BoolPtr(false), Enabled: hostresource.BoolPtr(true), Active: hostresource.BoolPtr(true)}, serviceOpts...); err != nil {
			return nil, err
		}
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"containerRuntime":  pulumi.String(cfg.Shared.ContainerRuntime),
		"containerdVersion": pulumi.String(containerdVersion),
		"cgroupDriver":      pulumi.String(cfg.ResolveCgroupDriver()),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
