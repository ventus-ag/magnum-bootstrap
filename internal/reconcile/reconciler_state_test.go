package reconcile

import (
	"path/filepath"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/carotation"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

// useTempMarker points the finalize marker at a per-test path that does not
// exist, so effectiveCARotationStateID falls back to the previous state ID.
func useTempMarker(t *testing.T) string {
	t.Helper()
	orig := carotation.MarkerPath
	path := filepath.Join(t.TempDir(), "last_ca_rotation_id")
	carotation.MarkerPath = path
	t.Cleanup(func() { carotation.MarkerPath = orig })
	return path
}

func TestEffectiveCARotationStateIDReturnsPreviousWithoutMarker(t *testing.T) {
	useTempMarker(t)
	cfg := config.Config{Trigger: config.TriggerConfig{CARotationID: "rotate-456"}}

	// No finalize marker → an in-flight rotation must NOT be recorded as
	// applied; the previous state ID is preserved so the next run resumes.
	got := effectiveCARotationStateID(cfg, "rotate-123")
	if got != "rotate-123" {
		t.Fatalf("expected previous rotation id without marker, got %q", got)
	}
}

func TestEffectiveCARotationStateIDReturnsMarkerWhenFinalized(t *testing.T) {
	path := useTempMarker(t)
	if err := carotation.WriteMarker("rotate-456"); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	_ = path

	cfg := config.Config{Trigger: config.TriggerConfig{CARotationID: "rotate-456"}}
	got := effectiveCARotationStateID(cfg, "rotate-123")
	if got != "rotate-456" {
		t.Fatalf("expected finalized marker id, got %q", got)
	}
}

func TestEffectiveCARotationStateIDPreservesPreviousForResize(t *testing.T) {
	useTempMarker(t)
	cfg := config.Config{
		Shared:  config.SharedConfig{IsResize: true},
		Trigger: config.TriggerConfig{CARotationID: "stale-rotate-id"},
	}

	got := effectiveCARotationStateID(cfg, "rotate-123")
	if got != "rotate-123" {
		t.Fatalf("expected previous rotation id, got %q", got)
	}
}

func TestEffectiveCARotationStateIDTrimsWhitespace(t *testing.T) {
	useTempMarker(t)
	cfg := config.Config{Trigger: config.TriggerConfig{CARotationID: "rotate-456"}}

	got := effectiveCARotationStateID(cfg, " rotate-123 ")
	if got != "rotate-123" {
		t.Fatalf("expected trimmed previous rotation id, got %q", got)
	}
}
