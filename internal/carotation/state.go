package carotation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// baseDir is the root for all per-rotation staging and state. It is a var so
// tests can redirect it away from /var/lib.
var baseDir = "/var/lib/magnum/ca-rotation"

// MarkerPath records the last fully finalized rotation ID. It is the
// authoritative completion signal: written only after finalize succeeds, and
// read by both the module (to skip a completed rotation) and the reconciler (to
// persist LastCARotationID only on real completion). It is a var for tests.
var MarkerPath = "/var/lib/magnum/last_ca_rotation_id"

// ReadMarker returns the last finalized rotation ID, or "" if none.
func ReadMarker() string {
	data, err := os.ReadFile(MarkerPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteMarker records rotationID as the last finalized rotation.
func WriteMarker(rotationID string) error {
	if err := os.MkdirAll(filepath.Dir(MarkerPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(MarkerPath, []byte(rotationID+"\n"), 0o644)
}

// trust/leaf mode constants recorded in State for observability and resume.
const (
	CAModeOld    = "old"
	CAModeBundle = "bundle"
	CAModeNew    = "new"

	LeafModeOld = "old"
	LeafModeNew = "new"
)

// State is the durable per-node protocol state. It is the source of truth for
// "where is this node" across reconciler runs; the Kubernetes ConfigMap only
// answers "may this node advance". State is written after every completed phase
// so an interrupted run resumes instead of restarting the rotation.
type State struct {
	RotationID   string `json:"rotationId"`
	Role         string `json:"role"`
	Instance     string `json:"instance"`
	Phase        Phase  `json:"phase"` // last phase completed locally ("" = none)
	CAMode       string `json:"caMode"`
	LeafMode     string `json:"leafMode"`
	SAVerifyMode string `json:"saVerifyMode"`
	SASignMode   string `json:"saSignMode"`
	UpdatedAt    string `json:"updatedAt"`
}

// StagingDir returns the per-rotation staging directory.
func StagingDir(rotationID string) string {
	return filepath.Join(baseDir, rotationID)
}

// OldDir, NewDir and BundleDir return the staging subdirectories that hold the
// snapshotted old material, the freshly generated new material, and the
// new+old CA bundle respectively.
func OldDir(rotationID string) string    { return filepath.Join(StagingDir(rotationID), "old") }
func NewDir(rotationID string) string    { return filepath.Join(StagingDir(rotationID), "new") }
func BundleDir(rotationID string) string { return filepath.Join(StagingDir(rotationID), "bundle") }

func statePath(rotationID string) string {
	return filepath.Join(StagingDir(rotationID), "state.json")
}

// LoadState reads the protocol state for a rotation. A missing file yields a
// zero State with no error so callers can treat "no state" as "not started".
func LoadState(rotationID string) (State, error) {
	data, err := os.ReadFile(statePath(rotationID))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("ca-rotation: read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("ca-rotation: parse state: %w", err)
	}
	return s, nil
}

// SaveState persists the protocol state, creating the staging directory if
// needed. The write is atomic (temp file + rename) so a crash mid-write cannot
// corrupt the resume point.
func SaveState(s State) error {
	if s.RotationID == "" {
		return errors.New("ca-rotation: cannot save state without rotation id")
	}
	if err := os.MkdirAll(StagingDir(s.RotationID), 0o700); err != nil {
		return fmt.Errorf("ca-rotation: create staging dir: %w", err)
	}
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := statePath(s.RotationID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("ca-rotation: write state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("ca-rotation: commit state: %w", err)
	}
	return nil
}
