package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
		// A node wedged by a PRE-FIX relocation (volume mounted over the live
		// store, old pods orphaned) lands here on every later run — self-heal
		// it instead of leaving it for a manual reboot.
		healChanges := healOrphanShimWedge(executor, req, runtimeService)
		return moduleapi.Result{
			Changes: healChanges,
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

	// Adopting an OLD node whose runtime store lives on the ROOT disk: mounting
	// the dedicated volume here would silently shadow the live store under the
	// running workloads. The runtime then restarts onto an empty store while
	// every old pod keeps running as an orphan shim — invisible to CRI, holding
	// RWO volume mounts, CNI IPs and host ports — and no new pod can start
	// until the node is rebooted. Relocation is intrinsically disruptive (all
	// node-local pods are recreated), so do it as an orderly reboot-equivalent
	// instead of a wedge: stop kubelet+runtime, kill leftover shims, unmount
	// everything below the store, clear the old content (reclaims the root
	// disk), then mount and let the services phase bring everything back up.
	relocationNeeded := liveStoreRelocationNeeded(executor, storageDir, runtime)

	if !req.Apply {
		if relocationNeeded {
			changes = append(changes, host.Change{Action: host.ActionUpdate, Path: storageDir,
				Summary: fmt.Sprintf("relocate live %s store from root disk to dedicated volume (node-local pods will be recreated)", runtime)})
		}
		changes = append(changes, host.Change{Action: host.ActionCreate, Path: storageDir,
			Summary: fmt.Sprintf("format %s and mount at %s", devicePath, storageDir)})
		return moduleapi.Result{Changes: changes, Outputs: map[string]string{"storageDir": storageDir}}, nil
	}

	if relocationNeeded {
		rc, err := relocateLiveRuntimeStore(executor, req, storageDir, runtimeService)
		if err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, rc...)
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

	// Restore SELinux context if we just formatted, or after a relocation (an
	// old volume may carry docker-era labels that break containerd) — Fedora
	// CoreOS only (Ubuntu uses AppArmor; restorecon is absent / a no-op there).
	if cfg.IsFCoS() && (formatted || relocationNeeded) {
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

// liveStoreRelocationNeeded reports whether mounting the dedicated volume at
// storageDir would shadow a live (or leftover) runtime store on the root disk.
// A fresh node is not affected: the runtime may have pre-created the directory
// skeleton before the storage phase runs, but it holds no snapshots/containers
// and no shims, so nothing here triggers.
func liveStoreRelocationNeeded(executor *host.Executor, storageDir, runtime string) bool {
	if executor.IsMountpoint(storageDir) {
		return false
	}
	if runtimeStoreHasData(storageDir, runtime) {
		return true
	}
	// Leftover shims keep old pods alive (and their mounts/IPs/ports held)
	// even when the store content was already shadowed by a prior wedged run.
	return leftoverShimsRunning(executor)
}

// runtimeStoreHasData reports whether storageDir holds meaningful store
// content: actual snapshot/container entries, not the empty directory skeleton
// a just-started runtime creates.
func runtimeStoreHasData(storageDir, runtime string) bool {
	patterns := []string{
		filepath.Join(storageDir, "io.containerd.snapshotter.*", "snapshots", "*"),
	}
	if runtime != "containerd" {
		patterns = []string{
			filepath.Join(storageDir, "containers", "*"),
			filepath.Join(storageDir, "overlay2", "??*"),
		}
	}
	for _, p := range patterns {
		if m, _ := filepath.Glob(p); len(m) > 0 {
			return true
		}
	}
	return false
}

func leftoverShimsRunning(executor *host.Executor) bool {
	out, err := executor.RunCapture("pgrep", "-f", "containerd-shim")
	return err == nil && strings.TrimSpace(out) != ""
}

// relocateLiveRuntimeStore performs the orderly reboot-equivalent takedown
// before the dedicated volume is mounted over a live root-disk store:
// stop kubelet and the runtime, kill leftover shims (their containers die with
// them), unmount everything below the store and the runtime state dir, clear
// the old content and stale CNI allocations, and mark kubelet+runtime for
// restart so the services phase brings the node back like after a reboot.
func relocateLiveRuntimeStore(executor *host.Executor, req moduleapi.Request, storageDir, runtimeService string) ([]host.Change, error) {
	req.Logger.Warnf("storage: live %s store detected on root disk at %s — relocating to dedicated volume; node-local pods will be recreated", runtimeService, storageDir)
	var changes []host.Change

	// Stop consumers first. kubelet may not exist yet on a half-migrated node,
	// so its stop is best-effort — but the runtime MUST be down before the
	// store is cleared, else a live daemon keeps recreating content and holds
	// the bolt DB open mid-wipe.
	_ = executor.Run("systemctl", "stop", "kubelet")
	_ = executor.Run("systemctl", "stop", runtimeService)
	if runtimeService == "docker" {
		_ = executor.Run("systemctl", "stop", "docker.socket")
	}
	if executor.SystemctlIsActive(runtimeService) {
		return nil, fmt.Errorf("storage: %s still active after stop; refusing to relocate a live store", runtimeService)
	}

	// Take the pods down before mounting over their store. Politely first: a
	// stateful workload (e.g. a database) gets a SIGTERM and a grace window to
	// flush and shut down cleanly; a well-behaved container exits and its shim
	// exits with it. Only shims that ignore the polite stop are then force-killed.
	runtimeStateDir := "/run/containerd"
	if runtimeService == "docker" {
		runtimeStateDir = "/run/docker"
	}
	if err := stopLeftoverPodShims(executor, req, storageDir, runtimeStateDir); err != nil {
		return nil, err
	}
	changes = append(changes, host.Change{Action: host.ActionDelete, Path: storageDir,
		Summary: "gracefully stop pods, then kill leftover shims before store relocation"})

	// Container rootfs overlays (and sandbox shm mounts) linger after their
	// processes die; unmount deepest-first so the store content is removable.
	uc, leftover := unmountAllUnder(executor, storageDir)
	changes = append(changes, uc...)
	if len(leftover) > 0 {
		// A mount we cannot detach even lazily would make the clear below
		// fail with an opaque EBUSY — name the offender instead.
		return nil, fmt.Errorf("storage: mounts still busy under %s after unmount attempts: %s", storageDir, strings.Join(leftover, ", "))
	}
	uc, leftover = unmountAllUnder(executor, runtimeStateDir)
	changes = append(changes, uc...)
	if len(leftover) > 0 {
		// State-dir leftovers (a stuck shm) don't block the store swap.
		req.Logger.Warnf("storage: mounts still busy under %s (non-fatal): %s", runtimeStateDir, strings.Join(leftover, ", "))
	}

	// Clear the old store so it doesn't sit shadowed under the new mount as a
	// silent root-disk space leak, plus the runtime state dir (bundles there
	// reference the old snapshots) and stale CNI allocations (every sandbox
	// they belong to is gone; the plugins recreate the dirs on next use).
	if err := clearDirContents(storageDir); err != nil {
		return nil, fmt.Errorf("storage: clear old runtime store %s: %w", storageDir, err)
	}
	changes = append(changes, host.Change{Action: host.ActionDelete, Path: storageDir,
		Summary: "clear old root-disk runtime store before mounting dedicated volume"})
	if err := clearDirContents(runtimeStateDir); err != nil {
		req.Logger.Warnf("storage: clear runtime state dir %s (non-fatal): %v", runtimeStateDir, err)
	}
	_ = os.RemoveAll("/var/lib/cni")

	if req.Restarts != nil {
		req.Restarts.Add(runtimeService, "runtime store relocated to dedicated volume")
		req.Restarts.Add("kubelet", "runtime store relocated to dedicated volume")
	}
	return changes, nil
}

const (
	// defaultRelocateGracePeriod is how long a container gets to shut down after
	// SIGTERM before it is force-killed. Sized above the 30s Kubernetes default
	// terminationGracePeriodSeconds so a database has time to flush.
	defaultRelocateGracePeriod = 90 * time.Second
	// defaultRelocateForceWait bounds the SIGKILL phase. The old value was a
	// fixed 15s, which tripped the guard on shims that were merely slow to die
	// (dozens of pods plus their CSI/overlay mounts tearing down on a real node).
	defaultRelocateForceWait = 120 * time.Second

	relocateGraceEnv = "MAGNUM_STORAGE_RELOCATE_GRACE_SECONDS"
	relocateForceEnv = "MAGNUM_STORAGE_RELOCATE_FORCE_SECONDS"
)

// stopLeftoverPodShims takes down the pods still running on the root-disk store
// before the dedicated volume is mounted over it. It escalates:
//  1. polite — SIGTERM each container's init process and wait the grace window,
//     so a stateful workload flushes and exits cleanly (its shim exits with it);
//  2. force — SIGKILL any shim that ignored the polite stop, re-issuing the kill
//     periodically because the container-runtime phase restarts containerd right
//     before storage runs and it re-adopts the old shims, racing a one-shot kill;
//  3. last resort — a shim stuck in uninterruptible sleep (D-state) on an
//     overlay/CSI mount cannot be reaped until that I/O unblocks, so lazy-unmount
//     the store and runtime state dir to release it, then try once more.
//
// It returns an error only if shims genuinely refuse to die, which on a real
// node means a hung mount backend that only a reboot clears.
func stopLeftoverPodShims(executor *host.Executor, req moduleapi.Request, storageDir, runtimeStateDir string) error {
	grace := resolveRelocateDuration(relocateGraceEnv, defaultRelocateGracePeriod)
	force := resolveRelocateDuration(relocateForceEnv, defaultRelocateForceWait)

	// 1. Polite: SIGTERM the container inits (children of the shims), not the
	// shims themselves — signalling PID 1 inside each container is what lets the
	// workload shut down cleanly. podman/conmon system units (kube-proxy,
	// heat-container-agent carrying the Heat signal) are not containerd shims and
	// are left untouched.
	if inits := containerInitPIDs(executor); len(inits) > 0 && grace > 0 {
		req.Logger.Infof("storage: SIGTERM %d container workload(s) before relocation, grace %s", len(inits), grace)
		for _, pid := range inits {
			_ = executor.Run("kill", "-TERM", pid)
		}
		pollUntil(grace, func() bool { return !leftoverShimsRunning(executor) })
	}
	if !leftoverShimsRunning(executor) {
		return nil
	}

	// 2. Force: SIGKILL, re-issuing every 10s to defeat the re-adopt race.
	req.Logger.Warnf("storage: shims still present after grace; force-killing (timeout %s)", force)
	kill := func() { _ = executor.Run("pkill", "-9", "-f", "containerd-shim") }
	kill()
	if pollUntilTick(force, 10*time.Second, func() bool { return !leftoverShimsRunning(executor) }, kill) {
		return nil
	}

	// 3. Last resort: release a D-state shim by detaching the mounts it is stuck
	// on (these unmounts also run on the success path below — harmless to repeat).
	req.Logger.Warnf("storage: shims survived SIGKILL; lazy-unmounting %s to release stuck mounts", storageDir)
	_, _ = unmountAllUnder(executor, storageDir)
	_, _ = unmountAllUnder(executor, runtimeStateDir)
	kill()
	if pollUntil(10*time.Second, func() bool { return !leftoverShimsRunning(executor) }) {
		return nil
	}
	return fmt.Errorf("storage: leftover containerd-shim processes survived SIGTERM+SIGKILL and mount release; a container is wedged on a hung mount — reboot the node to clear it")
}

// containerInitPIDs returns the PID of every container's init process — the
// direct child of each leftover containerd shim. These, not the shims, are what
// get SIGTERM so the in-container workload can shut down cleanly.
func containerInitPIDs(executor *host.Executor) []string {
	out, err := executor.RunCapture("pgrep", "-f", "containerd-shim")
	if err != nil {
		return nil
	}
	var pids []string
	for _, shim := range strings.Fields(out) {
		cout, cerr := executor.RunCapture("pgrep", "-P", shim)
		if cerr != nil {
			continue
		}
		pids = append(pids, strings.Fields(cout)...)
	}
	return pids
}

// resolveRelocateDuration reads env (seconds, non-negative int) or falls back to
// def. Garbage or a negative value uses def; 0 means "skip that wait".
func resolveRelocateDuration(env string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(env))
	if v == "" {
		return def
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return def
	}
	return time.Duration(secs) * time.Second
}

// pollUntil polls done every 500ms until it returns true or timeout elapses,
// reporting whether the condition was met. A zero/negative timeout checks once.
func pollUntil(timeout time.Duration, done func() bool) bool {
	return pollUntilTick(timeout, 0, done, nil)
}

// pollUntilTick is pollUntil that additionally invokes tick every tickEvery
// while it waits (used to re-issue a kill). A zero tickEvery disables ticking.
func pollUntilTick(timeout, tickEvery time.Duration, done func() bool, tick func()) bool {
	deadline := time.Now().Add(timeout)
	nextTick := time.Now().Add(tickEvery)
	for {
		if done() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		if tick != nil && tickEvery > 0 && !time.Now().Before(nextTick) {
			tick()
			nextTick = time.Now().Add(tickEvery)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// unmountAllUnder unmounts every mountpoint at or below prefix, deepest first,
// falling back to a lazy detach when a plain unmount is refused. It returns
// the recorded changes plus any mountpoints that survived both attempts.
func unmountAllUnder(executor *host.Executor, prefix string) ([]host.Change, []string) {
	mps := mountpointsUnder("/proc/self/mounts", prefix)
	var changes []host.Change
	var leftover []string
	for _, mp := range mps {
		if err := executor.Run("umount", mp); err != nil {
			if lerr := executor.Run("umount", "-l", mp); lerr != nil {
				leftover = append(leftover, mp)
				continue
			}
		}
		changes = append(changes, host.Change{Action: host.ActionDelete, Path: mp,
			Summary: "unmount leftover container mount under relocated store"})
	}
	return changes, leftover
}

// mountpointsUnder parses a mounts file (/proc/self/mounts format) and returns
// the mountpoints at or below prefix, deepest first.
func mountpointsUnder(mountsFile, prefix string) []string {
	data, err := os.ReadFile(mountsFile)
	if err != nil {
		return nil
	}
	var mps []string
	for _, ln := range strings.Split(string(data), "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		mp := strings.ReplaceAll(fields[1], "\\040", " ")
		if mp == prefix || strings.HasPrefix(mp, prefix+"/") {
			mps = append(mps, mp)
		}
	}
	sort.Slice(mps, func(i, j int) bool { return len(mps[i]) > len(mps[j]) })
	return mps
}

// clearDirContents removes everything inside dir but keeps dir itself (it may
// be referenced by systemd units or become the mountpoint right after).
func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
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
