package carotation

import (
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
