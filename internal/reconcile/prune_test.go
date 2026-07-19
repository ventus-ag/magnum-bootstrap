package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeHistoryVersion creates the checkpoint+history file pair Pulumi writes for
// one update version.
func writeHistoryVersion(t *testing.T, dir, version string) {
	t.Helper()
	for _, suffix := range []string{".history.json", ".checkpoint.json"} {
		f := filepath.Join(dir, version+suffix)
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
}

func TestPruneHistoryDirKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	total := historyKeep + 5
	for i := 0; i < total; i++ {
		// Zero-padded so lexical order == chronological order.
		writeHistoryVersion(t, dir, fmt.Sprintf("node-x-%04d", i))
	}

	removed := pruneHistoryDir(dir)
	if want := 5 * 2; removed != want {
		t.Fatalf("removed = %d, want %d", removed, want)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(entries); got != historyKeep*2 {
		t.Fatalf("remaining files = %d, want %d", got, historyKeep*2)
	}

	// The oldest 5 versions must be gone; the newest historyKeep must remain.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if names[0] != "node-x-0005.checkpoint.json" && names[0] != "node-x-0005.history.json" {
		t.Fatalf("expected oldest surviving version 0005, got %q", names[0])
	}
}

func TestPruneHistoryDirNoopUnderThreshold(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < historyKeep; i++ {
		writeHistoryVersion(t, dir, fmt.Sprintf("node-x-%04d", i))
	}
	if removed := pruneHistoryDir(dir); removed != 0 {
		t.Fatalf("removed = %d, want 0 (at/under keep threshold)", removed)
	}
}

func TestPruneStackHistoryMissingRootIsSafe(t *testing.T) {
	// No .pulumi/history under the temp dir: must not panic or error.
	pruneStackHistory(t.TempDir(), nil)
	// Empty stateDir: must be a no-op.
	pruneStackHistory("", nil)
}

func TestTargetPhaseURNs(t *testing.T) {
	urns := targetPhaseURNs("node-master-0", "magnum-bootstrap", "cluster-coredns")
	want := []string{
		"urn:pulumi:node-master-0::magnum-bootstrap::*::node-master-0-cluster-coredns",
		"urn:pulumi:node-master-0::magnum-bootstrap::*::node-master-0-cluster-coredns-*",
	}
	if len(urns) != len(want) {
		t.Fatalf("got %d urns, want %d", len(urns), len(want))
	}
	for i := range want {
		if urns[i] != want[i] {
			t.Errorf("urn[%d] = %q, want %q", i, urns[i], want[i])
		}
	}
}
