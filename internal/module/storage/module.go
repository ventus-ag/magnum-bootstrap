package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "storage" }

// RetryPolicy opts storage out of the default per-module retry: its failure mode
// is a ~30min Cinder device-attach wait that is deterministic (a missing volume
// will not appear within a second 30min wait), so retrying only doubles the time
// before a doomed run fails to Heat.
func (Module) RetryPolicy() moduleapi.RetryPolicy { return moduleapi.RetryPolicy{MaxAttempts: 1} }

func (Module) Dependencies() []string {
	return []string{"container-runtime", "stop-services"}
}

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if cfg.Shared.DockerVolumeSize <= 0 {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	runtime := cfg.Shared.ContainerRuntime
	storageDir := "/var/lib/containerd"
	if runtime != "containerd" {
		storageDir = "/var/lib/docker"
	}

	// Migrate a legacy docker-runtime volume mount to the containerd path. Old
	// host-docker clusters mounted the dedicated volume at /var/lib/docker; once
	// the runtime flips to containerd (config normalization for k8s >= 1.24) the
	// same device must serve /var/lib/containerd instead. A leftover
	// /var/lib/docker mount of the *same* device keeps it busy (Pulumi cannot
	// reclaim the dir → "unlinkat /var/lib/docker: device or resource busy") and
	// leaves a duplicate fstab entry that double-mounts on boot. Unmount it and
	// drop its fstab line before the containerd mount below claims the device.
	// This NEVER deletes content — only an unmount + empty-dir rmdir — because
	// /var/lib/docker and /var/lib/containerd are frequently the same filesystem.
	if req.Apply && runtime == "containerd" {
		mc, err := migrateLegacyDockerMount(executor)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, mc...)
	}

	// If container runtime is already running and storage is mounted, nothing to do.
	runtimeService := "containerd"
	if runtime != "containerd" {
		runtimeService = "docker"
	}
	if executor.SystemctlIsActive(runtimeService) && executor.IsMountpoint(storageDir) {
		return moduleapi.Result{
			Outputs: map[string]string{"storageDir": storageDir, "status": "already-mounted"},
		}, nil
	}
	// Also skip if just mounted (runtime might not be started yet).
	if executor.IsMountpoint(storageDir) {
		return moduleapi.Result{
			Outputs: map[string]string{"storageDir": storageDir, "status": "already-mounted"},
		}, nil
	}

	// Find the device path.
	devicePath, err := findDevicePath(cfg, executor, req.Logger)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if devicePath == "" {
		return moduleapi.Result{}, fmt.Errorf("storage: no device found for volume %s", cfg.Shared.DockerVolume)
	}

	if !req.Apply {
		changes = append(changes, host.Change{Action: host.ActionCreate, Path: storageDir,
			Summary: fmt.Sprintf("format %s and mount at %s", devicePath, storageDir)})
		return moduleapi.Result{Changes: changes, Outputs: map[string]string{"storageDir": storageDir}}, nil
	}

	// Ensure storage directory exists.
	dirResult, err := (hostresource.DirectorySpec{Path: storageDir, Mode: 0o755}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, dirResult.Changes...)

	// Format ONLY a genuinely blank device. A populated volume — xfs from a
	// prior run, or ext4/other from an older (e.g. Ussuri) cluster — must NEVER
	// be reformatted: /var/lib/containerd holds the live container image store,
	// and wiping it leaves containerd's metadata referencing missing content
	// ("blob not found" on every new pod sandbox; only already-running
	// containers survive, until they restart).
	//
	// The previous guard `blkid TYPE != "xfs"` was unsafe: blkid's error was
	// ignored, so a transient failure (empty output) OR a perfectly good
	// non-xfs volume both fell through to a destructive `mkfs.xfs -f`. `lsblk
	// FSTYPE` reliably reports the on-disk filesystem (empty == truly blank),
	// regardless of mount state.
	fstype, _ := executor.RunCapture("lsblk", "-ndo", "FSTYPE", devicePath)
	fstype = strings.TrimSpace(fstype)
	formatted := false
	if fstype == "" {
		// No filesystem signature → safe to create one. No `-f`: let mkfs
		// refuse if it detects a signature lsblk somehow missed (defence in
		// depth — fail loudly rather than destroy data).
		if err := executor.Run("mkfs.xfs", devicePath); err != nil {
			return moduleapi.Result{}, fmt.Errorf("format blank storage volume %s: %w", devicePath, err)
		}
		fstype = "xfs"
		formatted = true
		changes = append(changes, host.Change{Action: host.ActionCreate, Path: devicePath,
			Summary: fmt.Sprintf("format blank %s as xfs", devicePath)})
	}

	// Ensure fstab entry. Use the actual on-disk fstype (an older cluster's
	// volume may be ext4), not a hardcoded "xfs" that would fail to mount it.
	fstabLine := fmt.Sprintf("%s %s %s defaults 0 0", devicePath, storageDir, fstype)
	lineResult, err := (hostresource.LineSpec{Path: "/etc/fstab", Line: fstabLine, Mode: 0o644}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, lineResult.Changes...)

	// Mount only the target device — avoid mount -a which would mount all fstab entries.
	if !executor.IsMountpoint(storageDir) {
		if err := executor.Run("mount", storageDir); err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, host.Change{Action: host.ActionCreate, Path: storageDir,
			Summary: fmt.Sprintf("mount %s at %s", devicePath, storageDir)})
	}

	// Restore SELinux context only if we just formatted — Fedora CoreOS only
	// (Ubuntu uses AppArmor; restorecon is absent / a no-op there).
	if cfg.IsFCoS() && formatted {
		_ = executor.Run("restorecon", "-R", storageDir)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"storageDir": storageDir, "devicePath": devicePath},
	}, nil
}

