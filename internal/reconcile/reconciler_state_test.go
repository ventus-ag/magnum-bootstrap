package reconcile

import (
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func TestEffectiveCARotationStateIDUsesCurrentForPureRotation(t *testing.T) {
	cfg := config.Config{
		Trigger: config.TriggerConfig{
			CARotationID: "rotate-456",
		},
	}

	got := effectiveCARotationStateID(cfg, "rotate-123")
	if got != "rotate-456" {
		t.Fatalf("expected current rotation id, got %q", got)
	}
}

func TestEffectiveCARotationStateIDPreservesPreviousForResize(t *testing.T) {
	cfg := config.Config{
		Shared: config.SharedConfig{
			IsResize: true,
		},
		Trigger: config.TriggerConfig{
			CARotationID: "stale-rotate-id",
		},
	}

	got := effectiveCARotationStateID(cfg, "rotate-123")
	if got != "rotate-123" {
		t.Fatalf("expected previous rotation id, got %q", got)
	}
}

func TestEffectiveCARotationStateIDTrimsWhitespace(t *testing.T) {
	cfg := config.Config{
		Trigger: config.TriggerConfig{
			CARotationID: "  rotate-456  ",
		},
	}

	got := effectiveCARotationStateID(cfg, " rotate-123 ")
	if got != "rotate-456" {
		t.Fatalf("expected trimmed current rotation id, got %q", got)
	}
}
