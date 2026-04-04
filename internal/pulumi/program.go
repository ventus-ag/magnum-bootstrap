package pulumi

import (
	"context"
	"fmt"
	"sync"

	gopulumi "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/module"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
)

// ProgramMetadata carries stack-level metadata for the Pulumi program.
type ProgramMetadata struct {
	ProjectName    string   `json:"projectName"`
	StackName      string   `json:"stackName"`
	Role           string   `json:"role"`
	Operation      string   `json:"operation"`
	Command        string   `json:"command"`
	Diff           bool     `json:"diff"`
	InputChecksum  string   `json:"inputChecksum"`
	HeatParamsFile string   `json:"heatParamsFile"`
	PhaseIDs       []string `json:"phaseIds"`
}

// NodeReconcile is the top-level Pulumi component resource for a node reconcile run.
type NodeReconcile struct {
	gopulumi.ResourceState
}

// RunAccumulator collects module execution results from within the Pulumi program
// so the reconciler can build a structured result after the automation API returns.
type RunAccumulator struct {
	mu           sync.Mutex
	phaseChanges []string
	changes      []host.Change
	outputs      map[string]string
	warnings     []string
	failedPhase  string
	failedErr    error
}

func NewRunAccumulator() *RunAccumulator {
	return &RunAccumulator{outputs: make(map[string]string)}
}

func (a *RunAccumulator) RecordPhase(phaseID string, res moduleapi.Result) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(res.Changes) > 0 {
		a.phaseChanges = append(a.phaseChanges, phaseID)
		a.changes = append(a.changes, res.Changes...)
	}
	a.warnings = append(a.warnings, res.Warnings...)
	for k, v := range res.Outputs {
		a.outputs[phaseID+"."+k] = v
	}
}

func (a *RunAccumulator) RecordFailure(phaseID string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failedErr == nil {
		a.failedPhase = phaseID
		a.failedErr = err
	}
}

func (a *RunAccumulator) HasFailure() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failedErr != nil
}

func (a *RunAccumulator) Failure() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failedPhase, a.failedErr
}

func (a *RunAccumulator) PhaseChanges() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.phaseChanges
}

func (a *RunAccumulator) Changes() []host.Change {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.changes
}

func (a *RunAccumulator) Outputs() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.outputs
}

func (a *RunAccumulator) Warnings() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.warnings
}

// BuildMetadata constructs the program metadata from config, plan, and run options.
func BuildMetadata(cfg config.Config, reconcilePlan plan.Plan, command string, diff bool, heatParamsFile string) ProgramMetadata {
	return ProgramMetadata{
		ProjectName:    "magnum-bootstrap",
		StackName:      cfg.StackName(),
		Role:           cfg.Role().String(),
		Operation:      cfg.Operation().String(),
		Command:        command,
		Diff:           diff,
		InputChecksum:  cfg.InputChecksum,
		HeatParamsFile: heatParamsFile,
		PhaseIDs:       reconcilePlan.PhaseIDs(),
	}
}

