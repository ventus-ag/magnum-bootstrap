package reconcile

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optrefresh"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/module"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/internal/paths"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
	pulumipkg "github.com/ventus-ag/magnum-bootstrap/internal/pulumi"
	"github.com/ventus-ag/magnum-bootstrap/internal/result"
	"github.com/ventus-ag/magnum-bootstrap/internal/state"
)

// Run executes a reconcile run using the Pulumi Automation API.
//
// For mode=="preview", it calls stack.Preview() which invokes the program with
// ctx.DryRun()==true so modules perform dry-run checks only.
// For mode=="up", it calls stack.Up() which invokes the program with
// ctx.DryRun()==false so modules apply actual changes.
//
// When refresh==true, stack.Refresh() is called first to reconcile Pulumi state
// against the current observed node state before preview or up.
//
// If eventCh is non-nil, Pulumi engine events are streamed to it for real-time
// display. The caller should drain the channel in a separate goroutine.
func Run(ctx context.Context, mode string, diff bool, refresh bool, parallelism int, cfg config.Config, runtimePaths paths.Paths, reconcilePlan plan.Plan, req moduleapi.Request, eventCh chan<- events.EngineEvent) (result.Result, state.State, error) {
	metadata := pulumipkg.BuildMetadata(cfg, reconcilePlan, mode, diff, runtimePaths.HeatParamsFile)
	acc := pulumipkg.NewRunAccumulator()
	program := pulumipkg.BuildProgram(ctx, runtimePaths.HeatParamsFile, reconcilePlan, metadata, req, acc)

	workspaceDir := filepath.Join(runtimePaths.PulumiStateDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return result.Result{}, state.State{}, fmt.Errorf("failed to create pulumi workspace dir: %w", err)
	}

	// Install the Pulumi CLI on first run (idempotent — reuses existing install).
	// This avoids needing to ship the pulumi binary separately.
	pulumiRoot := filepath.Join(runtimePaths.PulumiStateDir, "cli")
	pulumiCmd, err := auto.InstallPulumiCommand(ctx, &auto.PulumiCommandOptions{
		Root: pulumiRoot,
	})
	if err != nil {
		return result.Result{}, state.State{}, fmt.Errorf("failed to install pulumi cli: %w", err)
	}

	// Use the local file backend with an empty passphrase — no cloud auth and
	// no password required. Our stack carries no secrets so no encryption is needed.
	envVars := map[string]string{
		"PULUMI_CONFIG_PASSPHRASE": "",
	}
	if runtimePaths.PulumiBackend != "" {
		envVars["PULUMI_BACKEND_URL"] = runtimePaths.PulumiBackend
	}

	stack, err := auto.UpsertStackInlineSource(ctx,
		cfg.StackName(),
		"magnum-bootstrap",
		program,
		auto.Pulumi(pulumiCmd),
		auto.WorkDir(workspaceDir),
		auto.EnvVars(envVars),
	)
	if err != nil {
		return result.Result{}, state.State{}, fmt.Errorf("failed to initialize pulumi stack: %w", err)
	}

	if refresh {
		if req.Logger != nil {
			req.Logger.Infof("refreshing pulumi stack state stack=%s", cfg.StackName())
		}
		if _, err := stack.Refresh(ctx,
			optrefresh.ProgressStreams(io.Discard),
			optrefresh.ErrorProgressStreams(io.Discard),
		); err != nil {
			return result.Result{}, state.State{}, fmt.Errorf("failed to refresh stack state: %w", err)
		}
	}

	if parallelism < 1 {
		parallelism = 10
	}

	switch mode {
	case "preview":
		previewOpts := []optpreview.Option{
			optpreview.Diff(),
			optpreview.Parallel(parallelism),
			optpreview.ProgressStreams(io.Discard),
			optpreview.ErrorProgressStreams(io.Discard),
		}
		if eventCh != nil {
			previewOpts = append(previewOpts, optpreview.EventStreams(eventCh))
		}
		if _, err := stack.Preview(ctx, previewOpts...); err != nil {
			return extractFailureResult(acc, mode, cfg, runtimePaths, reconcilePlan, err)
		}

	case "up":
		upOpts := []optup.Option{
			optup.Parallel(parallelism),
			optup.ProgressStreams(io.Discard),
			optup.ErrorProgressStreams(io.Discard),
		}
		if diff {
			upOpts = append(upOpts, optup.Diff())
		}
		if eventCh != nil {
			upOpts = append(upOpts, optup.EventStreams(eventCh))
		}
		if _, err := stack.Up(ctx, upOpts...); err != nil {
			return extractFailureResult(acc, mode, cfg, runtimePaths, reconcilePlan, err)
		}

	default:
		return result.Result{}, state.State{}, fmt.Errorf("unknown reconcile mode: %s", mode)
	}

	if acc.HasFailure() {
		failedPhase, failedErr := acc.Failure()
		return buildModuleFailureResult(cfg, runtimePaths, failedPhase, failedErr, acc.Changes(), acc.Warnings()),
			attemptedState(cfg, reconcilePlan), failedErr
	}

	missing := module.MissingPhases(module.BuildRegistry(cfg), reconcilePlan)
	tolerateMissing := !req.Apply || req.AllowPartial
	warnings := acc.Warnings()

	if len(missing) > 0 {
		if tolerateMissing {
			warnings = append(warnings, fmt.Sprintf("skipped unimplemented phase(s): %s", strings.Join(missing, ", ")))
		} else {
			return buildMissingModulesResult(mode, cfg, runtimePaths, reconcilePlan, missing, acc.Changes(), warnings, acc.Outputs()),
				attemptedState(cfg, reconcilePlan),
				fmt.Errorf("missing modules: %s", strings.Join(missing, ", "))
		}
	}

	reconcileState := successfulState(cfg, reconcilePlan)
	res := buildSuccessResult(mode, diff, cfg, runtimePaths, reconcilePlan, acc.PhaseChanges(), acc.Changes(), warnings, acc.Outputs(), missing)
	return res, reconcileState, nil
}

