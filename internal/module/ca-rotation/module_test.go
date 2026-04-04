package carotation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

func TestLatestAppliedCARotationIDPrefersMarkerFile(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "last_ca_rotation_id")
	if err := os.WriteFile(marker, []byte("marker-id\n"), 0o644); err != nil {
		t.Fatalf("failed to write marker file: %v", err)
	}

	got, err := latestAppliedCARotationID(marker, "state-id")
	if err != nil {
		t.Fatalf("latestAppliedCARotationID returned error: %v", err)
	}
	if got != "marker-id" {
		t.Fatalf("expected marker id, got %q", got)
	}
}

func TestLatestAppliedCARotationIDFallsBackToState(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "missing")

	got, err := latestAppliedCARotationID(marker, "state-id")
	if err != nil {
		t.Fatalf("latestAppliedCARotationID returned error: %v", err)
	}
	if got != "state-id" {
		t.Fatalf("expected state id fallback, got %q", got)
	}
}

func TestModuleRunSkipsNonPureCARotation(t *testing.T) {
	cfg := config.Config{
		Shared: config.SharedConfig{
			IsResize: true,
		},
		Trigger: config.TriggerConfig{
			CARotationID: "rotate-123",
		},
	}

	res, err := (Module{}).Run(context.Background(), cfg, moduleapi.Request{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no changes for non-pure ca rotation, got %d", len(res.Changes))
	}
}

func TestModuleRunSkipsWhenRotationAlreadyAppliedFromState(t *testing.T) {
	cfg := config.Config{
		Trigger: config.TriggerConfig{
			CARotationID: "rotate-123",
		},
	}

	res, err := (Module{}).Run(context.Background(), cfg, moduleapi.Request{
		PreviousCARotationID: "rotate-123",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no changes for already applied ca rotation, got %d", len(res.Changes))
	}
}