// migrateLegacyDockerMount removes a stale /var/lib/docker mount left behind by
// a host-docker cluster after the runtime has flipped to containerd. It is
// deliberately non-destructive: it unmounts the legacy path and removes the
// (now empty) mountpoint directory, but NEVER deletes filesystem content —
// /var/lib/docker and /var/lib/containerd are frequently the same device, so an
// rm there would wipe the live containerd store.
func migrateLegacyDockerMount(executor *host.Executor) ([]host.Change, error) {
	const legacy = "/var/lib/docker"
	var changes []host.Change

	if executor.IsMountpoint(legacy) {
		if err := executor.Run("umount", legacy); err != nil {
			// A busy/double mount may need a lazy detach; it still frees the path.
			if lerr := executor.Run("umount", "-l", legacy); lerr != nil {
				return nil, fmt.Errorf("unmount legacy docker volume %s: %w (lazy: %v)", legacy, err, lerr)
			}
		}
		changes = append(changes, host.Change{Action: host.ActionDelete, Path: legacy,
			Summary: "unmount legacy docker-runtime volume"})
	}

	fc, err := removeFstabMount(executor, legacy)
	if err != nil {
		return nil, err
	}
	if fc != nil {
		changes = append(changes, *fc)
	}

	// Remove the empty mountpoint dir so a Pulumi path-flip replace is a no-op.
	// os.Remove only succeeds on an empty dir; if anything is there (a filesystem
	// still mounted, or leftover files) it fails and we leave it untouched.
	if !executor.IsMountpoint(legacy) {
		if err := os.Remove(legacy); err == nil {
			changes = append(changes, host.Change{Action: host.ActionDelete, Path: legacy,
				Summary: "remove empty legacy docker mountpoint dir"})
		}
	}
	return changes, nil
}

// removeFstabMount strips any /etc/fstab line whose mountpoint field equals
// mountPoint, returning a Change when the file was actually rewritten.
func removeFstabMount(executor *host.Executor, mountPoint string) (*host.Change, error) {
	const fstab = "/etc/fstab"
	data, err := os.ReadFile(fstab)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	removed := false
	for _, ln := range lines {
		fields := strings.Fields(ln)
		if len(fields) >= 2 && fields[1] == mountPoint {
			removed = true
			continue
		}
		kept = append(kept, ln)
	}
	if !removed {
		return nil, nil
	}
	res, err := (hostresource.FileSpec{Path: fstab, Content: []byte(strings.Join(kept, "\n")), Mode: 0o644}).Apply(executor)
	if err != nil {
		return nil, err
	}
	if !res.Changed {
		return nil, nil
	}
	return &host.Change{Action: host.ActionUpdate, Path: fstab,
		Summary: "remove legacy " + mountPoint + " fstab entry"}, nil
}

