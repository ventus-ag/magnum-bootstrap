package pulumi

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/module"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
)

type phaseRunResult struct {
	phaseID string
	result  moduleapi.Result
	err     error
}

type phaseTask struct {
	phase plan.Phase
	mod   module.Module
}

type phaseRunHandler func(phase plan.Phase, mod module.Module, runResult phaseRunResult) error

// runPhaseDAG executes module Run() methods according to their declared
// Dependencies(). Dependencies outside the selected plan are ignored, matching
// the Pulumi DependsOn construction used for resource registration.
func runPhaseDAG(ctx context.Context, phases []plan.Phase, registry map[string]module.Module, cfg config.Config, req moduleapi.Request, parallelism int, onPhaseDone phaseRunHandler) (map[string]phaseRunResult, error) {
	if parallelism < 1 {
		parallelism = config.AutoParallelism()
	}

	phaseIndex := make(map[string]int, len(phases))
	tasks := make(map[string]phaseTask, len(phases))
	for i, phase := range phases {
		phaseIndex[phase.ID] = i
		if mod, ok := registry[phase.ID]; ok {
			tasks[phase.ID] = phaseTask{phase: phase, mod: mod}
		}
	}

	dependents := make(map[string][]string, len(tasks))
	remainingDeps := make(map[string]int, len(tasks))
	ready := make([]string, 0, len(tasks))
	for _, phase := range phases {
		task, ok := tasks[phase.ID]
		if !ok {
			continue
		}
		for _, depID := range task.mod.Dependencies() {
			if _, ok := tasks[depID]; !ok {
				continue
			}
			remainingDeps[phase.ID]++
			dependents[depID] = append(dependents[depID], phase.ID)
		}
		if remainingDeps[phase.ID] == 0 {
			ready = append(ready, phase.ID)
		}
	}

	sortPhaseIDs(ready, phaseIndex)
	for phaseID := range dependents {
		sortPhaseIDs(dependents[phaseID], phaseIndex)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(map[string]phaseRunResult, len(tasks))
	resultCh := make(chan phaseRunResult)
	running := 0
	stopScheduling := false
	var runFailure error
	var hardErr error

	for {
		for !stopScheduling && running < parallelism && len(ready) > 0 {
			phaseID := ready[0]
			ready = ready[1:]
			task := tasks[phaseID]
			running++

			go func(phaseID string, task phaseTask) {
				if req.Logger != nil {
					req.Logger.Infof("running phase=%s apply=%t", phaseID, req.Apply)
				}
				res, err := runModuleWithRetry(runCtx, cfg, req, phaseID, task.mod)
				resultCh <- phaseRunResult{phaseID: phaseID, result: res, err: err}
			}(phaseID, task)
		}

		if running == 0 {
			break
		}

		runResult := <-resultCh
		running--
		results[runResult.phaseID] = runResult

		releaseDependents := true
		if onPhaseDone != nil {
			task := tasks[runResult.phaseID]
			if err := onPhaseDone(task.phase, task.mod, runResult); err != nil {
				if hardErr == nil {
					hardErr = err
				}
				stopScheduling = true
				releaseDependents = false
				cancel()
			}
		}

		if runResult.err != nil {
			if runFailure == nil {
				runFailure = runResult.err
			}
			if !req.AllowPartial {
				stopScheduling = true
				releaseDependents = false
				cancel()
			}
		}

		if releaseDependents {
			for _, dependentID := range dependents[runResult.phaseID] {
				remainingDeps[dependentID]--
				if remainingDeps[dependentID] == 0 {
					ready = append(ready, dependentID)
				}
			}
			sortPhaseIDs(ready, phaseIndex)
		}
	}

	if hardErr != nil {
		return results, hardErr
	}
	if runFailure != nil {
		if req.AllowPartial {
			return results, nil
		}
		return results, runFailure
	}
	if len(results) != len(tasks) {
		return results, fmt.Errorf("phase dependency graph could not be completed: ran %d of %d phase(s)", len(results), len(tasks))
	}
	return results, nil
}

func sortPhaseIDs(ids []string, phaseIndex map[string]int) {
	sort.SliceStable(ids, func(i, j int) bool {
		return phaseIndex[ids[i]] < phaseIndex[ids[j]]
	})
}

// runModuleWithRetry runs a module's Run() honoring its effective RetryPolicy.
// A transient failure is retried silently (with a bounded delay) before the
// error is surfaced; only the final attempt's failure reaches the accumulator
// and Heat. Modules are idempotent by design, so re-running Run() is safe. The
// delay respects ctx cancellation so a module never sleeps through retries after
// a sibling failure has already doomed the run.
func runModuleWithRetry(ctx context.Context, cfg config.Config, req moduleapi.Request, phaseID string, mod module.Module) (moduleapi.Result, error) {
	policy := moduleapi.ResolveRetryPolicy(mod)
	var res moduleapi.Result
	var err error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		res, err = mod.Run(ctx, cfg, req)
		if err == nil || attempt == policy.MaxAttempts {
			return res, err
		}
		if req.Logger != nil {
			req.Logger.Warnf("phase=%s attempt %d/%d failed: %v; retrying in %s",
				phaseID, attempt, policy.MaxAttempts, err, policy.Delay)
		}
		if policy.Delay <= 0 {
			if ctx.Err() != nil {
				return res, err // run already cancelled by a sibling failure
			}
			continue
		}
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(policy.Delay):
		}
	}
	return res, err
}
