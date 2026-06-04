package carotation

import "testing"

func TestPhaseOrdering(t *testing.T) {
	if !PhaseCutover.AtLeast(PhasePrepare) {
		t.Error("cutover should be at least prepare")
	}
	if PhasePrepare.AtLeast(PhaseCutover) {
		t.Error("prepare should not be at least cutover")
	}
	if !PhaseFinalize.AtLeast(PhaseFinalize) {
		t.Error("finalize should be at least itself")
	}
	// Empty phase is "before prepare".
	if Phase("").AtLeast(PhasePrepare) {
		t.Error("empty phase should be before prepare")
	}
}

func TestPhaseNext(t *testing.T) {
	cases := map[Phase]Phase{
		"":            PhasePrepare,
		PhasePrepare:  PhaseCutover,
		PhaseCutover:  PhaseFinalize,
		PhaseFinalize: PhaseDone,
		PhaseDone:     PhaseDone,
	}
	for in, want := range cases {
		if got := in.Next(); got != want {
			t.Errorf("Next(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPhaseValid(t *testing.T) {
	for _, p := range []Phase{PhasePrepare, PhaseCutover, PhaseFinalize, PhaseDone} {
		if !p.Valid() {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []Phase{"", "bogus"} {
		if Phase(p).Valid() {
			t.Errorf("%q should be invalid", p)
		}
	}
}
