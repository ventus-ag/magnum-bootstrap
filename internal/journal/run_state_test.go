package journal

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverInterruptedCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconciler-run.json")
	if err := os.WriteFile(path, []byte(`{"status":"runn`), 0o600); err != nil {
		t.Fatal(err)
	}

	state, recovered, err := RecoverInterrupted(path)
	if err != nil {
		t.Fatalf("corrupt journal must recover, got error: %v", err)
	}
	if !recovered {
		t.Fatal("expected recovered=true for corrupt journal")
	}
	if state.Status != "interrupted" {
		t.Fatalf("expected status interrupted, got %q", state.Status)
	}

	// The rewritten file must parse and future runs must proceed normally.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("rewritten journal must load: %v", err)
	}
	if reloaded.Status != "interrupted" {
		t.Fatalf("expected reloaded status interrupted, got %q", reloaded.Status)
	}
}

func TestLoadCorruptReturnsErrCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconciler-run.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("expected ErrCorrupt, got %v", err)
	}
}

func TestMarkCompletedToleratesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reconciler-run.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MarkCompleted(path, "run-once", "done"); err != nil {
		t.Fatalf("MarkCompleted must tolerate corrupt file: %v", err)
	}
	state, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "completed" {
		t.Fatalf("expected completed, got %q", state.Status)
	}
}
