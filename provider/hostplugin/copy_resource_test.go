package hostplugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pulumi/pulumi-go-provider/infer"
)

func TestReadSourceMissingTolerant(t *testing.T) {
	_, sha, exists, err := readSource(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("readSource on a missing file must not error: %v", err)
	}
	if exists || sha != "" {
		t.Fatalf("missing source should be exists=false sha=\"\", got exists=%v sha=%q", exists, sha)
	}
}

func TestReadSourcePresent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, sha, exists, err := readSource(p)
	if err != nil || !exists || sha == "" {
		t.Fatalf("present source: exists=%v sha=%q err=%v", exists, sha, err)
	}
}

func TestCopyStateFromSpecMissingSource(t *testing.T) {
	spec, err := copySpec(CopyArgs{
		Source: filepath.Join(t.TempDir(), "absent-helm"),
		Path:   filepath.Join(t.TempDir(), "dest"),
		Mode:   "0755",
	})
	if err != nil {
		t.Fatalf("copySpec: %v", err)
	}
	state, err := copyStateFromSpec(spec)
	if err != nil {
		t.Fatalf("copyStateFromSpec must tolerate a missing source (preview), got: %v", err)
	}
	if state.SourceSHA256 != "" {
		t.Errorf("missing source should yield empty SourceSHA256, got %q", state.SourceSHA256)
	}
}

// TestCopyCreateDryRunMissingSource is the regression for the preview wedge:
// `bootstrap preview` on a fresh node failed at client-tools-helm-bin-copy
// because Copy.Create read its source (a helm binary produced by an upstream
// ExtractTar that has not applied yet during preview). Create with DryRun must
// not require the source to exist.
func TestCopyCreateDryRunMissingSource(t *testing.T) {
	dir := t.TempDir()
	args := CopyArgs{
		Source: filepath.Join(dir, "helm"), // never created
		Path:   filepath.Join(dir, "bin", "helm"),
		Mode:   "0755",
	}
	if _, err := (&Copy{}).Create(context.Background(), infer.CreateRequest[CopyArgs]{Inputs: args, DryRun: true}); err != nil {
		t.Fatalf("Copy.Create dry-run must not fail on a missing source: %v", err)
	}
}
