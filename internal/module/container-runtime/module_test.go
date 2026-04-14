package containerruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
