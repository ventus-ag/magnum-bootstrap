package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
)

// op is one entry in a scenario's operation chain. Tokens are written as
// "name" or "name=N" (N is an integer argument, e.g. a resize target).
type op struct {
	name   string
	arg    int
	hasArg bool
}

// argOr returns the op's integer argument, or def when the token had none.
func (o op) argOr(def int) int {
	if o.hasArg {
		return o.arg
	}
	return def
}

// knownOps is the set of accepted op names (validated at parse time so a typo in
// OPS/SCENARIO fails fast, before any billed cloud resource is created).
var knownOps = map[string]bool{
	"upgrade":         true,
	"ca-rotate":       true,
	"resize-workers":  true,
	"resize-masters":  true,
	"resize-nodepool": true,
	"add-nodepool":    true,
	"del-nodepool":    true,
	"post-rotate":     true,
	"cloud-smoke":     true,
	"verify-sa":       true,
	"autoscale":       true,
}

// parseOps parses a comma-separated op list. Each token is "name" or "name=N".
func parseOps(s string) ([]op, error) {
	var ops []op
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		name, argStr, hasArg := strings.Cut(tok, "=")
		name = strings.TrimSpace(name)
		if !knownOps[name] {
			return nil, fmt.Errorf("unknown op %q in op list (known: %s)", name, opNames())
		}
		o := op{name: name}
		if hasArg {
			n, err := strconv.Atoi(strings.TrimSpace(argStr))
			if err != nil {
				return nil, fmt.Errorf("op %q: invalid integer arg %q: %w", name, argStr, err)
			}
			o.arg, o.hasArg = n, true
		}
		ops = append(ops, o)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("empty op list")
	}
	return ops, nil
}

// formatOp renders one op back to its token form (for logs).
func formatOp(o op) string {
	if o.hasArg {
		return fmt.Sprintf("%s=%d", o.name, o.arg)
	}
	return o.name
}

// formatOps renders an op chain as a comma-separated token list (for logs).
func formatOps(ops []op) string {
	parts := make([]string, len(ops))
	for i, o := range ops {
		parts[i] = formatOp(o)
	}
	return strings.Join(parts, ",")
}

