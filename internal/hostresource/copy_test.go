package hostresource

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

// TestCopySpecApplyDryRunInSync guards the idempotency bug where a dry-run
// (preview) copy always reported a "replace" even when the destination already
// matched the source — making every preview/periodic run show a phantom change.
func TestCopySpecApplyDryRunInSync(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	content := []byte("ca-bundle-contents\n")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	spec := CopySpec{Source: src, Path: dst, Mode: 0o644}
	dryRun := host.NewExecutor(false, nil)

	// Destination missing → planned copy.
	res, err := spec.Apply(dryRun)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("missing dest: expected a planned change")
	}

	// Materialize the destination identical to source.
	if err := os.WriteFile(dst, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// In-sync → NO change in dry-run (the bug previously reported a replace).
	res, err = spec.Apply(dryRun)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || len(res.Changes) != 0 {
		t.Fatalf("in-sync dest: expected no change, got %+v", res.Changes)
	}

	// Content drift → change reported.
	if err := os.WriteFile(dst, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = spec.Apply(dryRun)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatalf("drifted dest: expected a change")
	}
}
