package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests are the heat-params contract guard: they parse the REAL
// write-heat-params*.sh from the forked driver and assert the scenario renderer
// emits every key those scripts write. Without this, the renderer silently
// drifts from production (as it did when RECONCILER_RUN_TIMEOUT_SECONDS was added
// to the driver but not here), and the e2e tiers stop being faithful.
//
// The driver checkout is located via MAGNUM_VICTORIA_DIR (CI sets it, so the
// guard is ENFORCED there) or a sibling ../../../magnum_victoria checkout for
// local dev. When neither exists the test skips rather than failing, so a bare
// `go test ./...` without the driver is still green.

func driverBootstrapDir(t *testing.T) string {
	t.Helper()
	rel := filepath.Join("magnum", "drivers", "common", "templates", "kubernetes", "bootstrap")
	if d := strings.TrimSpace(os.Getenv("MAGNUM_VICTORIA_DIR")); d != "" {
		p := filepath.Join(d, rel)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("MAGNUM_VICTORIA_DIR is set but %s is missing: %v", p, err)
		}
		return p
	}
	guess := filepath.Join("..", "..", "..", "magnum_victoria", rel)
	if _, err := os.Stat(guess); err == nil {
		return guess
	}
	t.Skip("driver scripts not found; set MAGNUM_VICTORIA_DIR to enforce the heat-params contract")
	return ""
}

// driverHeredocKeys returns the set of heat-params keys the given write-heat-
// params script writes, i.e. the LHS of every KEY="..." line inside its
// `cat << EOF > ${HEAT_PARAMS}` ... `EOF` block.
func driverHeredocKeys(t *testing.T, file string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(driverBootstrapDir(t), file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	keys := map[string]bool{}
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		if !inBlock {
			if strings.Contains(line, "cat << EOF > ") {
				inBlock = true
			}
			continue
		}
		if strings.TrimSpace(line) == "EOF" {
			break
		}
		if i := strings.IndexByte(line, '='); i > 0 {
			if k := line[:i]; isHeatKey(k) {
				keys[k] = true
			}
		}
	}
	if len(keys) == 0 {
		t.Fatalf("parsed no heredoc keys from %s — heredoc format may have changed", file)
	}
	return keys
}

func isHeatKey(k string) bool {
	if k == "" || (k[0] != '_' && (k[0] < 'A' || k[0] > 'Z')) {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// scenarioKeys is the set of keys the renderer emits for a role. Every key is
// emitted unconditionally (values, not presence, vary by operation), so any
// Config of that role yields the full set.
func scenarioKeys(role Role) map[string]bool {
	keys := map[string]bool{}
	for _, kv := range base(role, OpCreate).Inputs() {
		keys[kv.Name] = true
	}
	return keys
}

func assertSuperset(t *testing.T, role string, driver, scen map[string]bool) {
	t.Helper()
	var missing []string
	for k := range driver {
		if !scen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s: scenario renderer is missing %d heat-params key(s) the driver writes: %s\n"+
			"add them to scenario.go pairs() so the e2e tiers stay faithful to production",
			role, len(missing), strings.Join(missing, " "))
	}
}

func TestHeatParamsContractMaster(t *testing.T) {
	assertSuperset(t, "master", driverHeredocKeys(t, "write-heat-params-master.sh"), scenarioKeys(RoleMaster))
}

func TestHeatParamsContractWorker(t *testing.T) {
	assertSuperset(t, "worker", driverHeredocKeys(t, "write-heat-params.sh"), scenarioKeys(RoleWorker))
}

// TestKnownDriverGapMasterIndex documents and tracks a known mismatch: the
// binary reads MASTER_INDEX (internal/config) but the real driver does not write
// it, so MasterIndex is always 0 in production. The scenario renderer DOES emit
// it (the correct value). If the driver is fixed to write MASTER_INDEX, this
// test fails to remind us to drop the scenarioExtras note in scenario.go.
func TestKnownDriverGapMasterIndex(t *testing.T) {
	if driverHeredocKeys(t, "write-heat-params-master.sh")["MASTER_INDEX"] {
		t.Fatal("driver now writes MASTER_INDEX — update scenario.go's scenarioExtras note; the prod gap is closed")
	}
	if !scenarioKeys(RoleMaster)["MASTER_INDEX"] {
		t.Fatal("binary reads MASTER_INDEX; scenario must keep emitting it")
	}
}
