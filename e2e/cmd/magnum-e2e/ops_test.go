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
