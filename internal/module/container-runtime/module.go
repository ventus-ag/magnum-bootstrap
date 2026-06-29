package containerruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
	"1.36": "2.3.2",
	"1.35": "2.2.5",
	"1.32": "2.1.9",
	"1.31": "1.7.30",
}

// minGlibcMinorForContainerd2 is the host glibc minor version (the MM in 2.MM)
// required to run the official containerd 2.x release binaries. Those binaries
// are dynamically linked against GLIBC_2.34, so a node on older glibc (for
// example Fedora CoreOS 34, glibc 2.33) cannot exec them at all — containerd
// dies with "/usr/local/bin/containerd: /lib64/libc.so.6: version
// `GLIBC_2.34' not found". Such nodes must install a statically linked build.
const minGlibcMinorForContainerd2 = 34

// staticContainerdFallback is the statically linked containerd 1.7.x LTS release
// (cri-containerd-cni bundle) used on old-glibc nodes when no static containerd
// 2.x bundle is known for the desired 2.x line. It is fully static (runs on any
// glibc) and its CRI plugin officially supports Kubernetes through 1.35.
const staticContainerdFallback = "1.7.30"

// nerdctlFullStatic maps a containerd 2.x minor line ("2.1", "2.2", ...) to the
// nerdctl-full release that ships a statically linked containerd of that line,
// plus the exact containerd version that bundle contains. nerdctl-full is the
// only prebuilt source of static containerd 2.x binaries; the official
// containerd release tarballs are dynamically linked (GLIBC_2.34). Used to keep
// containerd 2.x running on old-glibc nodes (e.g. in-place k8s 1.32+ upgrades of
// Fedora CoreOS 34). The bundled containerd patch may trail the version map's
// preferred 2.x patch — acceptable; it stays on the same 2.x line.
var nerdctlFullStatic = map[string]struct{ nerdctlVersion, containerdVersion string }{
	"2.1": {nerdctlVersion: "2.1.6", containerdVersion: "2.1.4"},
	"2.2": {nerdctlVersion: "2.2.2", containerdVersion: "2.2.1"},
	"2.3": {nerdctlVersion: "2.3.4", containerdVersion: "2.3.2"},
}

// containerdSelection describes the containerd artifact to install on this node.
type containerdSelection struct {
	version       string // containerd version, matched against `containerd --version`
	url           string // tarball URL to download
	v2Layout      bool   // install under /usr/local (containerd 2.x, official or static)
	staticNerdctl bool   // source is a nerdctl-full bundle (extract only needed binaries)
}

// selectContainerd picks the containerd artifact for the node's Kubernetes
// version. On nodes whose glibc is too old to run the dynamically linked
// official containerd 2.x binaries it substitutes a statically linked source:
// a nerdctl-full bundle for the same 2.x line when one is known, otherwise the
// static containerd 1.7.x LTS cri bundle (supported through k8s 1.35).
func selectContainerd(kubeTag string) containerdSelection {
	version := config.LookupByKubeVersion(containerdVersions, kubeTag)
	major, _, ok := parseContainerdMajor(version)
	v2 := ok && major >= 2

	if v2 && !hostGlibcAtLeastMinor(minGlibcMinorForContainerd2) {
		if st, ok := nerdctlFullStatic[containerdMinorLine(version)]; ok {
			return containerdSelection{
				version:       st.containerdVersion,
				url:           nerdctlFullURL(st.nerdctlVersion),
				v2Layout:      true,
				staticNerdctl: true,
			}
		}
		return containerdSelection{
			version:  staticContainerdFallback,
			url:      containerdTarballURL(staticContainerdFallback, false),
			v2Layout: false,
		}
	}

	return containerdSelection{
		version:  version,
		url:      containerdTarballURL(version, v2),
		v2Layout: v2,
	}
}

// containerdMinorLine returns the "major.minor" line of a containerd version
// (e.g. "2.1.4" -> "2.1"). Returns "" if the version is not in major.minor form.
func containerdMinorLine(version string) string {
	first := strings.IndexByte(version, '.')
	if first < 1 {
		return ""
	}
	second := strings.IndexByte(version[first+1:], '.')
	if second < 1 {
		return version // already "major.minor"
	}
	return version[:first+1+second]
}

