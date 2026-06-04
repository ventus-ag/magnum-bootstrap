package kubeletconfig

import (
	"strings"
	"testing"
)

func TestFeatureGatesYAML(t *testing.T) {
	tests := []struct {
		name    string
		kubeTag string
		want    string
	}{
		{
			name:    "pre threshold version leaves block empty",
			kubeTag: "v1.21.14",
			want:    "",
		},
		{
			name:    "supported legacy versions disable graceful node shutdown",
			kubeTag: "v1.32.0",
			want:    "featureGates:\n  GracefulNodeShutdown: false\n",
		},
		{
			name:    "1.35 omits incompatible override",
			kubeTag: "v1.35.1",
			want:    "",
		},
		{
			name:    "unparseable version is treated conservatively",
			kubeTag: "devel",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FeatureGatesYAML(tt.kubeTag); got != tt.want {
				t.Fatalf("FeatureGatesYAML(%q) = %q, want %q", tt.kubeTag, got, tt.want)
			}
		})
	}
}

func TestParseKubeMajorMinor(t *testing.T) {
	tests := []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"v1.30.5", 1, 30, true},
		{"1.32.0", 1, 32, true},
		{"v1.36", 1, 36, true},
		{"garbage", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		maj, min, ok := ParseKubeMajorMinor(tt.in)
		if maj != tt.maj || min != tt.min || ok != tt.ok {
			t.Errorf("ParseKubeMajorMinor(%q) = (%d,%d,%t), want (%d,%d,%t)", tt.in, maj, min, ok, tt.maj, tt.min, tt.ok)
		}
	}
}

func TestKubeMinorAtLeast(t *testing.T) {
	if !KubeMinorAtLeast("v1.32.0", 32) {
		t.Error("1.32 >= 32 should be true")
	}
	if KubeMinorAtLeast("v1.31.9", 32) {
		t.Error("1.31 >= 32 should be false")
	}
	if KubeMinorAtLeast("bad", 1) {
		t.Error("unparseable tag should be false")
	}
}

// TestFeatureGatesGracefulShutdownWindow pins the kubelet panic guard across the
// whole supported range: the GracefulNodeShutdown=false override is emitted only
// for 1.22–1.34 and dropped at 1.35+ (where dependent gates default on and the
// override crashes kubelet).
func TestFeatureGatesGracefulShutdownWindow(t *testing.T) {
	for minor := 20; minor <= 36; minor++ {
		tag := "v1." + decimalString(minor) + ".0"
		got := FeatureGatesYAML(tag)
		wantOverride := minor > 21 && minor < 35
		hasOverride := strings.Contains(got, "GracefulNodeShutdown: false")
		if hasOverride != wantOverride {
			t.Errorf("%s: override present=%t, want %t (yaml=%q)", tag, hasOverride, wantOverride, got)
		}
	}
}

func decimalString(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
