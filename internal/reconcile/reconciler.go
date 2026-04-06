package reconcile

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	autodebug "github.com/pulumi/pulumi/sdk/v3/go/auto/debug"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optrefresh"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
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
// refreshRetries is the number of times to retry a failed Pulumi refresh
// before falling back to a non-fatal warning.  During CA rotation the
// Kubernetes API may be temporarily unreachable (other masters restarting
// etcd/apiserver), which causes the K8s provider in Pulumi to fail.
const refreshRetries = 2

// refreshRetryDelay is the pause between refresh retry attempts.
const refreshRetryDelay = 15 * time.Second

func Run(ctx context.Context, mode string, diff bool, refresh bool, debugEnabled bool, parallelism int, cfg config.Config, runtimePaths paths.Paths, reconcilePlan plan.Plan, req moduleapi.Request, eventCh chan<- events.EngineEvent) (result.Result, state.State, error) {
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

	// Try to select the existing stack first — avoids recreating stack
	// metadata on every run.  Fall back to upsert if the stack doesn't
	// exist yet (first run on a new node).
	stackOpts := []auto.LocalWorkspaceOption{
		auto.Pulumi(pulumiCmd),
		auto.WorkDir(workspaceDir),
		auto.EnvVars(envVars),
	}
	stack, err := auto.SelectStackInlineSource(ctx,
		cfg.StackName(), "magnum-bootstrap", program, stackOpts...)
	if err != nil {
		stack, err = auto.UpsertStackInlineSource(ctx,
			cfg.StackName(), "magnum-bootstrap", program, stackOpts...)
	}
	if err != nil {
		return result.Result{}, state.State{}, fmt.Errorf("failed to initialize pulumi stack: %w", err)
	}

	progressWriters, errorProgressWriters := pulumiProgressWriters(req.Logger, debugEnabled)
	pulumiDebugOpts := pulumiDebugLogging(debugEnabled)

	if refresh {
		start := time.Now()
		if req.Logger != nil {
			req.Logger.Infof("refreshing pulumi stack state stack=%s", cfg.StackName())
		}
		stopHeartbeat := startPulumiHeartbeat(ctx, req.Logger, "refresh", cfg.StackName(), start)
		refreshOpts := []optrefresh.Option{
			optrefresh.SuppressProgress(),
			optrefresh.ProgressStreams(progressWriters...),
			optrefresh.ErrorProgressStreams(errorProgressWriters...),
		}
		if diff {
			refreshOpts = append(refreshOpts, optrefresh.Diff())
		}
		if pulumiDebugOpts != nil {
			refreshOpts = append(refreshOpts, optrefresh.DebugLogging(*pulumiDebugOpts))
		}

		var refreshRes auto.RefreshResult
		var refreshErr error
		for attempt := 0; attempt <= refreshRetries; attempt++ {
			if attempt > 0 {
				if req.Logger != nil {
					req.Logger.Warnf("retrying pulumi refresh attempt=%d/%d stack=%s after=%s",
						attempt+1, refreshRetries+1, cfg.StackName(), refreshRetryDelay)
				}
				time.Sleep(refreshRetryDelay)
			}
			refreshRes, refreshErr = runWithAutoCancel(ctx, req.Logger, &stack, cfg.StackName(), "refresh", func() (auto.RefreshResult, error) {
				return stack.Refresh(ctx, refreshOpts...)
			})
			if refreshErr == nil {
				break
			}
		}
		stopHeartbeat()
		if refreshErr != nil {
			if req.Logger != nil {
				req.Logger.Warnf("pulumi refresh failed stack=%s duration=%s err=%v; continuing without refresh",
					cfg.StackName(), formatDuration(time.Since(start)), refreshErr)
			}
		} else if req.Logger != nil {
			req.Logger.Infof("pulumi refresh completed stack=%s duration=%s changes=%s",
				cfg.StackName(), formatDuration(time.Since(start)), formatUpdateChangeSummary(refreshRes.Summary.ResourceChanges))
		}
	}

	if parallelism < 1 {
		parallelism = 50
	}

	previewPlanText := ""

	switch mode {
	case "preview":
		start := time.Now()
		if req.Logger != nil {
			req.Logger.Infof("running pulumi preview stack=%s parallelism=%d diff=%t", cfg.StackName(), parallelism, diff)
		}
		stopHeartbeat := startPulumiHeartbeat(ctx, req.Logger, "preview", cfg.StackName(), start)
		previewOpts := []optpreview.Option{
			optpreview.Parallel(parallelism),
			optpreview.SuppressProgress(),
			optpreview.ProgressStreams(progressWriters...),
			optpreview.ErrorProgressStreams(errorProgressWriters...),
		}
		if diff {
			previewOpts = append(previewOpts, optpreview.Diff())
		}
		if eventCh != nil {
			previewOpts = append(previewOpts, optpreview.EventStreams(eventCh))
		}
		if pulumiDebugOpts != nil {
			previewOpts = append(previewOpts, optpreview.DebugLogging(*pulumiDebugOpts))
		}
		var previewRes auto.PreviewResult
		var err error
		if eventCh == nil {
			previewRes, err = runWithAutoCancel(ctx, req.Logger, &stack, cfg.StackName(), "preview", func() (auto.PreviewResult, error) {
				return stack.Preview(ctx, previewOpts...)
			})
		} else {
			previewRes, err = stack.Preview(ctx, previewOpts...)
		}
		stopHeartbeat()
		if err != nil {
			if req.Logger != nil {
				req.Logger.Errorf("pulumi preview failed stack=%s duration=%s err=%v", cfg.StackName(), formatDuration(time.Since(start)), err)
			}
			return extractFailureResult(acc, mode, cfg, runtimePaths, reconcilePlan, req, err)
		}
		if req.Logger != nil {
			req.Logger.Infof("pulumi preview completed stack=%s duration=%s changes=%s",
				cfg.StackName(), formatDuration(time.Since(start)), formatPreviewChangeSummary(previewRes.ChangeSummary))
		}
		if diff {
			previewPlanText = strings.TrimSpace(previewRes.StdOut)
		}

	case "up":
		start := time.Now()
		if req.Logger != nil {
			req.Logger.Infof("running pulumi up stack=%s parallelism=%d diff=%t", cfg.StackName(), parallelism, diff)
		}
		stopHeartbeat := startPulumiHeartbeat(ctx, req.Logger, "up", cfg.StackName(), start)
		upOpts := []optup.Option{
			optup.Parallel(parallelism),
			optup.SuppressProgress(),
			optup.ProgressStreams(progressWriters...),
			optup.ErrorProgressStreams(errorProgressWriters...),
		}
		if diff {
			upOpts = append(upOpts, optup.Diff())
		}
		if eventCh != nil {
			upOpts = append(upOpts, optup.EventStreams(eventCh))
		}
		if pulumiDebugOpts != nil {
			upOpts = append(upOpts, optup.DebugLogging(*pulumiDebugOpts))
		}
		var upRes auto.UpResult
		var err error
		if eventCh == nil {
			upRes, err = runWithAutoCancel(ctx, req.Logger, &stack, cfg.StackName(), "up", func() (auto.UpResult, error) {
				return stack.Up(ctx, upOpts...)
			})
		} else {
			upRes, err = stack.Up(ctx, upOpts...)
		}
		stopHeartbeat()
		if err != nil {
			if req.Logger != nil {
				req.Logger.Errorf("pulumi up failed stack=%s duration=%s err=%v", cfg.StackName(), formatDuration(time.Since(start)), err)
			}
			return extractFailureResult(acc, mode, cfg, runtimePaths, reconcilePlan, req, err)
		}
		if req.Logger != nil {
			req.Logger.Infof("pulumi up completed stack=%s duration=%s changes=%s",
				cfg.StackName(), formatDuration(time.Since(start)), formatUpdateChangeSummary(upRes.Summary.ResourceChanges))
		}

	default:
		return result.Result{}, state.State{}, fmt.Errorf("unknown reconcile mode: %s", mode)
	}

	if acc.HasFailure() {
		failedPhase, failedErr := acc.Failure()
		return buildModuleFailureResult(cfg, runtimePaths, failedPhase, failedErr, acc.Changes(), acc.Warnings()),
			attemptedState(cfg, req, reconcilePlan), failedErr
	}

	missing := module.MissingPhases(module.BuildRegistry(cfg), reconcilePlan)
	tolerateMissing := !req.Apply || req.AllowPartial
	warnings := acc.Warnings()

	if len(missing) > 0 {
		if tolerateMissing {
			warnings = append(warnings, fmt.Sprintf("skipped unimplemented phase(s): %s", strings.Join(missing, ", ")))
		} else {
			return buildMissingModulesResult(mode, cfg, runtimePaths, reconcilePlan, missing, acc.Changes(), warnings, acc.Outputs()),
				attemptedState(cfg, req, reconcilePlan),
				fmt.Errorf("missing modules: %s", strings.Join(missing, ", "))
		}
	}

	reconcileState := successfulState(cfg, req, reconcilePlan)
	res := buildSuccessResult(mode, diff, cfg, runtimePaths, reconcilePlan, acc.PhaseChanges(), acc.Changes(), warnings, acc.Outputs(), missing)
	if mode == "preview" {
		res.PreviewPlan = previewPlanText
	}
	return res, reconcileState, nil
}

