package carotation

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestRotationParked(t *testing.T) {
	defer SetBaseDir(t.TempDir())()

	if parked, err := RotationParked(""); err != nil || parked {
		t.Fatalf("empty rotation id is never parked, got %v %v", parked, err)
	}
	if parked, err := RotationParked("rot-1"); err != nil || parked {
		t.Fatalf("no state means not parked, got %v %v", parked, err)
	}

	// A rotation that is genuinely mid-flight must NOT read as parked, or the
	// cert heals would run while the protocol is still moving material.
	if err := SaveState(State{RotationID: "rot-1", Phase: PhaseCutover}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if parked, _ := RotationParked("rot-1"); parked {
		t.Error("cutover without Held must not read as parked")
	}

	if err := SaveState(State{RotationID: "rot-1", Phase: PhaseCutover, Held: true}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if parked, err := RotationParked("rot-1"); err != nil || !parked {
		t.Errorf("held at cutover must read as parked, got %v %v", parked, err)
	}

	// A different id must not inherit the parked state.
	if parked, _ := RotationParked("rot-2"); parked {
		t.Error("parked state must be scoped to its own rotation id")
	}

	// Once finalized it is no longer parked.
	if err := SaveState(State{RotationID: "rot-1", Phase: PhaseDone, Held: true}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if parked, _ := RotationParked("rot-1"); parked {
		t.Error("a finalized rotation is not parked")
	}
}

func TestInProgressRotation(t *testing.T) {
	defer SetBaseDir(t.TempDir())()

	if st, err := InProgressRotation(""); err != nil || st.RotationID != "" {
		t.Fatalf("no staging means nothing in progress, got %+v %v", st, err)
	}

	if err := SaveState(State{RotationID: "rot-1", Phase: PhasePrepare}); err != nil {
		t.Fatal(err)
	}
	st, err := InProgressRotation("")
	if err != nil || st.RotationID != "rot-1" || st.Phase != PhasePrepare {
		t.Fatalf("prepare must be resumable, got %+v %v", st, err)
	}

	// The finalize→cleanup window: the id is already recorded as applied, so it
	// must not be picked up again.
	if st, _ := InProgressRotation("rot-1"); st.RotationID != "" {
		t.Errorf("an already-applied rotation must not resume, got %+v", st)
	}

	// A parked rotation is still owed: releasing the hold has to finalize it even
	// if heat-params lost the token meanwhile.
	if err := SaveState(State{RotationID: "rot-1", Phase: PhaseCutover, Held: true}); err != nil {
		t.Fatal(err)
	}
	if st, _ := InProgressRotation(""); st.RotationID != "rot-1" {
		t.Errorf("a parked rotation must stay resumable, got %+v", st)
	}

	if err := SaveState(State{RotationID: "rot-1", Phase: PhaseDone}); err != nil {
		t.Fatal(err)
	}
	if st, _ := InProgressRotation(""); st.RotationID != "" {
		t.Errorf("a finished rotation must not resume, got %+v", st)
	}
}

func TestStagedRotationIDs(t *testing.T) {
	defer SetBaseDir(t.TempDir())()

	if ids := StagedRotationIDs(); len(ids) != 0 {
		t.Fatalf("no staging dirs yet, got %v", ids)
	}
	for _, id := range []string{"rot-old", "rot-new"} {
		if err := os.MkdirAll(StagingDir(id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Newest first, so a resumed node adopts the most recent staged rotation.
	if err := os.Chtimes(StagingDir("rot-old"), time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	ids := StagedRotationIDs()
	if len(ids) != 2 || ids[0] != "rot-new" {
		t.Fatalf("StagedRotationIDs = %v; want rot-new first", ids)
	}
}

func useTempMarker(t *testing.T) {
	t.Helper()
	orig := MarkerPath
	MarkerPath = t.TempDir() + "/last_ca_rotation_id"
	t.Cleanup(func() { MarkerPath = orig })
}

func TestFinalizedRemembersOlderRotations(t *testing.T) {
	useTempMarker(t)

	if Finalized("rot-1") || Finalized("") {
		t.Fatal("nothing finalized yet")
	}
	for _, id := range []string{"rot-1", "rot-2", "rot-2", "rot-3"} {
		if err := WriteMarker(id); err != nil {
			t.Fatalf("WriteMarker(%s): %v", id, err)
		}
	}
	if ReadMarker() != "rot-3" {
		t.Fatalf("marker = %q, want latest rot-3", ReadMarker())
	}
	for _, id := range []string{"rot-1", "rot-2", "rot-3"} {
		if !Finalized(id) {
			t.Errorf("%s finalized earlier must stay finalized", id)
		}
	}
	if Finalized("rot-4") {
		t.Error("rot-4 never finalized")
	}
	if got := readFinalized(); len(got) != 3 {
		t.Errorf("history = %v, want 3 unique ids", got)
	}
}

func TestFinalizedHistoryIsBounded(t *testing.T) {
	useTempMarker(t)
	for i := range finalizedKeep + 5 {
		if err := WriteMarker(fmt.Sprintf("rot-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(readFinalized()); got != finalizedKeep {
		t.Fatalf("history len = %d, want %d", got, finalizedKeep)
	}
	if Finalized("rot-0") {
		t.Error("oldest id must be pruned")
	}
	if !Finalized(fmt.Sprintf("rot-%d", finalizedKeep+4)) {
		t.Error("newest id must be kept")
	}
}

func TestFinalizedMarkerOnlyLegacyNode(t *testing.T) {
	useTempMarker(t)
	// A node finalized by a binary predating the history only has the marker.
	if err := os.WriteFile(MarkerPath, []byte("rot-7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Finalized("rot-7") {
		t.Error("marker alone must count as finalized")
	}
}

func BenchmarkFinalizedFullHistory(b *testing.B) {
	orig := MarkerPath
	MarkerPath = b.TempDir() + "/last_ca_rotation_id"
	defer func() { MarkerPath = orig }()
	for i := range finalizedKeep {
		if err := WriteMarker(fmt.Sprintf("rot-%d", i)); err != nil {
			b.Fatal(err)
		}
	}
	for b.Loop() {
		Finalized("rot-0")
	}
}
