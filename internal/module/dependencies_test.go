package module

import (
	"fmt"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
)

func TestDependenciesReferToRegisteredModules(t *testing.T) {
	registry := BuildRegistry(config.Config{})
	for phaseID, mod := range registry {
		for _, depID := range mod.Dependencies() {
			if _, ok := registry[depID]; !ok {
				t.Errorf("%s depends on unregistered phase %q", phaseID, depID)
			}
		}
	}
}

func TestRolePlansHaveRegisteredAcyclicDependencies(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
	}{
		{name: "master", cfg: config.Config{Shared: config.SharedConfig{NodegroupRole: "master"}}},
		{name: "worker", cfg: config.Config{Shared: config.SharedConfig{NodegroupRole: "worker"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reconcilePlan := plan.Build(tc.cfg)
			if missing := MissingPhases(BuildRegistry(tc.cfg), reconcilePlan); len(missing) > 0 {
				t.Fatalf("plan has unregistered phases: %v", missing)
			}
			if err := validateAcyclic(reconcilePlan, BuildRegistry(tc.cfg)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMasterDependencyContracts(t *testing.T) {
	reconcilePlan := plan.Build(config.Config{Shared: config.SharedConfig{NodegroupRole: "master"}})
	registry := BuildRegistry(config.Config{})

	assertDependsOn(t, reconcilePlan, registry, "kube-master-config",
		"master-certificates",
		"cert-api-manager",
		"kube-os-config",
		"client-tools",
		"container-runtime",
		"stop-services",
	)
	assertDependsOn(t, reconcilePlan, registry, "storage", "container-runtime", "stop-services")
	assertDependsOn(t, reconcilePlan, registry, "proxy-env", "storage")
	assertDependsOn(t, reconcilePlan, registry, "services",
		"kube-master-config",
		"proxy-env",
		"storage",
		"container-runtime",
		"master-certificates",
		"etcd",
	)
	assertDependsOn(t, reconcilePlan, registry, "health", "services", "start-services", "proxy-env")
	assertDependsOn(t, reconcilePlan, registry, "cluster-rbac", "health")

	for _, addon := range []string{
		"cluster-flannel",
		"cluster-coredns",
		"cluster-occm",
		"cluster-cinder-csi",
		"cluster-manila-csi",
		"cluster-metrics-server",
		"cluster-dashboard",
		"cluster-auto-healer",
		"cluster-autoscaler",
	} {
		assertDependsOn(t, reconcilePlan, registry, addon, "cluster-rbac")
		assertDependsOn(t, reconcilePlan, registry, "cluster-health", addon)
	}
}

func TestWorkerDependencyContracts(t *testing.T) {
	reconcilePlan := plan.Build(config.Config{Shared: config.SharedConfig{NodegroupRole: "worker"}})
	registry := BuildRegistry(config.Config{})

	assertDependsOn(t, reconcilePlan, registry, "kube-worker-config",
		"worker-certificates",
		"kube-os-config",
		"client-tools",
		"container-runtime",
		"stop-services",
	)
	assertDependsOn(t, reconcilePlan, registry, "storage", "container-runtime", "stop-services")
	assertDependsOn(t, reconcilePlan, registry, "proxy-env", "storage")
	assertDependsOn(t, reconcilePlan, registry, "services",
		"kube-worker-config",
		"proxy-env",
		"storage",
		"container-runtime",
		"worker-certificates",
	)
	assertDependsOn(t, reconcilePlan, registry, "health", "services", "start-services", "proxy-env")
}

func assertDependsOn(t *testing.T, reconcilePlan plan.Plan, registry map[string]Module, phaseID string, expectedDeps ...string) {
	t.Helper()
	deps := transitiveDependencies(reconcilePlan, registry, phaseID)
	for _, depID := range expectedDeps {
		if !deps[depID] {
			t.Errorf("%s does not depend on %s", phaseID, depID)
		}
	}
}

func validateAcyclic(reconcilePlan plan.Plan, registry map[string]Module) error {
	phaseIDs := phaseSet(reconcilePlan)
	visiting := map[string]bool{}
	visited := map[string]bool{}

	var visit func(string) error
	visit = func(phaseID string) error {
		if visited[phaseID] {
			return nil
		}
		if visiting[phaseID] {
			return fmt.Errorf("dependency cycle includes %s", phaseID)
		}
		visiting[phaseID] = true
		if mod, ok := registry[phaseID]; ok {
			for _, depID := range mod.Dependencies() {
				if !phaseIDs[depID] {
					continue
				}
				if err := visit(depID); err != nil {
					return err
				}
			}
		}
		delete(visiting, phaseID)
		visited[phaseID] = true
		return nil
	}

	for _, phase := range reconcilePlan.Phases {
		if err := visit(phase.ID); err != nil {
			return err
		}
	}
	return nil
}

func transitiveDependencies(reconcilePlan plan.Plan, registry map[string]Module, phaseID string) map[string]bool {
	phaseIDs := phaseSet(reconcilePlan)
	deps := map[string]bool{}

	var visit func(string)
	visit = func(currentID string) {
		mod, ok := registry[currentID]
		if !ok {
			return
		}
		for _, depID := range mod.Dependencies() {
			if !phaseIDs[depID] || deps[depID] {
				continue
			}
			deps[depID] = true
			visit(depID)
		}
	}

	visit(phaseID)
	return deps
}

func phaseSet(reconcilePlan plan.Plan) map[string]bool {
	phaseIDs := make(map[string]bool, len(reconcilePlan.Phases))
	for _, phase := range reconcilePlan.Phases {
		phaseIDs[phase.ID] = true
	}
	return phaseIDs
}