// BuildProgram returns a Pulumi RunFunc that:
//
//  1. Reads heat-params from heatParamsPath inside the RunFunc so the config
//     is loaded fresh on each Pulumi execution — Pulumi's diff mechanism then
//     detects desired-state changes via HeatParamsComponent outputs.
//  2. Registers a NodeReconcile root component.
//  3. Registers a HeatParamsComponent as its child — the relevant config
//     fields are stored as Pulumi outputs so the state JSON records what drove
//     each run and Pulumi can diff those values between runs.
//  4. For each phase: calls mod.Run() (imperative), accumulates results in acc,
//     then calls mod.Register() with heat as parent so every module component
//     is a Pulumi child of HeatParams in the resource tree.
//
// The apply flag on req is overridden by ctx.DryRun(): preview runs set
// Apply=false, up runs set Apply=true.
func BuildProgram(goCtx context.Context, heatParamsPath string, reconcilePlan plan.Plan, metadata ProgramMetadata, req moduleapi.Request, acc *RunAccumulator) gopulumi.RunFunc {
	return func(ctx *gopulumi.Context) error {
		// Load heat-params dynamically inside the Pulumi execution so the
		// program is self-contained and always reads the latest file contents.
		cfg, err := config.Load(heatParamsPath)
		if err != nil {
			return fmt.Errorf("failed to load heat params: %w", err)
		}

		// Let Pulumi determine whether this is a preview or an apply run.
		// DryRun() returns true during pulumi preview, false during pulumi up.
		actualReq := req
		actualReq.Apply = !ctx.DryRun()
		// Create a shared restart tracker for this run so config modules can
		// signal service restarts to the services module.
		actualReq.Restarts = moduleapi.NewRestartTracker()

		// Root component for this reconcile run.
		root := &NodeReconcile{}
		if err := ctx.RegisterComponentResource("magnum:index:NodeReconcile", metadata.StackName, root); err != nil {
			return err
		}

		// HeatParams is a child of root and the parent of all module components.
		// This creates the dependency chain:
		//   NodeReconcile → HeatParams → [prereq, client-tools, kube-os-config, ...]
		heat, err := moduleapi.NewHeatParamsComponent(
			ctx,
			metadata.StackName+"-heat-params",
			cfg,
			gopulumi.Parent(root),
		)
		if err != nil {
			return err
		}

		registry := module.BuildRegistry(cfg)
		executed := make(gopulumi.StringArray, 0, len(reconcilePlan.Phases))
		missing := make(gopulumi.StringArray, 0)

		// DAG-based dependency resolution: each module declares its
		// dependencies via Dependencies(). Modules whose dependencies are
		// all satisfied can run in parallel via Pulumi's engine.
		phaseResources := make(map[string]gopulumi.Resource)

		for _, phase := range reconcilePlan.Phases {
			mod, ok := registry[phase.ID]
			if !ok {
				missing = append(missing, gopulumi.String(phase.ID))
				continue
			}

			if actualReq.Logger != nil {
				actualReq.Logger.Infof("running phase=%s apply=%t", phase.ID, actualReq.Apply)
			}

			// Imperative execution: Run() does the actual host operations.
			phaseResult, err := mod.Run(goCtx, cfg, actualReq)
			if err != nil {
				acc.RecordFailure(phase.ID, err)
				return err
			}
			acc.RecordPhase(phase.ID, phaseResult)

			// Build Pulumi DependsOn from the module's declared dependencies.
			regOpts := []gopulumi.ResourceOption{gopulumi.Parent(heat)}
			var deps []gopulumi.Resource
			for _, depID := range mod.Dependencies() {
				if depRes, ok := phaseResources[depID]; ok && depRes != nil {
					deps = append(deps, depRes)
				}
			}
			if len(deps) > 0 {
				regOpts = append(regOpts, gopulumi.DependsOn(deps))
			}

			phaseRes, err := mod.Register(ctx, metadata.StackName+"-"+phase.ID, heat, regOpts...)
			if err != nil {
				return err
			}
			phaseResources[phase.ID] = phaseRes
			executed = append(executed, gopulumi.String(phase.ID))
		}

		return ctx.RegisterResourceOutputs(root, gopulumi.Map{
			"projectName":    gopulumi.String(metadata.ProjectName),
			"stackName":      gopulumi.String(metadata.StackName),
			"role":           gopulumi.String(metadata.Role),
			"operation":      gopulumi.String(metadata.Operation),
			"command":        gopulumi.String(metadata.Command),
			"diff":           gopulumi.Bool(metadata.Diff),
			"inputChecksum":  gopulumi.String(metadata.InputChecksum),
			"heatParamsFile": gopulumi.String(metadata.HeatParamsFile),
			"executedPhases": executed,
			"missingPhases":  missing,
		})
	}
}
