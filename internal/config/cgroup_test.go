package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCgroupDriverFromKubeletConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"systemd", "kind: KubeletConfiguration\ncgroupDriver: systemd\n", "systemd"},
		{"cgroupfs", "cgroupDriver: cgroupfs\nfoo: bar\n", "cgroupfs"},
		{"quoted", "cgroupDriver: \"systemd\"\n", "systemd"},
		{"indented-ignored", "  cgroupDriver: systemd\n", "systemd"},
		{"absent", "kind: KubeletConfiguration\nfoo: bar\n", ""},
		{"empty-value", "cgroupDriver:\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "kubelet-config.yaml")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := cgroupDriverFromKubeletConfig(p); got != tc.want {
				t.Fatalf("cgroupDriverFromKubeletConfig = %q, want %q", got, tc.want)
			}
		})
	}
	if got := cgroupDriverFromKubeletConfig("/no/such/file"); got != "" {
		t.Fatalf("missing file: got %q, want empty", got)
	}
}

func TestCgroupDriverFromContainerdConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"systemd", "        [plugins.runc.options]\n          SystemdCgroup = true\n", "systemd"},
		{"cgroupfs", "          SystemdCgroup = false\n", "cgroupfs"},
		{"absent", "version = 2\n[plugins]\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := cgroupDriverFromContainerdConfig(p); got != tc.want {
				t.Fatalf("cgroupDriverFromContainerdConfig = %q, want %q", got, tc.want)
			}
		})
	}
}
