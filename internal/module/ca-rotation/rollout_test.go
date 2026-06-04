package carotation

import (
	"os"
	"path/filepath"
	"testing"

	coord "github.com/ventus-ag/magnum-bootstrap/internal/carotation"
)

func TestCARolloutStampStableForSameCA(t *testing.T) {
	restore := coord.SetBaseDir(t.TempDir())
	defer restore()

	const id1, id2 = "rotate-1", "rotate-2"
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nsame-ca\n-----END CERTIFICATE-----\n")
	writeNewCA(t, id1, caPEM)
	writeNewCA(t, id2, caPEM)

	// Different rotation ids, identical CA content → identical rollout stamp,
	// so kubectl patch is a no-op and workloads are not re-rolled.
	if a, b := caRolloutStamp(id1), caRolloutStamp(id2); a != b {
		t.Fatalf("same CA produced different stamps: %q vs %q", a, b)
	}
}

func TestCARolloutStampChangesWithCA(t *testing.T) {
	restore := coord.SetBaseDir(t.TempDir())
	defer restore()

	writeNewCA(t, "rotate-1", []byte("ca-old"))
	writeNewCA(t, "rotate-2", []byte("ca-new"))

	if a, b := caRolloutStamp("rotate-1"), caRolloutStamp("rotate-2"); a == b {
		t.Fatalf("different CAs produced the same stamp: %q", a)
	}
}

func TestCARolloutStampFallsBackToRotationID(t *testing.T) {
	restore := coord.SetBaseDir(t.TempDir())
	defer restore()

	// No staged CA on disk → fall back to the rotation id (preserve the old
	// always-roll behaviour rather than skip a needed rollout).
	if got := caRolloutStamp("rotate-x"); got != "rotate-x" {
		t.Fatalf("expected fallback to rotation id, got %q", got)
	}
}

func writeNewCA(t *testing.T, rotationID string, pem []byte) {
	t.Helper()
	dir := coord.NewDir(rotationID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), pem, 0o444); err != nil {
		t.Fatal(err)
	}
}