// extractFailureResult returns a module-level failure if the accumulator captured one,
// or a generic pulumi-level failure otherwise.
func extractFailureResult(acc *pulumipkg.RunAccumulator, mode string, cfg config.Config, runtimePaths paths.Paths, reconcilePlan plan.Plan, runErr error) (result.Result, state.State, error) {
	if acc.HasFailure() {
		failedPhase, failedErr := acc.Failure()
		return buildModuleFailureResult(cfg, runtimePaths, failedPhase, failedErr, acc.Changes(), acc.Warnings()),
			attemptedState(cfg, reconcilePlan), failedErr
	}
	return result.Result{
		Status:    "failed",
		Step:      mode,
		Summary:   truncateError(runErr.Error(), 500),
		Reason:    truncateError(runErr.Error(), 500),
		ErrorCode: "pulumi_error",
		Details: map[string]string{
			"mode":    mode,
			"logFile": runtimePaths.LogFile,
		},
	}, attemptedState(cfg, reconcilePlan), runErr
}

func buildSuccessResult(mode string, _ bool, cfg config.Config, _ paths.Paths, reconcilePlan plan.Plan, phaseChanges []string, changeLog []host.Change, warnings []string, _ map[string]string, missing []string) result.Result {
	status := "succeeded"
	step := "apply"
	verb := "applied"
	if mode == "preview" {
		status = "planned"
		step = "preview"
		verb = "planned"
	}

	summary := fmt.Sprintf("%s %d implemented phase(s) for %s %s on %s (%d change(s))",
		verb, len(reconcilePlan.Phases)-len(missing), cfg.Role(), cfg.Operation(), cfg.Shared.InstanceName, len(changeLog))
	if len(missing) > 0 {
		summary += fmt.Sprintf("; skipped %d unimplemented phase(s)", len(missing))
	}

	details := map[string]string{
		"mode":          mode,
		"role":          cfg.Role().String(),
		"operation":     cfg.Operation().String(),
		"inputChecksum": cfg.InputChecksum,
		"changeCount":   fmt.Sprintf("%d", len(changeLog)),
	}
	if len(phaseChanges) > 0 {
		details["changedPhases"] = strings.Join(phaseChanges, ",")
	}
	if len(missing) > 0 {
		details["missingPhases"] = strings.Join(missing, ",")
	}

	return result.Result{
		Status:           status,
		Step:             step,
		Summary:          summary,
		Reason:           summary,
		Changed:          phaseChanges,
		Operations:       changeLog,
		Warnings:         warnings,
		Details:          details,
		DeployStatusCode: 0,
	}
}

