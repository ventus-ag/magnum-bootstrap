package containerruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

func TestNVIDIACgroupOptionsFollowRuntimeWithoutChangingOtherSettings(t *testing.T) {
	for _, systemd := range []bool{false, true} {
		t.Run(fmt.Sprintf("systemd=%t", systemd), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "99-nvidia.toml")
			original := "version = 4\n# custom toolkit settings\n"
			for _, runtime := range []string{"runc", "nvidia", "nvidia-cdi", "nvidia-legacy"} {
				original += fmt.Sprintf("[plugins.\"io.containerd.cri.v1.runtime\".containerd.runtimes.%s.options]\n  BinaryName = \"/custom/%s\"\n  SystemdCgroup = %t # retain comment\n", runtime, runtime, !systemd)
			}
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			spec, err := nvidiaContainerdConfig(path, systemd)
			if err != nil {
				t.Fatal(err)
			}
			expected := strings.ReplaceAll(original, fmt.Sprintf("SystemdCgroup = %t", !systemd), fmt.Sprintf("SystemdCgroup = %t", systemd))
			if string(spec.Content) != expected {
				t.Fatalf("unrelated NVIDIA settings changed: %s", spec.Content)
			}
			dry, err := spec.Apply(host.NewExecutor(false, nil))
			if err != nil || !dry.Changed {
				t.Fatalf("dry run missed cgroup correction: %+v %v", dry, err)
			}
			before, _ := os.ReadFile(path)
			if string(before) != original {
				t.Fatal("dry run wrote NVIDIA configuration")
			}
			applied, err := spec.Apply(host.NewExecutor(true, nil))
			if err != nil || !applied.Changed {
				t.Fatalf("configuration not updated: %+v %v", applied, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("existing permissions were not preserved: %v", err)
			}
			spec, err = nvidiaContainerdConfig(path, systemd)
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := spec.Apply(host.NewExecutor(true, nil))
			if err != nil || repeated.Changed {
				t.Fatalf("unchanged settings trigger another update: %+v %v", repeated, err)
			}
		})
	}
}

func TestNVIDIACgroupConfigIsNotCreatedWithoutToolkit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "99-nvidia.toml")
	spec, err := nvidiaContainerdConfig(path, true)
	if err != nil || spec != nil {
		t.Fatalf("non-GPU node gets NVIDIA config: %+v %v", spec, err)
	}
}

func TestRuntimeUpdatesRestartKubeletToReloadEffectiveDriver(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, previous, desired  string
		changed, wantRuntime, wantKubelet bool
	}{
		{"corrected containerd config with unchanged kubelet YAML", "containerd", "systemd", "systemd", true, true, true},
		{"unchanged containerd", "containerd", "systemd", "systemd", false, false, false},
		{"Docker config update", "docker", "cgroupfs", "cgroupfs", true, true, false},
		{"Docker driver change", "docker", "cgroupfs", "systemd", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := moduleapi.NewRestartTracker()
			markRuntimeRestarts(tc.runtime, tc.changed, tc.previous, tc.desired, tracker)
			if tracker.NeedsRestart(tc.runtime) != tc.wantRuntime || tracker.NeedsRestart("kubelet") != tc.wantKubelet {
				t.Fatalf("incorrect restart requests: %v", tracker.All())
			}
		})
	}
}
