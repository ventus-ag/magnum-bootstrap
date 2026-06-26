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
	carotation "github.com/ventus-ag/magnum-bootstrap/internal/module/ca-rotation"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/storage"
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

// countingModule fails its first failUntil Run() calls, then succeeds, tracking
// the number of invocations so retry behavior can be asserted.
type countingModule struct {
	failUntil int
	calls     int
}

func (m *countingModule) PhaseID() string        { return "stub" }
func (m *countingModule) Dependencies() []string { return nil }
func (m *countingModule) Run(context.Context, config.Config, moduleapi.Request) (moduleapi.Result, error) {
	m.calls++
	if m.calls <= m.failUntil {
		return moduleapi.Result{}, errors.New("boom")
	}
	return moduleapi.Result{}, nil
}
func (m *countingModule) Register(*gopulumi.Context, string, *moduleapi.HeatParamsComponent, ...gopulumi.ResourceOption) (gopulumi.Resource, error) {
	return nil, nil
}

// retryOverride adds a per-module RetryPolicy override (Retryable interface).
type retryOverride struct {
	countingModule
	policy moduleapi.RetryPolicy
}

func (m *retryOverride) RetryPolicy() moduleapi.RetryPolicy { return m.policy }

func TestRunModuleWithRetrySucceedsAfterRetries(t *testing.T) {
	t.Setenv("MAGNUM_MODULE_MAX_ATTEMPTS", "3")
	t.Setenv("MAGNUM_MODULE_RETRY_DELAY_SECONDS", "0")
	mod := &countingModule{failUntil: 2}

	_, err := runModuleWithRetry(context.Background(), config.Config{}, moduleapi.Request{}, "stub", mod)
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if mod.calls != 3 {
		t.Fatalf("expected 3 Run() calls, got %d", mod.calls)
	}
}

func TestRunModuleWithRetryFailsAfterMaxAttempts(t *testing.T) {
	t.Setenv("MAGNUM_MODULE_MAX_ATTEMPTS", "2")
	t.Setenv("MAGNUM_MODULE_RETRY_DELAY_SECONDS", "0")
	mod := &countingModule{failUntil: 99}

	_, err := runModuleWithRetry(context.Background(), config.Config{}, moduleapi.Request{}, "stub", mod)
	if err == nil {
		t.Fatal("expected failure after exhausting attempts")
	}
	if mod.calls != 2 {
		t.Fatalf("expected 2 Run() calls, got %d", mod.calls)
	}
}

func TestRunModuleWithRetryNoRetryWhenSingleAttempt(t *testing.T) {
	t.Setenv("MAGNUM_MODULE_MAX_ATTEMPTS", "1")
	t.Setenv("MAGNUM_MODULE_RETRY_DELAY_SECONDS", "0")
	mod := &countingModule{failUntil: 99}

	_, err := runModuleWithRetry(context.Background(), config.Config{}, moduleapi.Request{}, "stub", mod)
	if err == nil {
		t.Fatal("expected failure")
	}
	if mod.calls != 1 {
		t.Fatalf("expected exactly 1 Run() call, got %d", mod.calls)
	}
}

func TestRunModuleWithRetryClampsOverrideBelowOne(t *testing.T) {
	// A module override of MaxAttempts<1 is clamped to a single attempt.
	t.Setenv("MAGNUM_MODULE_MAX_ATTEMPTS", "5")
	t.Setenv("MAGNUM_MODULE_RETRY_DELAY_SECONDS", "0")
	mod := &retryOverride{countingModule: countingModule{failUntil: 99}, policy: moduleapi.RetryPolicy{MaxAttempts: 0}}

	_, _ = runModuleWithRetry(context.Background(), config.Config{}, moduleapi.Request{}, "stub", mod)
	if mod.calls != 1 {
		t.Fatalf("expected override<1 to clamp to 1 Run() call, got %d", mod.calls)
	}
}

func TestRunModuleWithRetryIgnoresInvalidEnv(t *testing.T) {
	// env=0 is invalid (rejected by the n>=1 guard) → falls back to default 2.
	t.Setenv("MAGNUM_MODULE_MAX_ATTEMPTS", "0")
	t.Setenv("MAGNUM_MODULE_RETRY_DELAY_SECONDS", "0")
	mod := &countingModule{failUntil: 99}

	_, _ = runModuleWithRetry(context.Background(), config.Config{}, moduleapi.Request{}, "stub", mod)
	if mod.calls != 2 {
		t.Fatalf("expected invalid env to fall back to default 2 Run() calls, got %d", mod.calls)
	}
}

func TestRunModuleWithRetryPerModuleOverrideBeatsDefault(t *testing.T) {
	// Default would allow only a single attempt; the module override raises it.
	t.Setenv("MAGNUM_MODULE_MAX_ATTEMPTS", "1")
	t.Setenv("MAGNUM_MODULE_RETRY_DELAY_SECONDS", "0")
	mod := &retryOverride{countingModule: countingModule{failUntil: 2}, policy: moduleapi.RetryPolicy{MaxAttempts: 3}}

	_, err := runModuleWithRetry(context.Background(), config.Config{}, moduleapi.Request{}, "stub", mod)
	if err != nil {
		t.Fatalf("expected override to allow success, got %v", err)
	}
	if mod.calls != 3 {
		t.Fatalf("expected 3 Run() calls from override, got %d", mod.calls)
	}
}

func TestDisruptiveModulesOptOutOfRetry(t *testing.T) {
	// Even with a high global default, storage and ca-rotation must resolve to a
	// single attempt (their failures are deterministic + their waits are long).
	t.Setenv("MAGNUM_MODULE_MAX_ATTEMPTS", "5")
	for _, mod := range []module.Module{storage.Module{}, carotation.Module{}} {
		if p := moduleapi.ResolveRetryPolicy(mod); p.MaxAttempts != 1 {
			t.Errorf("%s: expected MaxAttempts=1 (opted out), got %d", mod.PhaseID(), p.MaxAttempts)
		}
	}
}

func TestRunModuleWithRetryCancelAbortsDelay(t *testing.T) {
	mod := &retryOverride{countingModule: countingModule{failUntil: 99}, policy: moduleapi.RetryPolicy{MaxAttempts: 5, Delay: time.Hour}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	_, err := runModuleWithRetry(ctx, config.Config{}, moduleapi.Request{}, "stub", mod)
	if err == nil {
		t.Fatal("expected failure")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("expected cancel to abort the retry delay, took %s", elapsed)
	}
	if mod.calls != 1 {
		t.Fatalf("expected 1 Run() call before cancel abort, got %d", mod.calls)
	}
}
