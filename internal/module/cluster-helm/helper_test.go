package clusterhelm

import (
	"os"
	"testing"
)

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

func TestParseHelmNoDeployedReleases(t *testing.T) {
	errMsg := `code: 1
 ~  kubernetes:helm.sh/v3:Release node-e2e-master-0-cluster-coredns-chart updating (2s) [diff: ~forceUpdate]; error: "coredns" has no deployed releases`

	names := ParseHelmNoDeployedReleases(errMsg)
	if len(names) != 1 || names[0] != "coredns" {
		t.Fatalf("ParseHelmNoDeployedReleases() = %v, want [coredns]", names)
	}

	// Multiple releases, deduped.
	multi := `"coredns" has no deployed releases` + "\n" + `"flannel" has no deployed releases` + "\n" + `"coredns" has no deployed releases`
	names = ParseHelmNoDeployedReleases(multi)
	if len(names) != 2 {
		t.Fatalf("ParseHelmNoDeployedReleases() returned %d names, want 2: %v", len(names), names)
	}

	if got := ParseHelmNoDeployedReleases("error: update failed"); got != nil {
		t.Fatalf("ParseHelmNoDeployedReleases() on unrelated error = %v, want nil", got)
	}
}

func TestParseHelmRemovedAPIFailures(t *testing.T) {
	// Real error: cluster upgraded 1.23 -> 1.28 (crosses 1.25, which removed
	// policy/v1beta1 PodDisruptionBudget); the deployed autoscaler manifest is
	// unbuildable on the new cluster.
	errMsg := `error: 1 error occurred:
	* Helm release "kube-system/openstack-autoscaler" failed to initialize completely. ` +
		`Error: Helm Release kube-system/openstack-autoscaler: unable to build kubernetes objects ` +
		`from current release manifest: resource mapping not found for name: "openstack-autoscaler-manager" ` +
		`namespace: "" from "": no matches for kind "PodDisruptionBudget" in version "policy/v1beta1"`

	got := ParseHelmRemovedAPIFailures(errMsg)
	if len(got) != 1 || got[0].Namespace != "kube-system" || got[0].Name != "openstack-autoscaler" {
		t.Fatalf("ParseHelmRemovedAPIFailures() = %+v, want [{kube-system openstack-autoscaler}]", got)
	}

	// Unrelated helm errors must not match (so we don't uninstall on a transient).
	for _, other := range []string{
		`"coredns" has no deployed releases`,
		`error: update failed`,
		`Helm Release kube-system/openstack-ccm: timed out waiting for the condition`,
	} {
		if got := ParseHelmRemovedAPIFailures(other); got != nil {
			t.Errorf("ParseHelmRemovedAPIFailures(%q) = %v, want nil", other, got)
		}
	}
}

func TestManagedReleaseByName(t *testing.T) {
	oldRoot := helmMarkerRootDir
	helmMarkerRootDir = t.TempDir()
	defer func() { helmMarkerRootDir = oldRoot }()

	MarkManaged("coredns", "kube-system")

	rel, ok := ManagedReleaseByName("coredns")
	if !ok || rel.Namespace != "kube-system" || rel.Name != "coredns" {
		t.Fatalf("ManagedReleaseByName(coredns) = %+v, %t; want {kube-system coredns}, true", rel, ok)
	}
	if _, ok := ManagedReleaseByName("nonexistent"); ok {
		t.Fatal("ManagedReleaseByName(nonexistent) returned ok=true, want false")
	}
}

func TestParseHelmOwnershipConflicts(t *testing.T) {
	errMsg := `error: Unable to continue with update: ServiceAccount "cloud-controller-manager" in namespace "kube-system" exists and cannot be imported into the current release: invalid ownership metadata; label validation error: missing key "app.kubernetes.io/managed-by": must be set to "Helm"; annotation validation error: missing key "meta.helm.sh/release-name": must be set to "openstack-ccm"; annotation validation error: missing key "meta.helm.sh/release-namespace": must be set to "kube-system"`

	conflicts := ParseHelmOwnershipConflicts(errMsg)
	if len(conflicts) != 1 {
		t.Fatalf("ParseHelmOwnershipConflicts() returned %d conflicts, want 1", len(conflicts))
	}
	if got := conflicts[0]; got != (HelmOwnershipConflict{
		ReleaseNamespace:  "kube-system",
		ReleaseName:       "openstack-ccm",
		ResourceKind:      "ServiceAccount",
		ResourceNamespace: "kube-system",
		ResourceName:      "cloud-controller-manager",
	}) {
		t.Fatalf("unexpected ownership conflict parsed: %+v", got)
	}
}

// TestParseHelmOwnershipConflictsCrossRelease covers Helm's "wrong owner"
// phrasing (must equal / current value is) where a resource is already owned by
// a DIFFERENT release and only the release-name differs (namespace matches, so
// no namespace clause). The original regex matched neither this phrasing nor a
// single-field conflict, so the auto-repair never fired.
func TestParseHelmOwnershipConflictsCrossRelease(t *testing.T) {
	errMsg := `error: Unable to continue with install: ClusterRole "system:metrics-server-aggregated-reader" in namespace "" exists and cannot be imported into the current release: invalid ownership metadata; annotation validation error: key "meta.helm.sh/release-name" must equal "metrics-server": current value is "coredns"`

	conflicts := ParseHelmOwnershipConflicts(errMsg)
	if len(conflicts) != 1 {
		t.Fatalf("ParseHelmOwnershipConflicts() returned %d conflicts, want 1", len(conflicts))
	}
	if got := conflicts[0]; got != (HelmOwnershipConflict{
		ReleaseNamespace:  "", // not in the error (namespace already correct) — repair must not clobber it
		ReleaseName:       "metrics-server",
		ResourceKind:      "ClusterRole",
		ResourceNamespace: "",
		ResourceName:      "system:metrics-server-aggregated-reader",
	}) {
		t.Fatalf("unexpected cross-release conflict parsed: %+v", got)
	}
}

