package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewNode(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "reconciler-state.json")
	caCert := filepath.Join(dir, "ca.crt")

	if !newNode(stateFile, caCert) {
		t.Fatal("a node with no state and no CA is new")
	}
	if err := os.WriteFile(caCert, []byte("ca"), 0o644); err != nil {
		t.Fatal(err)
	}
	if newNode(stateFile, caCert) {
		t.Fatal("a legacy node with a CA but no reconciler state must still rotate")
	}
	if err := os.Remove(caCert); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if newNode(stateFile, caCert) {
		t.Fatal("a node the reconciler already ran on is not new")
	}
}