// nerdctlFullURL builds the nerdctl-full bundle URL for a nerdctl release.
func nerdctlFullURL(nerdctlVersion string) string {
	return fmt.Sprintf(
		"https://github.com/containerd/nerdctl/releases/download/v%s/nerdctl-full-%s-linux-amd64.tar.gz",
		nerdctlVersion, nerdctlVersion,
	)
}

// hostGlibcAtLeastMinor reports whether the host glibc is at least 2.<minMinor>.
// It parses the first line of `ldd --version`, whose final whitespace-separated
// token is the glibc version (e.g. "ldd (GNU libc) 2.33" or
// "ldd (Ubuntu GLIBC 2.35-0ubuntu3.4) 2.35"). On any exec/parse failure it
// returns true (assume a modern glibc) so detection problems never force the
// static fallback on nodes that can run the official binaries; the case we guard
// is a positively detected old glibc.
func hostGlibcAtLeastMinor(minMinor int) bool {
	out, err := exec.Command("ldd", "--version").Output()
	if err != nil {
		return true
	}
	first := string(out)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	fields := strings.Fields(first)
	if len(fields) == 0 {
		return true
	}
	major, minor, ok := parseGlibcVersion(fields[len(fields)-1])
	if !ok {
		return true
	}
	if major != 2 {
		return major > 2
	}
	return minor >= minMinor
}

