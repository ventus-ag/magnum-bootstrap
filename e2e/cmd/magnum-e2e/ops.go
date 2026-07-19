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
	// nodepool-metadata: patch node_labels/node_taints on the active nodepool
	// (single + multiple add, partial + full delete) and verify Node objects +
	// taint scheduling semantics after every stage (see nodepool_metadata.go).
	"nodepool-metadata": true,
	"post-rotate":       true,
	"cloud-smoke":       true,
	"verify-sa":         true,
	"autoscale":         true,
	"sonobuoy":          true,
	// component toggle: flip an addon label on the live cluster, then assert the
	// reconciler installed/uninstalled it (see toggle.go).
	"disable-autoscaler":    true,
	"enable-metrics-server": true,
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
//
// The optional template fields let a scenario pin its own create/upgrade/nodepool
// templates and SSH user so it is zero-config in CI (templates otherwise come from
// CLUSTER_TEMPLATE/UPGRADE_TEMPLATE). They are applied for any value the user did
// not set explicitly (loadConfig) or forced per-scenario in the "all" sweep
// (scenarioRunner).
type scenarioDef struct {
	masters int
	workers int
	ops     string

	template         string // create template (name or UUID); empty = use CLUSTER_TEMPLATE
	upgradeTemplate  string // upgrade target for the `upgrade` op; empty = same as template
	sshUser          string // node-log SSH user (FCoS=core, Ubuntu=ubuntu); empty = -ssh-user
	nodepoolTemplate string // cluster_template_id label for the extra nodepool

	// upgradeLadder, when non-empty, makes each `upgrade` op in this scenario climb
	// one rung instead of all re-upgrading to the same upgradeTemplate. It feeds
	// runner.ladder (the same cursor the version-ladder scenario uses) and so MUST
	// have at least as many rungs as the chain has `upgrade` ops. The create version
	// still comes from CLUSTER_TEMPLATE (this only supplies the upgrade targets, so
	// UPGRADE_TEMPLATE is unused for this scenario). An explicit UPGRADE_LADDER env
	// still overrides it (resolveLadder).
	upgradeLadder []string
}

// Ubuntu (k8s_ubuntu_v1) cluster templates on the ventus cloud. Overridable via
// CLUSTER_TEMPLATE / UPGRADE_TEMPLATE / NODEPOOL_TEMPLATE.
const (
	ubuntuTemplate129 = "v1.29.14-u22"
	ubuntuTemplate131 = "v1.31.6-u22"
)

