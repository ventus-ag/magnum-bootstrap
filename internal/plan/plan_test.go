package plan

import (
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func masterConfig() config.Config {
	return config.Config{Shared: config.SharedConfig{NodegroupRole: "master"}}
}

func workerConfig() config.Config {
	return config.Config{Shared: config.SharedConfig{NodegroupRole: "worker"}}
}

func assertUniqueNonEmpty(t *testing.T, ids []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Error("phase with empty ID")
		}
		if seen[id] {
			t.Errorf("duplicate phase ID %q", id)
		}
		seen[id] = true
	}
}

func TestBuildMasterPlan(t *testing.T) {
	p := Build(masterConfig())
	if p.Role != config.RoleMaster {
		t.Fatalf("Role = %q, want master", p.Role)
	}
	if p.Operation != config.OperationCreate {
		t.Fatalf("Operation = %q, want create", p.Operation)
	}
	ids := p.PhaseIDs()
	if len(ids) != 31 {
		t.Errorf("master plan has %d phases, want 31: %v", len(ids), ids)
	}
	if ids[0] != "prereq-validation" {
		t.Errorf("first master phase = %q, want prereq-validation", ids[0])
	}
	assertUniqueNonEmpty(t, ids)
	// Cluster-addon phases must be present on master.
	for _, want := range []string{"cluster-coredns", "cluster-flannel", "zincati"} {
		if !p.has(want) {
			t.Errorf("master plan missing phase %q", want)
		}
	}
}

func TestBuildWorkerPlan(t *testing.T) {
	p := Build(workerConfig())
	if p.Role != config.RoleWorker {
		t.Fatalf("Role = %q, want worker", p.Role)
	}
	ids := p.PhaseIDs()
	if len(ids) != 17 {
		t.Errorf("worker plan has %d phases, want 17: %v", len(ids), ids)
	}
	if ids[0] != "prereq-validation" {
		t.Errorf("first worker phase = %q, want prereq-validation", ids[0])
	}
	assertUniqueNonEmpty(t, ids)
	// Workers must NOT run cluster-addon phases.
	if p.has("cluster-coredns") {
		t.Error("worker plan must not include cluster-coredns")
	}
}

func TestLimitRunToPhase(t *testing.T) {
	p := Build(masterConfig())
	limited, ok := p.LimitRunToPhase("etcd")
	if !ok {
		t.Fatal("LimitRunToPhase(etcd) reported not found")
	}
	if len(limited.Phases) != len(p.Phases) {
		t.Fatalf("limited plan has %d phases, want %d (all phases must stay registered)", len(limited.Phases), len(p.Phases))
	}
	if limited.Role != p.Role || limited.Operation != p.Operation {
		t.Error("LimitRunToPhase must preserve role + operation")
	}
	for _, phase := range limited.Phases {
		if phase.ID == "etcd" && phase.SkipRun {
			t.Error("targeted phase must not have SkipRun set")
		}
		if phase.ID != "etcd" && !phase.SkipRun {
			t.Errorf("phase %s must have SkipRun set", phase.ID)
		}
	}
	if got := limited.RunTarget(); got != "etcd" {
		t.Errorf("RunTarget() = %q, want etcd", got)
	}
	if got := p.RunTarget(); got != "" {
		t.Errorf("RunTarget() on full plan = %q, want empty", got)
	}

	if _, ok := p.LimitRunToPhase("does-not-exist"); ok {
		t.Error("LimitRunToPhase(missing) reported found")
	}
}

func TestUnknownRolePlan(t *testing.T) {
	p := Build(config.Config{})
	if p.Role != config.RoleUnknown {
		t.Fatalf("Role = %q, want unknown", p.Role)
	}
	if len(p.Phases) == 0 {
		t.Error("unknown-role plan should still carry validation phases")
	}
}

func (p Plan) has(id string) bool {
	for _, ph := range p.Phases {
		if ph.ID == id {
			return true
		}
	}
	return false
}