func buildMissingModulesResult(mode string, cfg config.Config, _ paths.Paths, reconcilePlan plan.Plan, missing []string, changeLog []host.Change, warnings []string, _ map[string]string) result.Result {
	summary := fmt.Sprintf("missing module implementations for phase(s): %s", strings.Join(missing, ", "))
	details := map[string]string{
		"role":          cfg.Role().String(),
		"operation":     cfg.Operation().String(),
		"instance":      cfg.Shared.InstanceName,
		"missingPhases": strings.Join(missing, ","),
		"plannedPhases": strings.Join(reconcilePlan.PhaseIDs(), ","),
	}
	return result.Result{
		Status:     "failed",
		Step:       mode,
		Summary:    summary,
		Reason:     summary,
		Operations: changeLog,
		Warnings:   warnings,
		ErrorCode:  "module_missing",
		Details:    details,
	}
}

func buildModuleFailureResult(cfg config.Config, _ paths.Paths, phaseID string, err error, changeLog []host.Change, warnings []string) result.Result {
	summary := fmt.Sprintf("phase %s failed: %v", phaseID, err)
	details := map[string]string{
		"phase":     phaseID,
		"role":      cfg.Role().String(),
		"operation": cfg.Operation().String(),
		"instance":  cfg.Shared.InstanceName,
	}
	return result.Result{
		Status:     "failed",
		Step:       phaseID,
		Summary:    summary,
		Reason:     summary,
		Operations: changeLog,
		Warnings:   warnings,
		ErrorCode:  "phase_failed",
		Details:    details,
	}
}

func attemptedState(cfg config.Config, reconcilePlan plan.Plan) state.State {
	return state.State{
		LastAttemptedGeneration:        cfg.GenerationToken(),
		LastAttemptedReconcilerVersion: cfg.Shared.ReconcilerVersion,
		LastKubeTag:                    cfg.Shared.KubeTag,
		LastCARotationID:               cfg.Trigger.CARotationID,
		LastRole:                       cfg.Role().String(),
		LastOperation:                  cfg.Operation().String(),
		LastInputChecksum:              cfg.InputChecksum,
		PlannedPhases:                  reconcilePlan.PhaseIDs(),
	}
}

func successfulState(cfg config.Config, reconcilePlan plan.Plan) state.State {
	return state.State{
		LastAttemptedGeneration:         cfg.GenerationToken(),
		LastSuccessfulGeneration:        cfg.GenerationToken(),
		LastAttemptedReconcilerVersion:  cfg.Shared.ReconcilerVersion,
		LastSuccessfulReconcilerVersion: cfg.Shared.ReconcilerVersion,
		LastKubeTag:                     cfg.Shared.KubeTag,
		LastCARotationID:                cfg.Trigger.CARotationID,
		LastRole:                        cfg.Role().String(),
		LastOperation:                   cfg.Operation().String(),
		LastInputChecksum:               cfg.InputChecksum,
		PlannedPhases:                   reconcilePlan.PhaseIDs(),
	}
}

// truncateError trims a Pulumi error to a reasonable length for Heat signals.
// The full error (which can include entire stdout/stderr dumps) is still logged
// to the reconciler log file.
func truncateError(msg string, maxLen int) string {
	// Extract the first meaningful line before "code:" or "stdout:" dump.
	for _, sep := range []string{"\ncode:", "\nstdout:", "\nstderr:"} {
		if idx := strings.Index(msg, sep); idx > 0 {
			msg = msg[:idx]
			break
		}
	}
	if len(msg) > maxLen {
		return msg[:maxLen] + "..."
	}
	return msg
}