// extractFailureResult returns a module-level failure if the accumulator captured one,
// or a generic pulumi-level failure otherwise.
func extractFailureResult(acc *pulumipkg.RunAccumulator, mode string, cfg config.Config, runtimePaths paths.Paths, reconcilePlan plan.Plan, req moduleapi.Request, runErr error) (result.Result, state.State, error) {
	if acc.HasFailure() {
		failedPhase, failedErr := acc.Failure()
		return buildModuleFailureResult(cfg, runtimePaths, failedPhase, failedErr, acc.Changes(), acc.Warnings()),
			attemptedState(cfg, req, reconcilePlan), failedErr
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
	}, attemptedState(cfg, req, reconcilePlan), runErr
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

func pulumiProgressWriters(logger *logging.Logger, debugEnabled bool) ([]io.Writer, []io.Writer) {
	if logger == nil || !debugEnabled {
		return []io.Writer{io.Discard}, []io.Writer{io.Discard}
	}
	return []io.Writer{logger.Writer(logging.LevelDebug)}, []io.Writer{logger.Writer(logging.LevelDebug)}
}

func pulumiDebugLogging(debugEnabled bool) *autodebug.LoggingOptions {
	if !debugEnabled {
		return nil
	}
	level := uint(9)
	return &autodebug.LoggingOptions{
		LogLevel:      &level,
		LogToStdErr:   true,
		FlowToPlugins: true,
		Debug:         true,
	}
}

func startPulumiHeartbeat(ctx context.Context, logger *logging.Logger, operation string, stack string, start time.Time) func() {
	if logger == nil {
		return func() {}
	}

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				logger.Infof("pulumi %s still running stack=%s elapsed=%s", operation, stack, formatDuration(time.Since(start)))
			}
		}
	}()

	return func() {
		close(done)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func formatPreviewChangeSummary(summary map[apitype.OpType]int) string {
	if len(summary) == 0 {
		return "none"
	}

	parts := make([]string, 0, len(summary))
	for op, count := range summary {
		parts = append(parts, fmt.Sprintf("%s=%d", op, count))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func formatUpdateChangeSummary(summary *map[string]int) string {
	if summary == nil || len(*summary) == 0 {
		return "none"
	}

	parts := make([]string, 0, len(*summary))
	for op, count := range *summary {
		parts = append(parts, fmt.Sprintf("%s=%d", op, count))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func attemptedState(cfg config.Config, req moduleapi.Request, reconcilePlan plan.Plan) state.State {
	return state.State{
		LastAttemptedGeneration:        cfg.GenerationToken(),
		LastAttemptedReconcilerVersion: cfg.Shared.ReconcilerVersion,
		LastKubeTag:                    cfg.Shared.KubeTag,
		LastCARotationID:               effectiveCARotationStateID(cfg, req.PreviousCARotationID),
		LastRole:                       cfg.Role().String(),
		LastOperation:                  cfg.Operation().String(),
		LastInputChecksum:              cfg.InputChecksum,
		PlannedPhases:                  reconcilePlan.PhaseIDs(),
	}
}

func successfulState(cfg config.Config, req moduleapi.Request, reconcilePlan plan.Plan) state.State {
	return state.State{
		LastAttemptedGeneration:         cfg.GenerationToken(),
		LastSuccessfulGeneration:        cfg.GenerationToken(),
		LastAttemptedReconcilerVersion:  cfg.Shared.ReconcilerVersion,
		LastSuccessfulReconcilerVersion: cfg.Shared.ReconcilerVersion,
		LastKubeTag:                     cfg.Shared.KubeTag,
		LastCARotationID:                effectiveCARotationStateID(cfg, req.PreviousCARotationID),
		LastRole:                        cfg.Role().String(),
		LastOperation:                   cfg.Operation().String(),
		LastInputChecksum:               cfg.InputChecksum,
		PlannedPhases:                   reconcilePlan.PhaseIDs(),
	}
}

func effectiveCARotationStateID(cfg config.Config, previousID string) string {
	previousID = strings.TrimSpace(previousID)
	currentID := strings.TrimSpace(cfg.Trigger.CARotationID)
	if cfg.IsPureCARotation() && currentID != "" {
		return currentID
	}
	return previousID
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