// parseGlibcVersion extracts the major and minor from a glibc version token such
// as "2.33" or "2.35-0ubuntu3.4".
func parseGlibcVersion(s string) (major, minor int, ok bool) {
	dot := strings.IndexByte(s, '.')
	if dot < 1 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(s[:dot])
	if err != nil {
		return 0, 0, false
	}
	rest := s[dot+1:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(rest[:end])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
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

	// Detect a genuine cgroup-driver FLIP before we overwrite the runtime config.
	// ResolveCgroupDriver preserves the on-disk driver unless our app explicitly
	// defines CGROUP_DRIVER, so a difference here means the operator asked to
	// change it. A driver change must restart kubelet AND the runtime together,
	// or they sit in a mismatched window where runc rejects every new pod sandbox
	// (`expected cgroupsPath to be of format "slice:prefix:name"`).
	priorCgroupDriver := config.ExistingCgroupDriver()
	desiredCgroupDriver := cfg.ResolveCgroupDriver()
	cgroupDriverFlipped := priorCgroupDriver != "" && !strings.EqualFold(priorCgroupDriver, desiredCgroupDriver)

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

	// On a genuine cgroup-driver flip, the runtime's config necessarily changed
	// (handled above) — but kubelet must restart too so it emits cgroup paths in
	// the SAME format the runtime now expects. kube-master-config also rewrites
	// kubelet-config (which would trigger this), but signal it explicitly here so
	// the flip is safe even if the kubelet config text is otherwise unchanged.
	if cgroupDriverFlipped && req.Restarts != nil {
		req.Restarts.Add("kubelet", "cgroup driver flip "+priorCgroupDriver+"->"+desiredCgroupDriver)
		if req.Logger != nil {
			req.Logger.Infof("container-runtime: cgroup driver flipping %s->%s; restarting kubelet with the runtime", priorCgroupDriver, desiredCgroupDriver)
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

	strayCNI, err := removeStrayCNIConfs(cfg, executor)
	if err != nil {
		return nil, false, err
	}
	changes = append(changes, strayCNI...)

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

	// Select the containerd artifact for this Kubernetes version. On old-glibc
	// nodes selectContainerd swaps the dynamically linked official containerd 2.x
	// for a statically linked source (nerdctl-full for the same 2.x line, or the
	// static 1.7.x LTS cri bundle) so the binary can actually exec.
	//
	// Layouts:
	//   - containerd 1.x cri-containerd-cni bundle: extracts to "/" and includes
	//     binaries, CNI, runc, systemd unit, default config.toml.
	//   - containerd 2.x official tarball: only containerd binaries under "bin/",
	//     installed to "/usr/local"; CNI/runc handled separately.
	//   - nerdctl-full bundle: static containerd 2.x; we extract only the
	//     containerd binaries (+ static runc) we need (the bundle is ~260MB).
	wantedVersion := config.LookupByKubeVersion(containerdVersions, cfg.Shared.KubeTag)
	sel := selectContainerd(cfg.Shared.KubeTag)
	containerdVersion := sel.version
	useV2Layout := sel.v2Layout
	tarballURL := sel.url
	if sel.version != wantedVersion && executor.Logger != nil {
		source := "cri-bundle 1.7.x"
		if sel.staticNerdctl {
			source = "nerdctl-full"
		}
		executor.Logger.Infof("container-runtime: host glibc too old for dynamically linked containerd %s; installing static containerd %s (%s)",
			wantedVersion, sel.version, source)
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
		// Install roots differ by OS. Fedora CoreOS makes /usr/local and /opt
		// symlinks into the writable /var (/usr/local -> ../var/usrlocal,
		// /opt -> ../var/opt) while /usr itself is a read-only composefs overlay.
		// tar cannot create the cri bundle's usr/local/* or opt/* members through
		// those symlinks — composefs rejects the directory create with EXDEV
		// ("Invalid cross-device link"), even with --keep-directory-symlink and a
		// pre-created target. So on FCoS never extract through /usr: extract straight
		// into the writable /var target (containerd 2.x) or unpack to a scratch dir
		// and copy each tree into its real writable location (containerd 1.x).
		// /var/usrlocal IS /usr/local and /var/opt IS /opt there. On Ubuntu /usr/local
		// and /opt are ordinary writable directories (and /var/usrlocal does NOT
		// exist), so we must target them directly — otherwise the binary lands in
		// /var/usrlocal/bin while the systemd drop-in below points at
		// /usr/local/bin/containerd and containerd fails to start. /etc is writable
		// on both. This installs on any FCoS image (composefs or not) and on Ubuntu
		// with no pre-provisioning.
		usrLocalRoot, optRoot := "/var/usrlocal", "/var/opt"
		if cfg.IsUbuntu() {
			usrLocalRoot, optRoot = "/usr/local", "/opt"
		}
		usrLocalTarget, err := (hostresource.DirectorySpec{Path: usrLocalRoot, Mode: 0o755}).Apply(executor)
		if err != nil {
			return nil, false, fmt.Errorf("ensure containerd install target %s: %w", usrLocalRoot, err)
		}
		changes = append(changes, usrLocalTarget.Changes...)

		if useV2Layout {
			// containerd 2.x: tarball has bin/ at the archive root. Unpack to a
			// writable scratch dir, then copy into the usr/local root. This phase
			// runs before stop-services, so the *running* containerd binary must be
			// replaced atomically: a straight `tar -C usrLocalRoot` overwrite hits
			// ETXTBSY ("text file busy"), and `tar --unlink-first` fails on the
			// pre-existing non-empty bin/ directory ("Cannot unlink: Directory not
			// empty"). `cp --remove-destination` unlinks each destination file first,
			// so the new binary lands at the path while the old inode stays mapped
			// for the live process — same approach as the 1.x path below.
			scratch := "/srv/magnum/containerd-bundle"
			if err := executor.Run("rm", "-rf", scratch); err != nil {
				return nil, false, fmt.Errorf("clean containerd scratch dir: %w", err)
			}
			scratchDir, err := (hostresource.DirectorySpec{Path: scratch, Mode: 0o755}).Apply(executor)
			if err != nil {
				return nil, false, fmt.Errorf("create containerd scratch dir: %w", err)
			}
			changes = append(changes, scratchDir.Changes...)
			if sel.staticNerdctl {
				// nerdctl-full is ~260MB and ships buildkit/CNI/stargz/etc. under
				// bin,lib,libexec,share. Extract only the static containerd binaries we
				// install. runc is placed in sbin so it precedes any older (possibly
				// dynamically linked, glibc-incompatible) runc earlier in the systemd
				// PATH (/usr/local/sbin before /usr/local/bin).
				if err := executor.Run("tar", "xzf", localPath,
					"-C", scratch,
					"--no-same-owner", "--touch", "--no-same-permissions",
					"bin/containerd", "bin/containerd-shim-runc-v2", "bin/ctr", "bin/runc",
				); err != nil {
					return nil, false, fmt.Errorf("extract nerdctl-full containerd binaries: %w", err)
				}
				binDst := usrLocalRoot + "/bin"
				binDir, err := (hostresource.DirectorySpec{Path: binDst, Mode: 0o755}).Apply(executor)
				if err != nil {
					return nil, false, fmt.Errorf("ensure containerd bin dir %s: %w", binDst, err)
				}
				changes = append(changes, binDir.Changes...)
				for _, b := range []string{"containerd", "containerd-shim-runc-v2", "ctr"} {
					if err := executor.Run("cp", "-a", "--remove-destination", scratch+"/bin/"+b, binDst+"/"); err != nil {
						return nil, false, fmt.Errorf("install %s -> %s: %w", b, binDst, err)
					}
				}
				sbinDst := usrLocalRoot + "/sbin"
				sbinDir, err := (hostresource.DirectorySpec{Path: sbinDst, Mode: 0o755}).Apply(executor)
				if err != nil {
					return nil, false, fmt.Errorf("ensure runc sbin dir %s: %w", sbinDst, err)
				}
				changes = append(changes, sbinDir.Changes...)
				if err := executor.Run("cp", "-a", "--remove-destination", scratch+"/bin/runc", sbinDst+"/runc"); err != nil {
					return nil, false, fmt.Errorf("install runc -> %s: %w", sbinDst, err)
				}
				if err := executor.Run("rm", "-rf", scratch); err != nil {
					return nil, false, fmt.Errorf("remove containerd scratch dir: %w", err)
				}
			} else {
				// containerd 2.x official tarball: bin/ at the archive root.
				if err := executor.Run("tar", "xzf", localPath,
					"-C", scratch,
					"--no-same-owner", "--touch", "--no-same-permissions",
				); err != nil {
					return nil, false, fmt.Errorf("extract containerd tarball: %w", err)
				}
				if err := executor.Run("cp", "-a", "--remove-destination", scratch+"/.", usrLocalRoot); err != nil {
					return nil, false, fmt.Errorf("install containerd files -> %s: %w", usrLocalRoot, err)
				}
			}
		} else {
			// containerd 1.x cri-containerd-cni bundle: unpack to a scratch dir on
			// writable storage, then copy each tree to its real location. The bundle's
			// kept members are binaries under usr/local/{bin,sbin}, the systemd unit
			// + crictl.yaml under etc/, and the (mostly-excluded) opt/containerd tree.
			scratch := "/srv/magnum/containerd-bundle"
			if err := executor.Run("rm", "-rf", scratch); err != nil {
				return nil, false, fmt.Errorf("clean containerd scratch dir: %w", err)
			}
			scratchDir, err := (hostresource.DirectorySpec{Path: scratch, Mode: 0o755}).Apply(executor)
			if err != nil {
				return nil, false, fmt.Errorf("create containerd scratch dir: %w", err)
			}
			changes = append(changes, scratchDir.Changes...)
			if err := executor.Run("tar", "xzf", localPath,
				"-C", scratch,
				"--no-same-owner", "--touch", "--no-same-permissions",
				"--exclude=etc/cni/net.d",
				"--exclude=etc/containerd/config.toml",
				"--exclude=opt/cni/bin",
				"--exclude=*.txt",
				"--exclude=opt/containerd/cluster/gce",
			); err != nil {
				return nil, false, fmt.Errorf("extract containerd tarball: %w", err)
			}
			// dst: FCoS /var/usrlocal == /usr/local, /var/opt == /opt; Ubuntu uses
			// /usr/local and /opt directly. /etc is writable on both.
			placements := []struct{ tree, dst string }{
				{"usr/local", usrLocalRoot},
				{"opt", optRoot},
				{"etc", "/etc"},
			}
			for _, p := range placements {
				if _, statErr := os.Stat(scratch + "/" + p.tree); statErr != nil {
					continue // tree absent after excludes
				}
				dstDir, err := (hostresource.DirectorySpec{Path: p.dst, Mode: 0o755}).Apply(executor)
				if err != nil {
					return nil, false, fmt.Errorf("ensure containerd install dir %s: %w", p.dst, err)
				}
				changes = append(changes, dstDir.Changes...)
				// Existing containerd shim binaries may still be mapped by live
				// processes after Docker/containerd is stopped. Unlink before copy so
				// the old inode can remain executable while the new file lands at path.
				if err := executor.Run("cp", "-a", "--remove-destination", scratch+"/"+p.tree+"/.", p.dst); err != nil {
					return nil, false, fmt.Errorf("install containerd files %s -> %s: %w", p.tree, p.dst, err)
				}
			}
			if err := executor.Run("rm", "-rf", scratch); err != nil {
				return nil, false, fmt.Errorf("remove containerd scratch dir: %w", err)
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
		// TimeoutStartSec: containerd.service is Type=notify, so `systemctl start`
		// waits for READY=1. On a small/loaded node, containerd's start-time
		// snapshot/image recovery can exceed systemd's 90s default and the job
		// fails ("Job for containerd.service failed because a timeout was
		// exceeded") even though it would have come up — fail the whole upgrade.
		// Give it generous headroom.
		dropin := "[Service]\nExecStart=\nExecStart=/usr/local/bin/containerd\nTimeoutStartSec=300\n"
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

// removeStrayCNIConfs deletes non-Kubernetes CNI configs that ship baked into
// the node image. The Ubuntu image carries podman's default
// /etc/cni/net.d/87-podman-bridge.conflist (a node-local 10.88.0.0/16 bridge).
// containerd's CRI plugin selects the lexicographically-first valid config in
// conf_dir, so during the boot window before the flannel DaemonSet writes
// 10-flannel.conflist, that podman config is the only one present and becomes
// the cluster's default CNI. Any pod scheduled in that window (CoreDNS,
// metrics-server, addons) lands on the node-local podman bridge and cannot be
// routed to from other nodes — breaking cross-node DNS and any pod that must
// reach the control plane. Removing it before kubelet starts (container-runtime
// is phase 3; kubelet comes up in phase 14) restores Fedora CoreOS behavior:
// /etc/cni/net.d stays empty until flannel lands, so pods wait Pending instead
// of being placed on the wrong network.
//
// Gated to Ubuntu: on Fedora CoreOS the heat-container-agent runs under podman
// and may legitimately use this bridge, so the file is left untouched there.
// strayCNIConfPaths are the non-Kubernetes CNI configs removed on Ubuntu nodes.
// Declared as a package var so tests can point it at a temp directory.
var strayCNIConfPaths = []string{
	"/etc/cni/net.d/87-podman-bridge.conflist",
	"/etc/cni/net.d/87-podman.conflist",
}

func removeStrayCNIConfs(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	if !cfg.IsUbuntu() {
		return nil, nil
	}
	var changes []host.Change
	for _, path := range strayCNIConfPaths {
		res, err := (hostresource.FileSpec{Path: path, Absent: true}).Apply(executor)
		if err != nil {
			return nil, fmt.Errorf("remove stray CNI config %s: %w", path, err)
		}
		changes = append(changes, res.Changes...)
	}
	return changes, nil
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

	sel := selectContainerd(cfg.Shared.KubeTag)
	containerdVersion := sel.version
	useV2Layout := sel.v2Layout

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
		tarballRes, err = hostsdk.RegisterDownloadSpec(ctx, name+"-tarball", hostresource.DownloadSpec{URL: sel.url, Path: "/srv/magnum/containerd.tar.gz", Mode: 0o644, Retries: 5}, childOpts...)
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
			dropinRes, err := hostsdk.RegisterFileSpec(ctx, name+"-containerd-dropin", hostresource.FileSpec{Path: "/etc/systemd/system/containerd.service.d/10-exec-start.conf", Content: []byte("[Service]\nExecStart=\nExecStart=/usr/local/bin/containerd\nTimeoutStartSec=300\n"), Mode: 0o644}, dropinOpts...)
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
