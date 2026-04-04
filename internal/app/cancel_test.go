package app

import (
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/paths"
)

func TestIsNoRunningUpdateError(t *testing.T) {
	cases := []string{
		"failed to cancel update: no update is currently running",
		"error: No update in progress",
		"stack has no in-progress update",
	}

	for _, tc := range cases {
		if !isNoRunningUpdateError(assertError(tc)) {
			t.Fatalf("expected %q to be treated as no-running-update", tc)
		}
	}
}

func TestResolveCancelStackNamePrefersOverride(t *testing.T) {
	got, err := resolveCancelStackName(cancelFlags{stackName: "node-manual"}, paths.Paths{})
	if err != nil {
		t.Fatalf("resolveCancelStackName returned error: %v", err)
	}
	if got != "node-manual" {
		t.Fatalf("expected override stack name, got %q", got)
	}
}

type stringError string

func (e stringError) Error() string { return string(e) }

func assertError(text string) error { return stringError(text) }
