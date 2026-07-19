package plan

import "github.com/ventus-ag/magnum-bootstrap/internal/config"

type Phase struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	Disruptive bool   `json:"disruptive"`
	// SkipRun marks a phase whose imperative Run() must not execute in this
	// run. The phase still registers its Pulumi resources so the program
	// mirrors the full plan and untargeted resources are never deleted from
	// state (a targeted run additionally scopes the apply via pulumi --target).
	SkipRun bool `json:"skipRun,omitempty"`
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

// LimitRunToPhase returns a plan that keeps every phase (so the Pulumi
// program still registers the complete resource tree) but marks all phases
// except the given one as SkipRun. Reports whether the phase exists in the
// plan.
func (p Plan) LimitRunToPhase(phaseID string) (Plan, bool) {
	found := false
	limited := make([]Phase, len(p.Phases))
	for i, phase := range p.Phases {
		phase.SkipRun = phase.ID != phaseID
		if phase.ID == phaseID {
			found = true
		}
		limited[i] = phase
	}
	return Plan{
		Role:      p.Role,
		Operation: p.Operation,
		Phases:    limited,
	}, found
}

// RunTarget returns the single phase ID whose Run() is enabled when the plan
// was limited with LimitRunToPhase, or "" for a normal full plan.
func (p Plan) RunTarget() string {
	target := ""
	skipped := false
	for _, phase := range p.Phases {
		if phase.SkipRun {
			skipped = true
			continue
		}
		if target != "" {
			return ""
		}
		target = phase.ID
	}
	if !skipped {
		return ""
	}
	return target
}

func newPhase(id, summary string, disruptive bool) Phase {
	return Phase{
		ID:         id,
		Summary:    summary,
		Disruptive: disruptive,
	}
}
