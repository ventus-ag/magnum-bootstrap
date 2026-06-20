package main

import (
	"fmt"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
)

func TestParseOps(t *testing.T) {
	ops, err := parseOps("upgrade, ca-rotate ,resize-workers=3,add-nodepool=2")
	if err != nil {
		t.Fatalf("parseOps: %v", err)
	}
	want := []op{
		{name: "upgrade"},
		{name: "ca-rotate"},
		{name: "resize-workers", arg: 3, hasArg: true},
		{name: "add-nodepool", arg: 2, hasArg: true},
	}
	if len(ops) != len(want) {
		t.Fatalf("got %d ops, want %d (%v)", len(ops), len(want), ops)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Errorf("op %d = %+v, want %+v", i, ops[i], want[i])
		}
	}
	// round-trip
	if got := formatOps(ops); got != "upgrade,ca-rotate,resize-workers=3,add-nodepool=2" {
		t.Errorf("formatOps round-trip = %q", got)
	}
}

func TestParseOpsErrors(t *testing.T) {
	for _, s := range []string{
		"upgrade,bogus-op",
		"resize-workers=notint",
		"",
		"   ",
	} {
		if _, err := parseOps(s); err == nil {
			t.Errorf("parseOps(%q) = nil error, want error", s)
		}
	}
}

func TestArgOr(t *testing.T) {
	if got := (op{name: "resize-workers"}).argOr(2); got != 2 {
		t.Errorf("argOr default = %d, want 2", got)
	}
	if got := (op{name: "resize-workers", arg: 5, hasArg: true}).argOr(2); got != 5 {
		t.Errorf("argOr explicit = %d, want 5", got)
	}
}

// TestScenariosParse guards that every built-in scenario's op string is valid —
// a typo in the catalog fails here, not on a billed cloud run.
func TestScenariosParse(t *testing.T) {
	for name, sc := range scenarios {
		if _, err := parseOps(sc.ops); err != nil {
			t.Errorf("scenario %q ops %q: %v", name, sc.ops, err)
		}
		if sc.masters < 1 || sc.workers < 0 {
			t.Errorf("scenario %q bad shape %d/%d", name, sc.masters, sc.workers)
		}
	}
	// chained scenarios must contain the user-requested wedge sequence.
	for _, name := range []string{"chained-single", "chained-multinode"} {
		ops, err := parseOps(scenarios[name].ops)
		if err != nil {
			t.Fatal(err)
		}
		var rotates, upgrades int
		for _, o := range ops {
			switch o.name {
			case "ca-rotate":
				rotates++
			case "upgrade":
				upgrades++
			}
		}
		if rotates < 3 || upgrades < 3 {
			t.Errorf("scenario %q: want >=3 upgrades and >=3 ca-rotates, got %d/%d", name, upgrades, rotates)
		}
	}
}

// TestAllScenariosCoverMap ensures the "all" meta-scenario runs exactly the
// scenarios defined in the catalog (no preset silently dropped, no dangling
// name).
func TestAllScenariosCoverMap(t *testing.T) {
	if len(allScenarios) != len(scenarios) {
		t.Fatalf("allScenarios has %d entries, scenarios map has %d", len(allScenarios), len(scenarios))
	}
	for _, scn := range allScenarios {
		if _, ok := scenarios[scn]; !ok {
			t.Errorf("allScenarios entry %q not in scenarios map", scn)
		}
	}
}

func TestPerScenarioName(t *testing.T) {
	cases := []struct {
		base, scn, want string
	}{
		{"recon-e2e-all-12345", "smoke", "recon-e2e-smoke-12345"},
		{"recon-e2e-all-12345", "chained-multinode", "recon-e2e-chained-multinode-12345"},
		{"my-cluster", "multinode", "my-cluster-multinode"},
	}
	for _, c := range cases {
		if got := perScenarioName(c.base, c.scn); got != c.want {
			t.Errorf("perScenarioName(%q,%q) = %q, want %q", c.base, c.scn, got, c.want)
		}
	}
}

