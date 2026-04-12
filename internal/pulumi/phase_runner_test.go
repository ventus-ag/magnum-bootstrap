package pulumi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gopulumi "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/module"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
)

type fakeModule struct {
	id   string
	deps []string
	run  func() error
}

func (m fakeModule) PhaseID() string        { return m.id }
func (m fakeModule) Dependencies() []string { return m.deps }
func (m fakeModule) Run(context.Context, config.Config, moduleapi.Request) (moduleapi.Result, error) {
	if m.run == nil {
		return moduleapi.Result{}, nil
	}
	return moduleapi.Result{}, m.run()
}
func (m fakeModule) Register(*gopulumi.Context, string, *moduleapi.HeatParamsComponent, ...gopulumi.ResourceOption) (gopulumi.Resource, error) {
	return nil, nil
}

func TestRunPhaseDAGRunsIndependentPhasesInParallel(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan error, 1)

	registry := map[string]module.Module{
		"a": fakeModule{id: "a", run: blockingRun("a", started, release)},
		"b": fakeModule{id: "b", run: blockingRun("b", started, release)},
	}

	go func() {
		_, err := runPhaseDAG(context.Background(), phases("a", "b"), registry, config.Config{}, moduleapi.Request{}, 2, nil)
		done <- err
	}()

	seen := map[string]bool{}
	timeout := time.After(500 * time.Millisecond)
	for len(seen) < 2 {
		select {
		case id := <-started:
			seen[id] = true
		case <-timeout:
			close(release)
			t.Fatalf("expected both independent phases to start before either was released; saw %v", seen)
		}
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runPhaseDAG returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runPhaseDAG did not finish")
	}
}

func TestRunPhaseDAGWaitsForDependencies(t *testing.T) {
	var aDone atomic.Bool
	registry := map[string]module.Module{
		"a": fakeModule{id: "a", run: func() error {
			time.Sleep(20 * time.Millisecond)
			aDone.Store(true)
			return nil
		}},
		"b": fakeModule{id: "b", deps: []string{"a"}, run: func() error {
			if !aDone.Load() {
				return errors.New("b ran before a completed")
			}
			return nil
		}},
	}

	_, err := runPhaseDAG(context.Background(), phases("a", "b"), registry, config.Config{}, moduleapi.Request{}, 10, nil)
	if err != nil {
		t.Fatalf("runPhaseDAG returned error: %v", err)
	}
}

func TestRunPhaseDAGReleasesDependentsAfterHandler(t *testing.T) {
	var mu sync.Mutex
	registered := map[string]bool{}
	registry := map[string]module.Module{
		"a": fakeModule{id: "a"},
		"b": fakeModule{id: "b", deps: []string{"a"}, run: func() error {
			mu.Lock()
			defer mu.Unlock()
			if !registered["a"] {
				return errors.New("b ran before a handler completed")
			}
			return nil
		}},
	}

	_, err := runPhaseDAG(context.Background(), phases("a", "b"), registry, config.Config{}, moduleapi.Request{}, 10,
		func(phase plan.Phase, _ module.Module, _ phaseRunResult) error {
			mu.Lock()
			defer mu.Unlock()
			registered[phase.ID] = true
			return nil
		})
	if err != nil {
		t.Fatalf("runPhaseDAG returned error: %v", err)
	}
}

func TestRunPhaseDAGStopsSchedulingOnFailure(t *testing.T) {
	fail := errors.New("boom")
	var bRan atomic.Bool
	registry := map[string]module.Module{
		"a": fakeModule{id: "a", run: func() error { return fail }},
		"b": fakeModule{id: "b", deps: []string{"a"}, run: func() error {
			bRan.Store(true)
			return nil
		}},
	}

	_, err := runPhaseDAG(context.Background(), phases("a", "b"), registry, config.Config{}, moduleapi.Request{}, 1, nil)
	if !errors.Is(err, fail) {
		t.Fatalf("expected failure %v, got %v", fail, err)
	}
	if bRan.Load() {
		t.Fatal("dependent phase ran after dependency failed")
	}
}

func TestRunPhaseDAGAllowPartialContinuesAfterFailure(t *testing.T) {
	fail := errors.New("boom")
	var bRan atomic.Bool
	registry := map[string]module.Module{
		"a": fakeModule{id: "a", run: func() error { return fail }},
		"b": fakeModule{id: "b", deps: []string{"a"}, run: func() error {
			bRan.Store(true)
			return nil
		}},
	}

	_, err := runPhaseDAG(context.Background(), phases("a", "b"), registry, config.Config{}, moduleapi.Request{AllowPartial: true}, 1, nil)
	if err != nil {
		t.Fatalf("allow-partial run returned error: %v", err)
	}
	if !bRan.Load() {
		t.Fatal("dependent phase did not run with allow-partial enabled")
	}
}

func TestRunPhaseDAGHandlerErrorIsFatalWithAllowPartial(t *testing.T) {
	handlerErr := errors.New("register failed")
	registry := map[string]module.Module{
		"a": fakeModule{id: "a"},
	}

	_, err := runPhaseDAG(context.Background(), phases("a"), registry, config.Config{}, moduleapi.Request{AllowPartial: true}, 1,
		func(plan.Phase, module.Module, phaseRunResult) error {
			return handlerErr
		})
	if !errors.Is(err, handlerErr) {
		t.Fatalf("expected handler error %v, got %v", handlerErr, err)
	}
}

func blockingRun(id string, started chan<- string, release <-chan struct{}) func() error {
	return func() error {
		started <- id
		<-release
		return nil
	}
}

func phases(ids ...string) []plan.Phase {
	out := make([]plan.Phase, 0, len(ids))
	for _, id := range ids {
		out = append(out, plan.Phase{ID: id})
	}
	return out
}
