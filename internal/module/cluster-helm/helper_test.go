package clusterhelm

import "testing"

func TestParseHelmReleasePair(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  HelmReleasePair
		ok    bool
	}{
		{
			name:  "valid pair",
			input: "kube-system/npd",
			want:  HelmReleasePair{Namespace: "kube-system", Name: "npd"},
			ok:    true,
		},
		{
			name:  "trims whitespace",
			input: "  kube-flannel/flannel  ",
			want:  HelmReleasePair{Namespace: "kube-flannel", Name: "flannel"},
			ok:    true,
		},
		{
			name:  "missing slash",
			input: "npd",
			ok:    false,
		},
		{
			name:  "missing name",
			input: "kube-system/",
			ok:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseHelmReleasePair(tt.input)
			if ok != tt.ok {
				t.Fatalf("parseHelmReleasePair() ok = %t, want %t", ok, tt.ok)
			}
			if got != tt.want {
				t.Fatalf("parseHelmReleasePair() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHasHelmNameReuseConflict(t *testing.T) {
	if !HasHelmNameReuseConflict("diag: cannot re-use a name that is still in use") {
		t.Fatal("expected name reuse conflict to be detected")
	}
	if HasHelmNameReuseConflict("diag: update failed") {
		t.Fatal("expected unrelated error to be ignored")
	}
}
