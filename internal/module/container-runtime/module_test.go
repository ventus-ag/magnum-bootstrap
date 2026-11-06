package containerruntime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

// Run with CONTAINERD_TEST_BINARY pointing to each supported containerd binary.
// Decode through containerd itself so incorrect plugin paths and incompatible
// drop-in versions cannot pass merely because the rendered TOML looks right.
func TestContainerdConfigWithRuntimeDropIn(t *testing.T) {
	binary := os.Getenv("CONTAINERD_TEST_BINARY")
	if binary == "" {
		t.Skip("set CONTAINERD_TEST_BINARY to validate with a real containerd binary")
	}
	out, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("containerd --version: %v\n%s", err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		t.Fatalf("unexpected containerd version output: %s", out)
	}
	version := strings.TrimPrefix(fields[2], "v")
	major, _, ok := parseContainerdMajor(version)
	if !ok {
		t.Fatalf("cannot parse containerd version %q", version)
	}
	out, err = exec.Command(binary, "config", "default").CombinedOutput()
	if err != nil {
		t.Fatalf("containerd config default: %v\n%s", err, out)
	}
	var versionLine string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "version = ") {
			versionLine = line
			break
		}
	}
	if versionLine == "" {
		t.Fatalf("containerd default config has no version:\n%s", out)
	}
	plugin := "io.containerd.grpc.v1.cri"
	if major >= 2 {
		plugin = "io.containerd.cri.v1.runtime"
	}
	for _, systemd := range []bool{false, true} {
		t.Run(fmt.Sprintf("systemd=%t", systemd), func(t *testing.T) {
			dir := t.TempDir()
			dropinDir := filepath.Join(dir, "conf.d")
			if err := os.Mkdir(dropinDir, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := containerdConfig(version, "registry.k8s.io/pause:3.10", systemd)
			cfg = strings.ReplaceAll(cfg, "/etc/containerd/conf.d/*.toml", filepath.Join(dropinDir, "*.toml"))
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			// The toolkit uses the binary's current config version in its drop-in.
			dropin := fmt.Sprintf(`%s
[plugins.%q.containerd.runtimes.nvidia]
  runtime_type = "io.containerd.runc.v2"
  [plugins.%q.containerd.runtimes.nvidia.options]
    BinaryName = "/usr/local/nvidia/toolkit/nvidia-container-runtime"
    SystemdCgroup = %t
`, versionLine, plugin, plugin, systemd)
			// Containerd 1.7 merges the CRI plugin as one map. Like the toolkit,
			// preserve its existing settings alongside the added runtime.
			if major < 2 {
				dropin = versionLine + "\n" + cfg[strings.Index(cfg, "[plugins]"):] + strings.TrimPrefix(dropin, versionLine+"\n")
			}
			if err := os.WriteFile(filepath.Join(dropinDir, "99-nvidia.toml"), []byte(dropin), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(binary, "--config", path, "config", "dump").CombinedOutput()
			if err != nil {
				t.Fatalf("containerd config dump: %v\n%s", err, out)
			}
			dump := string(out)
			if strings.Contains(dump, "Ignoring unknown key") {
				t.Fatalf("containerd ignored configuration keys:\n%s", dump)
			}
			for _, want := range []string{"runtimes.nvidia]", "registry.k8s.io/pause:3.10", "/opt/cni/bin", "/etc/cni/net.d"} {
				if !strings.Contains(dump, want) {
					t.Errorf("effective config missing %q\n%s", want, dump)
				}
			}
			wantCgroup := fmt.Sprintf("SystemdCgroup = %t", systemd)
			if strings.Count(dump, wantCgroup) != 2 || strings.Contains(dump, fmt.Sprintf("SystemdCgroup = %t", !systemd)) {
				t.Errorf("runc and NVIDIA must both use %q\n%s", wantCgroup, dump)
			}
		})
	}
}

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

			for _, version := range []string{"1.7.30", "2.1.9", "2.2.5", "2.3.2"} {
				cfg := containerdConfig(version, "registry.k8s.io/pause:3.10", got)
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
