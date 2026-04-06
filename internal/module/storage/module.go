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
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "storage" }
func (Module) Dependencies() []string { return []string{"container-runtime"} }

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
	change, err := executor.EnsureDir(storageDir, 0o755)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Format only if not already xfs.
	fstype, _ := executor.RunCapture("blkid", "-s", "TYPE", "-o", "value", devicePath)
	if strings.TrimSpace(fstype) != "xfs" {
		if err := executor.Run("mkfs.xfs", "-f", devicePath); err != nil {
			return moduleapi.Result{}, fmt.Errorf("format storage volume: %w", err)
		}
		changes = append(changes, host.Change{Action: host.ActionCreate, Path: devicePath,
			Summary: fmt.Sprintf("format %s as xfs", devicePath)})
	}

	// Ensure fstab entry.
	fstabLine := fmt.Sprintf("%s %s xfs defaults 0 0", devicePath, storageDir)
	change, err = executor.EnsureLine("/etc/fstab", fstabLine, 0o644)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Mount only the target device — avoid mount -a which would mount all fstab entries.
	if !executor.IsMountpoint(storageDir) {
		if err := executor.Run("mount", storageDir); err != nil {
			return moduleapi.Result{}, err
		}
		changes = append(changes, host.Change{Action: host.ActionCreate, Path: storageDir,
			Summary: fmt.Sprintf("mount %s at %s", devicePath, storageDir)})
	}

	// Restore SELinux context only if we just formatted.
	if strings.TrimSpace(fstype) != "xfs" {
		_ = executor.Run("restorecon", "-R", storageDir)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"storageDir": storageDir, "devicePath": devicePath},
	}, nil
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

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Storage", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"dockerVolumeSize": pulumi.Int(heat.Cfg.Shared.DockerVolumeSize),
		"containerRuntime": pulumi.String(heat.Cfg.Shared.ContainerRuntime),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
