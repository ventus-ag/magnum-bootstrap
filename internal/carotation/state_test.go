package carotation

import "testing"

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
