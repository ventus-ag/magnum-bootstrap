package reconcile

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
)

// historyKeep is the number of most-recent Pulumi update-history entries to
// retain per stack. The local ("DIY") file backend writes one history snapshot
// (a *.history.json + *.checkpoint.json pair) on every up AND every refresh and
// never prunes them. Over a cluster's lifetime — many CA rotations, upgrades,
// and daily periodic runs — this directory grows without bound, and the backend
// touches it on every operation, so each successive reconcile gets slower. We
// keep a generous window for debuggability and drop the rest.
const historyKeep = 20

// pruneStackHistory trims the local Pulumi backend's per-stack update-history
// directory to the newest historyKeep entries. It is best-effort: any error is
// logged and ignored, because failing to prune must never fail a reconcile. It
// deliberately touches ONLY .pulumi/history/<stack>/ — never the live
// checkpoint under .pulumi/stacks/ — so it cannot corrupt stack state.
func pruneStackHistory(stateDir string, logger *logging.Logger) {
	if stateDir == "" {
		return
	}
	historyRoot := filepath.Join(stateDir, ".pulumi", "history")
	stackDirs, err := os.ReadDir(historyRoot)
	if err != nil {
		return // no history yet, or backend not local — nothing to prune
	}
	for _, sd := range stackDirs {
		if !sd.IsDir() {
			continue
		}
		removed := pruneHistoryDir(filepath.Join(historyRoot, sd.Name()))
		if removed > 0 && logger != nil {
			logger.Infof("pruned pulumi history stack=%s removedFiles=%d keep=%d", sd.Name(), removed, historyKeep)
		}
	}
}

// pruneHistoryDir keeps the newest historyKeep history "versions" in a single
// stack history directory and deletes older ones. Pulumi names each entry
// <stack>-<RFC3339-ish-timestamp>.{history,checkpoint}.json, so lexical sort on
// the version prefix is chronological. Returns the number of files removed.
func pruneHistoryDir(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	// Group files by their version prefix (everything before the first dot).
	versions := map[string][]string{}
	order := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		dot := strings.IndexByte(name, '.')
		if dot <= 0 {
			continue
		}
		prefix := name[:dot]
		if _, seen := versions[prefix]; !seen {
			order = append(order, prefix)
		}
		versions[prefix] = append(versions[prefix], name)
	}
	if len(order) <= historyKeep {
		return 0
	}
	sort.Strings(order) // chronological: oldest first
	stale := order[:len(order)-historyKeep]
	removed := 0
	for _, prefix := range stale {
		for _, f := range versions[prefix] {
			if err := os.Remove(filepath.Join(dir, f)); err == nil {
				removed++
			}
		}
	}
	return removed
}
