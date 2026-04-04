package kubeletconfig

import "testing"

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