// scenarios are the CI/dispatch presets.
//
//   - smoke             — default 1m/1w coverage: addon label toggles, upgrade,
//     worker resize, repeated CA-rotation/upgrade wedge sequence, then
//     post-rotation master add + SA check.
//   - multinode         — default 3m/2w coverage: extra nodepool lifecycle
//     (including the node_labels/node_taints add+delete metadata cycle),
//     worker+nodepool resize up/down, repeated upgrade/CA-rotation wedge sequence,
//     then post-rotation add-node + SA check.
//   - chained-single    — the repeated-op wedge sequence on 1 node.
//   - chained-multinode — the same chain on 3m/2w + nodepool (concurrent dual-CA
//     barrier + heterogeneous node sizes through the whole chain).
var scenarios = map[string]scenarioDef{
	"smoke": {
		masters: 1, workers: 1,
		ops: "disable-autoscaler,enable-metrics-server,upgrade,cloud-smoke,resize-workers=2,ca-rotate,ca-rotate,upgrade,upgrade,ca-rotate,post-rotate",
		// The 3 `upgrade` ops climb a real version ladder (see climbLadder) instead
		// of re-upgrading to 1.31 three times — one rung per `upgrade` op.
		upgradeLadder: climbLadder,
	},
	"multinode": {
		masters: 3, workers: 2,
		ops: "add-nodepool=2,nodepool-metadata,resize-workers=3,resize-nodepool=3,resize-workers=2,resize-nodepool=1,upgrade,ca-rotate,ca-rotate,upgrade,upgrade,ca-rotate,post-rotate,del-nodepool",
		// 3-master coverage: the 3 `upgrade` ops climb 1.31→1.32→1.33 (see
		// climbLadder) so multimaster upgrades exercise real minor bumps.
		upgradeLadder: climbLadder,
	},
	// multimaster — fast per-PR INITIAL 3-master coverage: proves batch etcd
	// formation (3 members from scratch behind the control-plane LB VIP), then a
	// CA rotation through the concurrent dual-CA barrier and an SA-consistency
	// check across all three apiservers. This is the reliable multi-master gate
	// the TCG FCoS tier cannot be.
	"multimaster": {
		masters: 3, workers: 1,
		ops: "cloud-smoke,ca-rotate,verify-sa",
	},
	// multimaster-scale — fast per-PR SCALE 1->3 coverage: create a single-master
	// cluster then resize the master nodegroup to 3, exercising the sequential
	// etcd learner-join + promotion path on a live cluster (exactly what breaks
	// under TCG on the FCoS tier), then rotate + verify SA trust across the grown
	// control plane. verifyBundle asserts control-plane node count == master
	// nodegroup count after the resize.
	"multimaster-scale": {
		masters: 1, workers: 1,
		ops: "resize-masters=3,cloud-smoke,ca-rotate,verify-sa",
	},
	"chained-single": {
		masters: 1, workers: 1,
		ops: "upgrade,ca-rotate,ca-rotate,upgrade,upgrade,ca-rotate",
	},
	"chained-multinode": {
		masters: 3, workers: 2,
		ops: "add-nodepool=1,upgrade,ca-rotate,ca-rotate,upgrade,upgrade,ca-rotate",
		// 3-master repeated-op wedge chain: the 3 `upgrade` ops climb 1.31→1.32→1.33
		// (see climbLadder) so each is a real bump through the concurrent dual-CA
		// barrier, not the same transition thrice.
		upgradeLadder: climbLadder,
	},
	// ubuntu-upgrade — k8s_ubuntu_v1 driver lifecycle: create 1.29 → upgrade 1.31
	// (a 2-minor cluster upgrade; the ±1 skew guard is nodepool-only) →
	// ca-rotate → verify-sa → cloud-smoke. The rotation is deliberately here:
	// Ubuntu is the second OS and this is the only tier that ever exercises
	// the disruptive CA-rotation path on it (cloud-init provisioning, units
	// under /lib/systemd, podman control plane).
	"ubuntu-upgrade": {
		masters: 1, workers: 1,
		ops:             "upgrade,ca-rotate,verify-sa,cloud-smoke",
		template:        ubuntuTemplate129,
		upgradeTemplate: ubuntuTemplate131,
		sshUser:         "ubuntu",
	},
	// ubuntu-nodepool — Ubuntu cluster + extra Ubuntu nodepool created via the
	// fork's cluster_template_id label path (same OS, same version → within skew).
	"ubuntu-nodepool": {
		masters: 1, workers: 1,
		ops:              "add-nodepool=1,nodepool-metadata,resize-nodepool=2,del-nodepool",
		template:         ubuntuTemplate131,
		sshUser:          "ubuntu",
		nodepoolTemplate: ubuntuTemplate131,
	},
	// sonobuoy — create a cluster and run a Sonobuoy conformance test against it.
	// Mode is SONOBUOY_MODE (default "quick" for the daily/PR sweep; the weekly
	// cron sets "certified-conformance"). The conformance workflow runs one leg
	// per Kubernetes version (matrix), each its own cluster + Actions job. The
	// create version comes from CLUSTER_TEMPLATE (+ optional KUBE_TAG override for
	// versions newer than any pinned template — see resolveConformanceLegs).
	//
	// TWO workers: Kubernetes conformance REQUIRES at least two untainted
	// (schedulable) nodes — the master is tainted, so 1 worker fails
	// "[sig-architecture] ... should have at least two untainted nodes"
	// ("Conformance requires at least two nodes") plus a cluster of DNS/DaemonSet
	// tests that need to schedule across two nodes (and relieves the single-node
	// contention that timed the DNS log-fetches out). Confirmed by the first full
	// certified-conformance sweep (6-7 identical failures per version on 1m/1w).
	"sonobuoy": {
		masters: 1, workers: 2,
		ops: "sonobuoy",
	},
	// component-toggle — flip cluster addon labels on a live cluster and assert the
	// reconciler installs/uninstalls: disable autoscaler (Pulumi prunes the Helm
	// release) then enable metrics-server. OS-agnostic; uses the default template.
	"component-toggle": {
		masters: 1, workers: 1,
		ops: "disable-autoscaler,enable-metrics-server",
	},
}