func opNames() string {
	names := make([]string, 0, len(knownOps))
	for n := range knownOps {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// scenarioDef is a named preset: a cluster shape plus an op chain. The shape
// values are applied as defaults in loadConfig (explicit flags/env still win);
// the op string is parsed at run start.
type scenarioDef struct {
	masters int
	workers int
	ops     string
}

// scenarios are the CI/dispatch presets.
//
//   - smoke             — current 1m/1w linear coverage (back-compat baseline).
//   - multinode         — headline 3m/2w + extra nodepool; worker+nodepool resize
//     up/down THEN upgrade THEN ca-rotate (the explicitly verified
//     resize→upgrade→ca-rotate order), then post-rotate add-node + SA check.
//   - chained-single    — the repeated-op wedge sequence on 1 node.
//   - chained-multinode — the same chain on 3m/2w + nodepool (concurrent dual-CA
//     barrier + heterogeneous node sizes through the whole chain).
var scenarios = map[string]scenarioDef{
	"smoke": {
		masters: 1, workers: 1,
		ops: "upgrade,resize-workers=2,ca-rotate,post-rotate",
	},
	"multinode": {
		masters: 3, workers: 2,
		ops: "add-nodepool=2,resize-workers=3,resize-nodepool=3,resize-workers=2,resize-nodepool=1,upgrade,ca-rotate,post-rotate",
	},
	"chained-single": {
		masters: 1, workers: 1,
		ops: "upgrade,ca-rotate,ca-rotate,upgrade,upgrade,ca-rotate",
	},
	"chained-multinode": {
		masters: 3, workers: 2,
		ops: "add-nodepool=1,upgrade,ca-rotate,ca-rotate,upgrade,upgrade,ca-rotate",
	},
}

// allScenarios is the ordered list run by the "all" meta-scenario (one cluster
// per entry, sequentially, in a single invocation). Order matters: cheapest
// first so a smoke break fails fast before the long multi-master chains, and the
// long version-ladder walk runs LAST.
//
// version-ladder is the one entry whose op chain is generated from the upgrade
// ladder (not a fixed preset in the scenarios map), so scenarioRunner/preflightAll
// special-case it; see ladderScenario.
var allScenarios = []string{"smoke", "multinode", "chained-single", "chained-multinode", ladderScenario}

// ladderScenario is the name of the multi-version upgrade scenario. Its op chain
// is generated from the upgrade ladder (one upgrade+cloud-smoke per rung), so it
// lives outside the static scenarios map (and outside allScenarios).
const ladderScenario = "version-ladder"

// defaultVersionLadder is the version-ladder scenario's default walk: the cluster
// is CREATEd at rung[0] and upgraded through each subsequent rung, re-running the
// cloud-controller smoke (LB serves traffic + Cinder PVC resize) at every step.
// Each entry is a version-pinned Magnum cluster-template name (kube_tag baked in);
// all eight exist on the ventus cloud (Upper-Austria-M1). The jumps are
// deliberately multi-minor (e.g. 1.23→1.28) to stress aggressive upgrades.
var defaultVersionLadder = []string{
	"v1.20.12", "v1.23.17", "v1.28.4", "v1.30.10",
	"v1.32.2", "v1.33.10", "v1.34.6", "v1.35.3",
}

// splitTrim splits a comma-separated list, trimming spaces and dropping empties.
func splitTrim(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// nextLadderTarget returns the upgrade template at pos and the advanced position,
// or an error if the op chain requested more upgrades than the ladder has rungs.
// It is pure (no cluster I/O) so the advance/overflow logic is unit-testable.
func nextLadderTarget(ladder []string, pos int) (string, int, error) {
	if pos >= len(ladder) {
		return "", pos, fmt.Errorf("upgrade ladder exhausted: requested upgrade #%d but ladder has only %d rung(s) (%s)",
			pos+1, len(ladder), strings.Join(ladder, ","))
	}
	return ladder[pos], pos + 1, nil
}

// ladderOps generates the version-ladder op chain: per ladder rung, upgrade then
// re-check the cloud controller (cloud-smoke: LB serves + PVC resize) then drive
// the cluster-autoscaler up>2 and back down (autoscale). The create-time
// cloud-smoke (run()) already covers the create version.
func ladderOps(ladder []string) string {
	parts := make([]string, 0, len(ladder)*3)
	for range ladder {
		parts = append(parts, "upgrade", "cloud-smoke", "autoscale")
	}
	return strings.Join(parts, ",")
}

// opsContain reports whether the op chain includes an op with the given name.
func opsContain(ops []op, name string) bool {
	for _, o := range ops {
		if o.name == name {
			return true
		}
	}
	return false
}

func scenarioNames() string {
	names := make([]string, 0, len(scenarios))
	for n := range scenarios {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

// legacyOps builds the pre-op-engine default chain from the SKIP_* flags, so a
// run with neither OPS nor SCENARIO behaves exactly as the old linear pipeline.
func legacyOps(c config) string {
	var parts []string
	if !c.skipUpgrade {
		parts = append(parts, "upgrade")
	}
	if !c.skipResize {
		parts = append(parts, fmt.Sprintf("resize-workers=%d", c.nodeCountResize))
	}
	if !c.skipCARotate {
		parts = append(parts, "ca-rotate")
		if !c.skipPostRotate {
			parts = append(parts, "post-rotate")
		}
	}
	return strings.Join(parts, ",")
}

// resolveOpList picks the op chain by precedence: explicit OPS > SCENARIO preset
// > legacy SKIP_* flags. It also validates the scenario name.
func (r *runner) resolveOpList() ([]op, error) {
	raw := strings.TrimSpace(r.cfg.ops)
	switch {
	case raw != "":
		// explicit override
	case r.cfg.scenario == ladderScenario:
		if len(r.ladder) == 0 {
			return nil, fmt.Errorf("scenario %q needs an upgrade ladder (set UPGRADE_LADDER / -upgrade-ladder)", ladderScenario)
		}
		raw = ladderOps(r.ladder)
	case r.cfg.scenario != "":
		sc, ok := scenarios[r.cfg.scenario]
		if !ok {
			return nil, fmt.Errorf("unknown scenario %q (known: %s, %s)", r.cfg.scenario, scenarioNames(), ladderScenario)
		}
		raw = sc.ops
	default:
		raw = legacyOps(r.cfg)
	}
	return parseOps(raw)
}

// retryableMutationErr classifies an error from a Magnum mutating trigger
// (upgrade/resize/ca-rotate/nodegroup) as worth retrying after the cluster
// settles, vs a hard failure. The motivating case is the chained-op race where
// the previous update is still in flight and Magnum rejects the next trigger
// with 400 "Updating a cluster when status is \"UPDATE_IN_PROGRESS\" is not
// supported"; transient 5xx / 409 / connection errors are also retryable. A
// genuine *_FAILED surfaced as an error is NOT retryable.
func retryableMutationErr(err error) bool {
	if err == nil {
		return false
	}
	var rc gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &rc) {
		switch {
		case rc.Actual == 409:
			return true
		case rc.Actual >= 500:
			return true
		case rc.Actual == 400 && bytes.Contains(bytes.ToUpper(rc.Body), []byte("IN_PROGRESS")):
			return true
		default:
			// Some 400s carry the message only in Error() (wrapped); fall through
			// to the substring check below rather than rejecting here.
		}
	}
	msg := strings.ToUpper(err.Error())
	switch {
	case strings.Contains(msg, "IN_PROGRESS"):
		return true
	case strings.Contains(msg, "CONNECTION RESET"),
		strings.Contains(msg, "CONNECTION REFUSED"),
		strings.Contains(msg, "TIMEOUT"),
		strings.Contains(msg, "TEMPORARY"),
		strings.Contains(msg, "EOF"):
		return true
	}
	return false
}