// TestSplitTrim covers comma/space trimming and empty-element dropping.
func TestSplitTrim(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"a", []string{"a"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
		{"v1.23.17,v1.28.4", []string{"v1.23.17", "v1.28.4"}},
	}
	for _, c := range cases {
		got := splitTrim(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitTrim(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitTrim(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

// TestNextLadderTarget pins ladder advancement and overflow: each call returns
// the rung at pos and pos+1, and a pos past the end errors (the op chain asked
// for more upgrades than the ladder has rungs).
func TestNextLadderTarget(t *testing.T) {
	ladder := []string{"v1.23.17", "v1.28.4", "v1.30.10"}
	pos := 0
	for i, want := range ladder {
		got, next, err := nextLadderTarget(ladder, pos)
		if err != nil {
			t.Fatalf("rung %d: unexpected err %v", i, err)
		}
		if got != want {
			t.Errorf("rung %d: got %q, want %q", i, got, want)
		}
		if next != pos+1 {
			t.Errorf("rung %d: next = %d, want %d", i, next, pos+1)
		}
		pos = next
	}
	if _, _, err := nextLadderTarget(ladder, pos); err == nil {
		t.Errorf("nextLadderTarget past end = nil error, want overflow error")
	}
}

// TestLadderOps checks the generated version-ladder chain: one (upgrade,
// cloud-smoke) pair per rung, valid op names, in order.
func TestLadderOps(t *testing.T) {
	ladder := []string{"v1.23.17", "v1.28.4", "v1.30.10"}
	ops, err := parseOps(ladderOps(ladder))
	if err != nil {
		t.Fatalf("generated ladder ops do not parse: %v", err)
	}
	if len(ops) != len(ladder)*2 {
		t.Fatalf("got %d ops, want %d (%s)", len(ops), len(ladder)*2, formatOps(ops))
	}
	for i, o := range ops {
		want := "upgrade"
		if i%2 == 1 {
			want = "cloud-smoke"
		}
		if o.name != want {
			t.Errorf("op %d = %q, want %q", i, o.name, want)
		}
		if !knownOps[o.name] {
			t.Errorf("op %d %q not in knownOps", i, o.name)
		}
	}
}

// TestDefaultVersionLadder pins the requested 1.20→1.35 walk: 8 version-pinned
// rungs in order (rung[0] is the create template, the rest the upgrade ladder).
func TestDefaultVersionLadder(t *testing.T) {
	want := []string{"v1.20.12", "v1.23.17", "v1.28.4", "v1.30.10", "v1.32.2", "v1.33.10", "v1.34.6", "v1.35.3"}
	if len(defaultVersionLadder) != len(want) {
		t.Fatalf("defaultVersionLadder has %d rungs, want %d", len(defaultVersionLadder), len(want))
	}
	for i := range want {
		if defaultVersionLadder[i] != want[i] {
			t.Errorf("rung %d = %q, want %q", i, defaultVersionLadder[i], want[i])
		}
	}
	// The generated chain for the built-in ladder upgrades through every rung
	// after the create version (len-1 upgrades).
	ops, err := parseOps(ladderOps(defaultVersionLadder[1:]))
	if err != nil {
		t.Fatal(err)
	}
	var upgrades int
	for _, o := range ops {
		if o.name == "upgrade" {
			upgrades++
		}
	}
	if upgrades != len(want)-1 {
		t.Errorf("built-in ladder generates %d upgrades, want %d", upgrades, len(want)-1)
	}
}

// TestRetryableMutationErr pins the exact observed failure as retryable and a
// genuine *_FAILED / 404 as not — this is the core of the chained-op robustness
// fix (settle + retry instead of hard-fail + teardown).
func TestRetryableMutationErr(t *testing.T) {
	// The exact body Magnum returned in the wild.
	busyBody := []byte(`{"errors": [{"request_id": "", "code": "client", "status": 400, "title": "Updating a cluster when status is \"UPDATE_IN_PROGRESS\" is not supported", "detail": "Updating a cluster when status is \"UPDATE_IN_PROGRESS\" is not supported.", "links": []}]}`)
	busy := gophercloud.ErrUnexpectedResponseCode{Actual: 400, Body: busyBody, Expected: []int{202}, Method: "PATCH", URL: "https://example:9511/certificates/x"}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"busy-400-direct", busy, true},
		{"busy-400-wrapped", fmt.Errorf("trigger CA rotation: %w", busy), true},
		{"server-500", gophercloud.ErrUnexpectedResponseCode{Actual: 500, Body: []byte("boom")}, true},
		{"conflict-409", gophercloud.ErrUnexpectedResponseCode{Actual: 409, Body: []byte("conflict")}, true},
		{"not-found-404", gophercloud.ErrUnexpectedResponseCode{Actual: 404, Body: []byte("nope")}, false},
		{"update-failed", fmt.Errorf("cluster entered UPDATE_FAILED: stack create failed"), false},
		{"conn-reset", fmt.Errorf("dial tcp: connection reset by peer"), true},
	}
	for _, tc := range cases {
		if got := retryableMutationErr(tc.err); got != tc.want {
			t.Errorf("%s: retryableMutationErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}
