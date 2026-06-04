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

func TestFilterToPhase(t *testing.T) {
	p := Build(masterConfig())
	one := p.FilterToPhase("etcd")
	if len(one.Phases) != 1 || one.Phases[0].ID != "etcd" {
		t.Fatalf("FilterToPhase(etcd) = %v, want single etcd phase", one.PhaseIDs())
	}
	if one.Role != p.Role || one.Operation != p.Operation {
		t.Error("FilterToPhase must preserve role + operation")
	}
	none := p.FilterToPhase("does-not-exist")
	if len(none.Phases) != 0 {
		t.Errorf("FilterToPhase(missing) = %v, want empty", none.PhaseIDs())
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
