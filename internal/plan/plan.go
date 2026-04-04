package plan

import "github.com/ventus-ag/magnum-bootstrap/internal/config"

type Phase struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	Disruptive bool   `json:"disruptive"`
}

type Plan struct {
	Role      config.Role      `json:"role"`
	Operation config.Operation `json:"operation"`
	Phases    []Phase          `json:"phases"`
}

// Build returns the reconcile plan for the given config.  Every operation
// (create, upgrade, resize, ca-rotate, periodic drift correction) uses the
// same unified phase list.  Each module internally checks desired vs current
// state and only acts when something actually needs changing.
func Build(cfg config.Config) Plan {
	role := cfg.Role()

	var phases []Phase
	switch role {
	case config.RoleMaster:
		phases = masterPhases()
	case config.RoleWorker:
		phases = workerPhases()
	default:
		phases = []Phase{
			newPhase("prereq-validation", "validate desired node input and prerequisites", false),
			newPhase("health", "verify node role detection and input normalization", false),
		}
	}

	return Plan{
		Role:      role,
		Operation: cfg.Operation(),
		Phases:    phases,
	}
}

func (p Plan) PhaseIDs() []string {
	ids := make([]string, 0, len(p.Phases))
	for _, phase := range p.Phases {
		ids = append(ids, phase.ID)
	}
	return ids
}

// FilterToPhase returns a plan containing only the phase with the given ID.
// If no phase matches, the returned plan has no phases.
func (p Plan) FilterToPhase(phaseID string) Plan {
	filtered := make([]Phase, 0, 1)
	for _, phase := range p.Phases {
		if phase.ID == phaseID {
			filtered = append(filtered, phase)
		}
	}
	return Plan{
		Role:      p.Role,
		Operation: p.Operation,
		Phases:    filtered,
	}
}

func newPhase(id, summary string, disruptive bool) Phase {
	return Phase{
		ID:         id,
		Summary:    summary,
		Disruptive: disruptive,
	}
}
