package clienttools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

func TestBinaryNeedsReconcile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(target, []byte("abc"), 0o755); err != nil {
		t.Fatalf("failed to write target: %v", err)
	}
	sum, err := host.FileSHA256(target)
	if err != nil {
		t.Fatalf("failed to compute checksum: %v", err)
	}

	if binaryNeedsReconcile(target, "https://example/kubectl", "https://example/kubectl", sum) {
		t.Fatalf("expected no reconcile when url and checksum match")
	}
	if !binaryNeedsReconcile(target, "https://example/new", "https://example/old", sum) {
		t.Fatalf("expected reconcile when url changes")
	}
	if !binaryNeedsReconcile(target, "https://example/kubectl", "https://example/kubectl", "bad") {
		t.Fatalf("expected reconcile when checksum drifts")
	}
	if !binaryNeedsReconcile(filepath.Join(dir, "missing"), "https://example/kubectl", "https://example/kubectl", sum) {
		t.Fatalf("expected reconcile when file is missing")
	}
}