// allScenarios is the ordered default sweep run by the "all" meta-scenario (one
// cluster per entry, sequentially, in a single invocation). It intentionally keeps
// Fedora coverage to the two cluster shapes we need to create (single-master and
// multi-master), keeps Ubuntu upgrade/nodepool coverage as separate OS-driver
// scenarios, and keeps the long version-ladder walk. Other named scenarios remain
// dispatchable for focused/manual runs.
//
// version-ladder is the one entry whose op chain is generated from the upgrade
// ladder (not a fixed preset in the scenarios map), so scenarioRunner/preflightAll
// special-case it; see ladderScenario.
var allScenarios = []string{"smoke", "multinode", "ubuntu-upgrade", "ubuntu-nodepool", ladderScenario}

// ladderScenario is the name of the multi-version upgrade scenario. Its op chain
// is generated from the upgrade ladder (one upgrade+cloud-smoke per rung), so it
// lives outside the static scenarios map.
const ladderScenario = "version-ladder"

// defaultVersionLadder is the version-ladder scenario's default walk: the cluster
// is CREATEd at rung[0] and upgraded through each subsequent rung, re-running the
// cloud-controller smoke (LB serves traffic + Cinder PVC resize) at every step.
// Each entry is a version-pinned Magnum cluster-template name (kube_tag baked in);
// all nine exist on the ventus cloud (Upper-Austria-M1). The jumps are
// deliberately multi-minor (e.g. 1.23→1.28) to stress aggressive upgrades.
var defaultVersionLadder = []string{
	"v1.20.12", "v1.23.17", "v1.28.4", "v1.30.10",
	"v1.32.2", "v1.33.10", "v1.34.6", "v1.35.3",
	"v1.36.2",
}

// climbLadder is the per-upgrade target ladder shared by the lifecycle scenarios
// whose chain has exactly 3 `upgrade` ops (smoke, multinode, chained-multinode).
// Instead of re-upgrading to the same version 3×, each `upgrade` op climbs one
// real minor: create v1.30.10 (CLUSTER_TEMPLATE) → 1.31.6 → 1.32.2 → 1.33.10. The
// upgrade-after-ca-rotate race coverage is preserved (the later rungs still fire
// right after a ca-rotate). All three rungs exist on the ventus cloud.
var climbLadder = []string{"v1.31.6", "v1.32.2", "v1.33.10"}

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

// resolveLadder picks the per-upgrade target ladder for a runner. Precedence:
// an explicit UPGRADE_LADDER (cfg.upgradeLadder, also set by the version-ladder
// scenario's applyLadderDefaults) always wins; otherwise a scenario's built-in
// upgradeLadder (e.g. smoke's 1.31→1.32→1.33 climb) is used. Empty result =
// single fixed upgradeTemplate (upgradeTarget falls back to cfg.upgradeTemplate).
func resolveLadder(cfg config) []string {
	if l := splitTrim(cfg.upgradeLadder); len(l) > 0 {
		return l
	}
	if def, ok := scenarios[cfg.scenario]; ok && len(def.upgradeLadder) > 0 {
		return append([]string(nil), def.upgradeLadder...)
	}
	return nil
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
