package containerruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

func TestRemoveStrayCNIConfs(t *testing.T) {
	dir := t.TempDir()
	podman := filepath.Join(dir, "87-podman-bridge.conflist")
	flannel := filepath.Join(dir, "10-flannel.conflist")
	for _, p := range []string{podman, flannel} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	orig := strayCNIConfPaths
	strayCNIConfPaths = []string{podman}
	t.Cleanup(func() { strayCNIConfPaths = orig })

	executor := host.NewExecutor(true, nil)

	// FCoS: must NOT touch the podman conf (heat-container-agent may use it).
	changes, err := removeStrayCNIConfs(config.Config{Distro: "fedora-coreos"}, executor)
	if err != nil {
		t.Fatalf("fcos: unexpected error: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("fcos: expected no changes, got %d", len(changes))
	}
	if _, err := os.Stat(podman); err != nil {
		t.Fatalf("fcos: podman conf must remain, stat err: %v", err)
	}

	// Ubuntu: removes the podman conf, leaves flannel's untouched.
	changes, err = removeStrayCNIConfs(config.Config{Distro: "ubuntu"}, executor)
	if err != nil {
		t.Fatalf("ubuntu: unexpected error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("ubuntu: expected 1 change, got %d", len(changes))
	}
	if _, err := os.Stat(podman); !os.IsNotExist(err) {
		t.Fatalf("ubuntu: podman conf should be removed, stat err: %v", err)
	}
	if _, err := os.Stat(flannel); err != nil {
		t.Fatalf("ubuntu: flannel conf must remain, stat err: %v", err)
	}

	// Idempotent: second Ubuntu run is a no-op (file already gone).
	changes, err = removeStrayCNIConfs(config.Config{Distro: "ubuntu"}, executor)
	if err != nil {
		t.Fatalf("ubuntu rerun: unexpected error: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("ubuntu rerun: expected no changes, got %d", len(changes))
	}
}

func TestContainerdConfigUsesResolvedCgroupDriver(t *testing.T) {
	tests := []struct {
		name          string
		cgroupDriver  string
		wantSystemd   bool
		wantDirective string
	}{
		{
			name:          "systemd driver",
			cgroupDriver:  "systemd",
			wantSystemd:   true,
			wantDirective: "SystemdCgroup = true",
		},
		{
			name:          "cgroupfs driver",
			cgroupDriver:  "cgroupfs",
			wantSystemd:   false,
			wantDirective: "SystemdCgroup = false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containerdUsesSystemdCgroup(tt.cgroupDriver)
			if got != tt.wantSystemd {
				t.Fatalf("containerdUsesSystemdCgroup(%q) = %t, want %t", tt.cgroupDriver, got, tt.wantSystemd)
			}

			for _, cfg := range []string{
				containerdV2Config("registry.k8s.io/pause:3.10", got),
				containerdV3Config("registry.k8s.io/pause:3.10", got),
			} {
				if !strings.Contains(cfg, tt.wantDirective) {
					t.Fatalf("containerd config missing %q\n%s", tt.wantDirective, cfg)
				}
			}
		})
	}
}

func TestLegacyDockerUnitNeedsRemoval(t *testing.T) {
	t.Run("missing path", func(t *testing.T) {
		remove, err := legacyDockerUnitNeedsRemoval(filepath.Join(t.TempDir(), "docker.service"))
		if err != nil {
			t.Fatalf("legacyDockerUnitNeedsRemoval() error = %v", err)
		}
		if remove {
			t.Fatal("expected missing path to require no removal")
		}
	})

	t.Run("regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docker.service")
		if err := os.WriteFile(path, []byte("[Service]\nExecStart=/usr/bin/dockerd\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		remove, err := legacyDockerUnitNeedsRemoval(path)
		if err != nil {
			t.Fatalf("legacyDockerUnitNeedsRemoval() error = %v", err)
		}
		if !remove {
			t.Fatal("expected regular file to require removal")
		}
	})

	t.Run("masked symlink", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docker.service")
		if err := os.Symlink("/dev/null", path); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
		remove, err := legacyDockerUnitNeedsRemoval(path)
		if err != nil {
			t.Fatalf("legacyDockerUnitNeedsRemoval() error = %v", err)
		}
		if remove {
			t.Fatal("expected /dev/null symlink to be preserved")
		}
	})
}