func findDevicePath(cfg config.Config, executor *host.Executor, logger *logging.Logger) (string, error) {
	if !cfg.Shared.EnableCinder {
		path, err := filepath.EvalSymlinks("/dev/disk/by-label/ephemeral0")
		if err != nil {
			return "", nil
		}
		return path, nil
	}

	volume := cfg.Shared.DockerVolume
	if volume == "" {
		return "", nil
	}

	// Try matching with the first 20 chars (bash compat: grep ${VOL:0:20}$)
	// and also with the full volume ID (newer libvirt uses full UUIDs).
	shortID := volume
	if len(shortID) > 20 {
		shortID = shortID[:20]
	}

	// Wait up to 60 attempts (30 seconds) for the device to appear,
	// matching bash behavior for Cinder volume attachment.
	for attempt := 0; attempt < 60; attempt++ {
		entries, err := os.ReadDir("/dev/disk/by-id")
		if err == nil {
			for _, entry := range entries {
				name := entry.Name()
				// Match either truncated suffix (bash compat) or full UUID substring.
				if strings.HasSuffix(name, shortID) || strings.Contains(name, volume) {
					return "/dev/disk/by-id/" + name, nil
				}
			}
		}
		if logger != nil {
			logger.Infof("storage: waiting for volume %s device (attempt %d/60)", volume, attempt+1)
		}
		_ = executor.Run("udevadm", "trigger")
		time.Sleep(500 * time.Millisecond)
	}

	return "", fmt.Errorf("storage: disk device for volume %s did not appear after 30s", volume)
}

// Destroy unmounts the storage directory.
func (Module) Destroy(_ context.Context, cfg config.Config, req moduleapi.Request) error {
	executor := host.NewExecutor(req.Apply, req.Logger)

	storageDir := "/var/lib/containerd"
	if cfg.Shared.ContainerRuntime != "containerd" {
		storageDir = "/var/lib/docker"
	}

	if req.Logger != nil {
		req.Logger.Infof("storage destroy: unmounting %s", storageDir)
	}
	_ = executor.Run("umount", storageDir)

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Storage", name, res, opts...); err != nil {
		return nil, err
	}
	if heat.Cfg.Shared.DockerVolumeSize > 0 {
		childOpts := append(opts, pulumi.Parent(res))
		storageDir := storageDirForRuntime(heat.Cfg)
		// RetainOnDelete: the storage dir is a live mountpoint. When the runtime
		// flips (host-docker→containerd) its path changes /var/lib/docker →
		// /var/lib/containerd, which Pulumi treats as a replace (delete old +
		// create new). Deleting a mounted directory fails with "device or resource
		// busy" and wedges the whole update. Retaining on delete makes the path
		// flip a state-only swap; the imperative migrateLegacyDockerMount in Run()
		// handles the actual unmount/cleanup.
		dirOpts := append([]pulumi.ResourceOption{}, childOpts...)
		dirOpts = append(dirOpts, pulumi.RetainOnDelete(true))
		dirRes, err := hostsdk.RegisterDirectorySpec(ctx, name+"-dir", hostresource.DirectorySpec{Path: storageDir, Mode: 0o755}, dirOpts...)
		if err != nil {
			return nil, err
		}
		executor := host.NewExecutor(false, nil)
		devicePath, err := findDevicePath(heat.Cfg, executor, nil)
		if err == nil && devicePath != "" {
			// Register the fstab line with the real on-disk fstype, exactly as
			// Run() writes it. A hardcoded "xfs" on an ext4 legacy volume would
			// add a second, conflicting fstab entry for the same mountpoint —
			// systemd takes the last one and the mount fails at boot. A blank
			// (not yet formatted) device is skipped: Run() formats it and writes
			// the line; registration catches up on the next reconcile.
			if fstype := executor.BlockDeviceFstype(devicePath); fstype != "" {
				fstabLine := fmt.Sprintf("%s %s %s defaults 0 0", devicePath, storageDir, fstype)
				fstabOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, dirRes)
				if _, err := hostsdk.RegisterLineSpec(ctx, name+"-fstab", hostresource.LineSpec{Path: "/etc/fstab", Line: fstabLine, Mode: 0o644}, fstabOpts...); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"dockerVolumeSize": pulumi.Int(heat.Cfg.Shared.DockerVolumeSize),
		"containerRuntime": pulumi.String(heat.Cfg.Shared.ContainerRuntime),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func storageDirForRuntime(cfg config.Config) string {
	if cfg.Shared.ContainerRuntime == "containerd" {
		return "/var/lib/containerd"
	}
	return "/var/lib/docker"
}
