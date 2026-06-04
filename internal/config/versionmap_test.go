package config

import "testing"

func TestLookupByKubeVersionExactAndBoundary(t *testing.T) {
	// Dense map (contiguous minors) like the cluster-addon chart maps.
	dense := map[string]string{
		"1.28": "a",
		"1.29": "b",
		"1.30": "c",
		"1.31": "d",
	}
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{"exact mid", "v1.29.4", "b"},
		{"exact lowest", "1.28.0", "a"},
		{"exact highest", "v1.31.10", "d"},
		{"below lowest clamps up", "v1.20.0", "a"},
		{"above highest clamps down", "v1.40.0", "d"},
		{"no v prefix", "1.30.2", "c"},
		{"minor only", "1.30", "c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LookupByKubeVersion(dense, tt.version); got != tt.want {
				t.Fatalf("LookupByKubeVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

// TestLookupByKubeVersionFloorOnSparseMap pins the floor behavior that the
// sparse containerd map depends on: a version between two keys must resolve to
// the nearest LOWER key, not the lowest key overall. Before the floor fix, k8s
// 1.33/1.34 wrongly resolved to the 1.31 entry (containerd 1.7.x / v1 layout)
// instead of the 1.32 entry (containerd 2.x / v2 layout).
func TestLookupByKubeVersionFloorOnSparseMap(t *testing.T) {
	containerd := map[string]string{
		"1.31": "1.7.30",
		"1.32": "2.1.6",
		"1.35": "2.2.2",
	}
	tests := []struct {
		version string
		want    string
	}{
		{"v1.30.5", "1.7.30"}, // below lowest -> clamp up to lowest
		{"v1.31.4", "1.7.30"}, // exact
		{"v1.32.0", "2.1.6"},  // exact
		{"v1.33.0", "2.1.6"},  // between 1.32 and 1.35 -> floor 1.32
		{"v1.34.2", "2.1.6"},  // between 1.32 and 1.35 -> floor 1.32
		{"v1.35.1", "2.2.2"},  // exact
		{"v1.36.0", "2.2.2"},  // above highest -> clamp down to highest
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := LookupByKubeVersion(containerd, tt.version); got != tt.want {
				t.Fatalf("LookupByKubeVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestLookupByKubeVersionEmptyAndMalformed(t *testing.T) {
	if got := LookupByKubeVersion(map[string]string{}, "v1.30.0"); got != "" {
		t.Fatalf("empty map should return empty string, got %q", got)
	}
	// Malformed version (no minor) falls back to the lowest entry.
	m := map[string]string{"1.25": "lo", "1.30": "hi"}
	if got := LookupByKubeVersion(m, "garbage"); got != "lo" {
		t.Fatalf("malformed version should clamp to lowest, got %q", got)
	}
}
