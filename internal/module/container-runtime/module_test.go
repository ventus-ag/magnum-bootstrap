package containerruntime

import (
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
