// Package carotation implements the dual-CA, three-barrier rolling rotation
// protocol described in docs/ca-rotation-design.md. It owns the protocol's
// durable per-node state and the Kubernetes-API-based coordination layer
// (desired-phase ConfigMap, per-node status annotations, and a control-plane
// restart Lease).
package carotation

// Phase is one stage of the rotation protocol. Phases are strictly ordered and
// only ever advance forward.
type Phase string

const (
	// PhasePrepare writes the dual-CA bundle and the old+new SA verify keys
	// while keeping the old leaf certificates live.
	PhasePrepare Phase = "prepare"
	// PhaseCutover swaps live leaf certificates to the new CA and switches the
	// SA signing key, keeping bundle trust in place.
	PhaseCutover Phase = "cutover"
	// PhaseFinalize drops old trust: ca.crt and the SA verify set become
	// new-only.
	PhaseFinalize Phase = "finalize"
	// PhaseDone marks a fully finalized rotation in local state.
	PhaseDone Phase = "done"
)

// orderedPhases lists the protocol phases in advancement order.
var orderedPhases = []Phase{PhasePrepare, PhaseCutover, PhaseFinalize, PhaseDone}

// Index returns the position of the phase in the protocol order, or -1 when the
// phase is empty/unknown (treated as "before prepare").
func (p Phase) Index() int {
	for i, candidate := range orderedPhases {
		if candidate == p {
			return i
		}
	}
	return -1
}

// AtLeast reports whether p is the same as or later than other.
func (p Phase) AtLeast(other Phase) bool {
	return p.Index() >= other.Index()
}

// Next returns the phase that follows p. Next(PhaseFinalize) is PhaseDone and
// Next(PhaseDone) is PhaseDone.
func (p Phase) Next() Phase {
	idx := p.Index()
	if idx < 0 {
		return PhasePrepare
	}
	if idx+1 >= len(orderedPhases) {
		return PhaseDone
	}
	return orderedPhases[idx+1]
}

// Valid reports whether p is one of the protocol phases (excluding the empty
// value). PhaseDone is considered valid.
func (p Phase) Valid() bool {
	return p.Index() >= 0
}