// TestParseHelmOwnershipConflictsMultiple ensures each resource block is scoped
// to its own validation errors (the required release-name is read from the same
// block as the header, not bled across resources).
func TestParseHelmOwnershipConflictsMultiple(t *testing.T) {
	errMsg := `Unable to continue with install: ` +
		`ClusterRole "a" in namespace "" exists and cannot be imported into the current release: invalid ownership metadata; annotation validation error: key "meta.helm.sh/release-name" must equal "metrics-server": current value is "coredns" ` +
		`ServiceAccount "sa" in namespace "kube-system" exists and cannot be imported into the current release: invalid ownership metadata; label validation error: missing key "app.kubernetes.io/managed-by": must be set to "Helm"; annotation validation error: missing key "meta.helm.sh/release-name": must be set to "npd"; annotation validation error: missing key "meta.helm.sh/release-namespace": must be set to "kube-system"`

	conflicts := ParseHelmOwnershipConflicts(errMsg)
	if len(conflicts) != 2 {
		t.Fatalf("got %d conflicts, want 2: %+v", len(conflicts), conflicts)
	}
	if conflicts[0].ReleaseName != "metrics-server" || conflicts[0].ResourceName != "a" || conflicts[0].ReleaseNamespace != "" {
		t.Errorf("conflict[0] wrong: %+v", conflicts[0])
	}
	if conflicts[1].ReleaseName != "npd" || conflicts[1].ReleaseNamespace != "kube-system" || conflicts[1].ResourceName != "sa" {
		t.Errorf("conflict[1] wrong: %+v", conflicts[1])
	}
}

func TestPromoteManagedReleasesMarksAdopted(t *testing.T) {
	oldRoot := helmMarkerRootDir
	helmMarkerRootDir = t.TempDir()
	defer func() { helmMarkerRootDir = oldRoot }()

	cleanup := []string{
		managedMarkerPath("kube-flannel", "flannel"),
		adoptedMarkerPath("kube-flannel", "flannel"),
	}
	for _, path := range cleanup {
		defer func(path string) {
			_ = removeIfExists(path)
		}(path)
	}

	MarkManaged("flannel", "kube-flannel")
	PromoteManagedReleases()

	if _, err := os.Stat(adoptedMarkerPath("kube-flannel", "flannel")); err != nil {
		t.Fatalf("expected adopted marker after promoting managed release: %v", err)
	}
}

func TestPromoteManagedReleasesClearsImportMarkerAfterSuccessfulImport(t *testing.T) {
	oldRoot := helmMarkerRootDir
	helmMarkerRootDir = t.TempDir()
	defer func() { helmMarkerRootDir = oldRoot }()

	MarkManaged("coredns", "kube-system")
	if err := os.WriteFile(importMarkerPath("kube-system", "coredns"), []byte("kube-system/coredns"), 0o644); err != nil {
		t.Fatalf("write import marker: %v", err)
	}

	PromoteManagedReleases()

	if _, err := os.Stat(adoptedMarkerPath("kube-system", "coredns")); err != nil {
		t.Fatalf("expected adopted marker after promoting managed imported release: %v", err)
	}
	if _, err := os.Stat(importMarkerPath("kube-system", "coredns")); !os.IsNotExist(err) {
		t.Fatalf("expected import marker to be removed after promoting managed imported release, got: %v", err)
	}
}

func removeIfExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return os.Remove(path)
}

func TestParseHelmPendingOperations(t *testing.T) {
	msg := `error: update failed: 1 error occurred: Helm Release kube-system/coredns: another operation (install/upgrade/rollback) is in progress`
	pairs, matched := ParseHelmPendingOperations(msg)
	if !matched {
		t.Fatal("expected pending-operation match")
	}
	if len(pairs) != 1 || pairs[0].Namespace != "kube-system" || pairs[0].Name != "coredns" {
		t.Fatalf("unexpected pairs: %+v", pairs)
	}

	// Phrase present but release not named → matched with empty pairs
	// (caller falls back to scanning managed releases).
	pairs, matched = ParseHelmPendingOperations("another operation (install/upgrade/rollback) is in progress")
	if !matched || len(pairs) != 0 {
		t.Fatalf("expected matched with no pairs, got matched=%v pairs=%+v", matched, pairs)
	}

	if _, matched := ParseHelmPendingOperations("cannot patch: something is invalid"); matched {
		t.Fatal("unrelated error must not match")
	}
}

func TestParseHelmPatchFailuresLineScoped(t *testing.T) {
	msg := `error: update failed: 2 errors occurred:
Helm Release kube-system/coredns: cannot patch "coredns" with kind Deployment: Deployment.apps "coredns" is invalid
Helm Release kube-system/metrics-server: context deadline exceeded`
	pairs := ParseHelmPatchFailures(msg)
	if len(pairs) != 1 || pairs[0].Name != "coredns" {
		t.Fatalf("must mark only the failing release, got %+v", pairs)
	}
	if ParseHelmPatchFailures("all healthy") != nil {
		t.Fatal("no phrase → nil")
	}
}
