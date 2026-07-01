package reconcile

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	autodebug "github.com/pulumi/pulumi/sdk/v3/go/auto/debug"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optdestroy"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optrefresh"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	"github.com/ventus-ag/magnum-bootstrap/internal/buildinfo"
	"github.com/ventus-ag/magnum-bootstrap/internal/carotation"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/module"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/internal/paths"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
	pulumipkg "github.com/ventus-ag/magnum-bootstrap/internal/pulumi"
	"github.com/ventus-ag/magnum-bootstrap/internal/result"
	"github.com/ventus-ag/magnum-bootstrap/internal/state"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
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

// defaultRefreshTimeout bounds a single Pulumi refresh attempt. A refresh has no
// natural deadline of its own, and during a create (or a rotation while the API
// is down) the Kubernetes provider's Read calls can block on an unreachable API
// server for the better part of an hour — long enough that the whole reconcile
// budget is consumed and the killed refresh subprocess leaves a stale Pulumi
// lock behind, wedging every subsequent run. Bounding each attempt lets a hung
// refresh fall back to "continue without refresh" quickly (refresh only syncs
// Pulumi-managed K8s/Helm state; host drift is still detected by module Run()).
const defaultRefreshTimeout = 5 * time.Minute

// refreshTimeoutEnv lets ops override the per-attempt refresh timeout (seconds).
// 0 disables the bound (restores the old unbounded behavior).
const refreshTimeoutEnv = "MAGNUM_RECONCILE_REFRESH_TIMEOUT_SECONDS"

// resolveRefreshTimeout reads refreshTimeoutEnv (seconds), falling back to
// defaultRefreshTimeout. A non-negative integer is required; anything else uses
// the default. A value of 0 means "no per-attempt timeout".
func resolveRefreshTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv(refreshTimeoutEnv))
	if v == "" {
		return defaultRefreshTimeout
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return defaultRefreshTimeout
	}
	return time.Duration(secs) * time.Second
}

func Run(ctx context.Context, mode string, diff bool, refresh bool, debugEnabled bool, parallelism int, cfg config.Config, runtimePaths paths.Paths, reconcilePlan plan.Plan, req moduleapi.Request, eventCh chan<- events.EngineEvent) (result.Result, state.State, error) {
	if parallelism < 1 {
		parallelism = config.AutoParallelism()
	}

	metadata := pulumipkg.BuildMetadata(cfg, reconcilePlan, mode, diff, runtimePaths.HeatParamsFile)
	acc := pulumipkg.NewRunAccumulator()
	program := pulumipkg.BuildProgram(ctx, runtimePaths.HeatParamsFile, reconcilePlan, metadata, req, acc, parallelism)

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

	hostProviderDir, err := ensureHostProviderPlugin(ctx, runtimePaths, req.Logger)
	if err != nil {
		return result.Result{}, state.State{}, err
	}

	// Use the local file backend with an empty passphrase — no cloud auth and
	// no password required. Our stack carries no secrets so no encryption is needed.
	envVars := map[string]string{
		"PULUMI_CONFIG_PASSPHRASE": "",
	}
	if hostProviderDir != "" {
		envVars["PATH"] = hostProviderDir + string(os.PathListSeparator) + os.Getenv("PATH")
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
	// When diff mode is enabled, also capture Pulumi stdout so the preview
	// plan text (resource-level create/update/delete summary) is available.
	var diffBuf *strings.Builder
	if diff && !debugEnabled {
		diffBuf = &strings.Builder{}
		progressWriters = []io.Writer{diffBuf}
	}
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

		refreshTimeout := resolveRefreshTimeout()
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
			// Bound each attempt so a refresh that blocks on an unreachable API
			// can't burn the whole reconcile budget (and leave a stale lock).
			// The parent ctx is passed to runWithAutoCancel so its pulumi cancel
			// still runs even after attemptCtx fires; only stack.Refresh uses the
			// deadline-bounded attemptCtx.
			attemptCtx := ctx
			cancelAttempt := func() {}
			if refreshTimeout > 0 {
				attemptCtx, cancelAttempt = context.WithTimeout(ctx, refreshTimeout)
			}
			refreshRes, refreshErr = runWithAutoCancel(ctx, req.Logger, &stack, cfg.StackName(), "refresh", func() (auto.RefreshResult, error) {
				return stack.Refresh(attemptCtx, refreshOpts...)
			})
			cancelAttempt()
			if refreshErr == nil {
				break
			}
			if refreshTimeout > 0 && attemptCtx.Err() != nil && ctx.Err() == nil && req.Logger != nil {
				req.Logger.Warnf("pulumi refresh attempt timed out stack=%s timeout=%s",
					cfg.StackName(), refreshTimeout)
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

	previewPlanText := ""
	pulumiSummaryText := ""

	switch mode {
	case "preview":
		start := time.Now()
		if req.Logger != nil {
			req.Logger.Infof("running pulumi preview stack=%s parallelism=%d diff=%t", cfg.StackName(), parallelism, diff)
		}
		stopHeartbeat := startPulumiHeartbeat(ctx, req.Logger, "preview", cfg.StackName(), start)
		previewOpts := []optpreview.Option{
			optpreview.Parallel(parallelism),
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
		previewRes, err = stack.Preview(ctx, previewOpts...)
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
			// StdOut may be empty when SuppressProgress is active.
			// Fall back to the captured buffer.
			if previewPlanText == "" && diffBuf != nil {
				previewPlanText = strings.TrimSpace(diffBuf.String())
			}
		}
		pulumiSummaryText = formatPreviewSummaryLine(previewRes.ChangeSummary)

	case "up":
		start := time.Now()
		if req.Logger != nil {
			req.Logger.Infof("running pulumi up stack=%s parallelism=%d diff=%t", cfg.StackName(), parallelism, diff)
		}
		stopHeartbeat := startPulumiHeartbeat(ctx, req.Logger, "up", cfg.StackName(), start)
		// baseUpOpts deliberately excludes EventStreams: the Pulumi SDK closes
		// user-supplied event channels when an operation finishes, so any retry
		// (stale-lock or helm recovery) that reused eventCh would make the SDK
		// send on / close a closed channel and panic in a goroutine outside our
		// recover. Only the very first attempt streams events; retries fall back
		// to progress writers.
		baseUpOpts := []optup.Option{
			optup.Parallel(parallelism),
			optup.ProgressStreams(progressWriters...),
			optup.ErrorProgressStreams(errorProgressWriters...),
		}
		if diff {
			baseUpOpts = append(baseUpOpts, optup.Diff())
		}
		if pulumiDebugOpts != nil {
			baseUpOpts = append(baseUpOpts, optup.DebugLogging(*pulumiDebugOpts))
		}
		firstUpOpts := baseUpOpts
		if eventCh != nil {
			firstUpOpts = append(append([]optup.Option{}, baseUpOpts...), optup.EventStreams(eventCh))
		}
		var upRes auto.UpResult
		var err error
		retryUp := func(label string) {
			start = time.Now()
			stopHeartbeat = startPulumiHeartbeat(ctx, req.Logger, label, cfg.StackName(), start)
			upRes, err = stack.Up(ctx, baseUpOpts...)
			stopHeartbeat()
		}
		// Wrap the initial up so a stale Pulumi lock (left by an earlier run
		// whose process is now dead) is auto-cancelled and retried, instead of
		// failing the reconcile. Without this, a run-once (refresh=false) that
		// inherits a stale lock can never self-heal — it never reaches the
		// refresh path's recovery — and every subsequent run wedges on the same
		// lock. runWithAutoCancel only cancels when the lock owner pid is dead;
		// a genuinely active run is left alone.
		upAttempt := 0
		upRes, err = runWithAutoCancel(ctx, req.Logger, &stack, cfg.StackName(), "up", func() (auto.UpResult, error) {
			opts := firstUpOpts
			if upAttempt > 0 {
				opts = baseUpOpts
			}
			upAttempt++
			return stack.Up(ctx, opts...)
		})
		stopHeartbeat()
		// Recover from Helm failures across MULTIPLE classes in one run. Each
		// branch below repairs one class; a repaired retry can expose a DIFFERENT
		// class (e.g. ownership repair → "no deployed releases"). The branches run
		// top-to-bottom, so a class surfaced by a later branch that belongs to an
		// earlier one would otherwise never be retried in this run. Loop the whole
		// cascade until the error clears or stops changing (no progress), bounded
		// so a persistently-failing class cannot spin forever.
		const maxHelmRecoveryPasses = 6
		for helmPass := 0; err != nil && helmPass < maxHelmRecoveryPasses; helmPass++ {
			prevErr := err.Error()
			executor := host.NewExecutor(true, req.Logger)
			// Check if the failure is a Helm release patch error. These
			// happen during chart version upgrades when the strategic merge
			// patch produces an invalid K8s object. Retry once with
			// ForceUpdate which replaces resources instead of patching.
			if helmFailures := clusterhelm.ParseHelmPatchFailures(err.Error()); len(helmFailures) > 0 {
				names := make([]string, len(helmFailures))
				for i, rel := range helmFailures {
					names[i] = rel.Namespace + "/" + rel.Name
					clusterhelm.MarkForceUpdate(rel.Name, rel.Namespace)
				}
				if req.Logger != nil {
					req.Logger.Warnf("helm patch failure for %v, retrying with force update", names)
				}

				// Clean up Helm releases left in "failed" state by the
				// first attempt so the retry can start fresh.
				for _, rel := range helmFailures {
					clusterhelm.CleanupFailedRelease(executor, rel.Name, rel.Namespace)
				}

				retryUp("up (force retry)")
			}
			// A release can be referenced in Pulumi state while Helm has no
			// deployed revision for it (e.g. an earlier failure-cleanup
			// uninstalled it). `helm upgrade` then fails with "has no deployed
			// releases" on every run — a permanent wedge no other branch
			// matches. Recover by clearing markers + purging orphaned storage,
			// then refreshing so Pulumi drops the stale resource and recreates
			// it fresh on retry.
			if err != nil {
				if names := clusterhelm.ParseHelmNoDeployedReleases(err.Error()); len(names) > 0 {
					recovered := make([]string, 0, len(names))
					for _, name := range names {
						ns := "kube-system"
						if rel, ok := clusterhelm.ManagedReleaseByName(name); ok {
							ns = rel.Namespace
						}
						clusterhelm.ResetDesyncedRelease(executor, name, ns)
						recovered = append(recovered, ns+"/"+name)
					}
					if req.Logger != nil {
						req.Logger.Warnf("helm release(s) %v present in Pulumi state but not deployed in Helm, refreshing state and recreating", recovered)
					}
					if _, refreshErr := stack.Refresh(ctx,
						optrefresh.SuppressProgress(),
						optrefresh.ProgressStreams(progressWriters...),
						optrefresh.ErrorProgressStreams(errorProgressWriters...),
					); refreshErr != nil {
						if req.Logger != nil {
							req.Logger.Warnf("refresh before helm recreate failed stack=%s err=%v; retrying up anyway", cfg.StackName(), refreshErr)
						}
					}
					retryUp("up (helm recreate retry)")
				}
			}
			// A release installed on an older Kubernetes can carry a resource whose
			// apiVersion was REMOVED by the version we are upgrading to (e.g.
			// policy/v1beta1 PodDisruptionBudget, gone in 1.25). `helm upgrade` must
			// build the deployed manifest to diff it, can't map the removed kind, and
			// fails on every run — a permanent wedge for any upgrade crossing that
			// boundary. A patch/force retry can't help (the stored manifest is itself
			// unbuildable). Recover by uninstalling the stale release + purging its
			// storage, then refreshing so Pulumi recreates it fresh from the new
			// chart (which renders the current apiVersion).
			if err != nil {
				if removed := clusterhelm.ParseHelmRemovedAPIFailures(err.Error()); len(removed) > 0 {
					recovered := make([]string, 0, len(removed))
					for _, rel := range removed {
						clusterhelm.ResetRemovedAPIRelease(executor, rel.Name, rel.Namespace)
						recovered = append(recovered, rel.Namespace+"/"+rel.Name)
					}
					if req.Logger != nil {
						req.Logger.Warnf("helm release(s) %v reference an apiVersion this Kubernetes no longer serves (removed-API upgrade boundary); uninstalling + recreating fresh from the new chart", recovered)
					}
					if _, refreshErr := stack.Refresh(ctx,
						optrefresh.SuppressProgress(),
						optrefresh.ProgressStreams(progressWriters...),
						optrefresh.ErrorProgressStreams(errorProgressWriters...),
					); refreshErr != nil {
						if req.Logger != nil {
							req.Logger.Warnf("refresh before helm removed-api recreate failed stack=%s err=%v; retrying up anyway", cfg.StackName(), refreshErr)
						}
					}
					retryUp("up (helm removed-api recreate retry)")
				}
			}
			// A previous helm install/upgrade/rollback killed mid-flight (node
			// reboot, Heat timeout, OOM) leaves the release's newest storage
			// record in pending-*; Helm then refuses every subsequent operation
			// with "another operation ... is in progress" — a permanent wedge no
			// other branch matches. Recover by deleting only the pending
			// revision Secrets: the last deployed revision becomes current again
			// (live workload untouched), or a fresh install happens if nothing
			// was ever deployed.
			if err != nil {
				if pending, matched := clusterhelm.ParseHelmPendingOperations(err.Error()); matched {
					if len(pending) == 0 {
						pending = clusterhelm.PendingOperationManagedReleases(executor)
					}
					recovered := make([]string, 0, len(pending))
					for _, rel := range pending {
						if clusterhelm.ResetPendingOperationRelease(executor, rel.Name, rel.Namespace) {
							recovered = append(recovered, rel.Namespace+"/"+rel.Name)
						}
					}
					if len(recovered) > 0 {
						if req.Logger != nil {
							req.Logger.Warnf("helm release(s) %v stuck in a pending operation (interrupted install/upgrade/rollback); cleared pending revision(s) and retrying", recovered)
						}
						retryUp("up (helm pending-operation retry)")
					}
				}
			}
			// Some legacy clusters have Helm release resources whose objects exist
			// in the cluster without the Helm ownership labels/annotations. Patch
			// the expected metadata onto those live resources and keep retrying
			// within this run until the next ownership blocker is exposed.
			if err != nil {
				const maxOwnershipRepairRetries = 10
				for attempt := 1; err != nil && attempt <= maxOwnershipRepairRetries; attempt++ {
					ownershipConflicts := clusterhelm.ParseHelmOwnershipConflicts(err.Error())
					if len(ownershipConflicts) == 0 {
						break
					}
					repaired := clusterhelm.RepairHelmOwnershipConflicts(executor, ownershipConflicts)
					if len(repaired) == 0 {
						break
					}
					names := make([]string, len(repaired))
					for i, conflict := range repaired {
						if conflict.ResourceNamespace != "" {
							names[i] = conflict.ResourceNamespace + "/" + strings.ToLower(conflict.ResourceKind) + "/" + conflict.ResourceName
						} else {
							names[i] = strings.ToLower(conflict.ResourceKind) + "/" + conflict.ResourceName
						}
					}
					if req.Logger != nil {
						req.Logger.Warnf("helm ownership metadata conflict for %v, retrying after patching live resources (attempt %d/%d)", names, attempt, maxOwnershipRepairRetries)
					}
					retryUp(fmt.Sprintf("up (ownership repair retry %d)", attempt))
				}
				if err != nil {
					ownershipConflicts := clusterhelm.ParseHelmOwnershipConflicts(err.Error())
					if len(ownershipConflicts) > 0 {
						deleted := clusterhelm.DeleteHelmOwnershipConflicts(executor, ownershipConflicts)
						if len(deleted) > 0 {
							names := make([]string, len(deleted))
							for i, conflict := range deleted {
								if conflict.ResourceNamespace != "" {
									names[i] = conflict.ResourceNamespace + "/" + strings.ToLower(conflict.ResourceKind) + "/" + conflict.ResourceName
								} else {
									names[i] = strings.ToLower(conflict.ResourceKind) + "/" + conflict.ResourceName
								}
							}
							if req.Logger != nil {
								req.Logger.Warnf("helm ownership conflict persisted for %v, retrying after deleting live resources", names)
							}
							retryUp("up (ownership delete retry)")
						}
					}
				}
			}
			// Legacy clusters may already have Helm releases created outside
			// Pulumi state. If adoption/import still results in Helm reporting
			// "name is still in use", first repair stale import markers for
			// existing managed releases and retry import. If that still fails,
			// uninstall only the still-pending import releases and retry once so
			// Pulumi can recreate them fresh.
			if err != nil && clusterhelm.HasHelmNameReuseConflict(err.Error()) {
				repaired := clusterhelm.PrepareManagedImports(executor)
				if len(repaired) > 0 {
					names := make([]string, len(repaired))
					for i, rel := range repaired {
						names[i] = rel.Namespace + "/" + rel.Name
					}
					if req.Logger != nil {
						req.Logger.Warnf("helm release name conflict for %v, retrying after re-preparing import markers", names)
					}
					retryUp("up (import repair retry)")
				}
				if err != nil && clusterhelm.HasHelmNameReuseConflict(err.Error()) {
					pending := clusterhelm.CleanupPendingImportReleases(executor)
					if len(pending) > 0 {
						names := make([]string, len(pending))
						for i, rel := range pending {
							names[i] = rel.Namespace + "/" + rel.Name
						}
						if req.Logger != nil {
							req.Logger.Warnf("helm release import conflict for %v, retrying after uninstalling legacy releases", names)
						}

						retryUp("up (import retry)")
					}
				}
			}
			if err == nil {
				break
			}
			if err.Error() == prevErr {
				break // no recovery branch made progress this pass; stop retrying
			}
		}
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
		clusterhelm.PromoteManagedReleases()
		clusterhelm.ClearAllForceUpdateMarkers()
		pulumiSummaryText = formatUpdateSummaryLine(upRes.Summary.ResourceChanges)
		// Bound the local backend's ever-growing update-history directory so
		// each successive operation does not pay a rising checkpoint/history
		// cost (best-effort; never fails the reconcile).
		pruneStackHistory(runtimePaths.PulumiStateDir, req.Logger)

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
	res.PulumiSummary = pulumiSummaryText
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

// Destroy removes all Pulumi-managed resources (K8s resources, Helm releases)
// and then runs module-level Destroy() for modules that implement Destroyer
// (e.g., etcd removes cluster membership and cleans data directories).
func Destroy(ctx context.Context, cfg config.Config, runtimePaths paths.Paths, reconcilePlan plan.Plan, req moduleapi.Request, eventCh chan<- events.EngineEvent) (result.Result, error) {
	workspaceDir := filepath.Join(runtimePaths.PulumiStateDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return result.Result{}, fmt.Errorf("failed to create pulumi workspace dir: %w", err)
	}

	pulumiRoot := filepath.Join(runtimePaths.PulumiStateDir, "cli")
	pulumiCmd, err := auto.InstallPulumiCommand(ctx, &auto.PulumiCommandOptions{
		Root: pulumiRoot,
	})
	if err != nil {
		return result.Result{}, fmt.Errorf("failed to install pulumi cli: %w", err)
	}

	hostProviderDir, err := ensureHostProviderPlugin(ctx, runtimePaths, req.Logger)
	if err != nil {
		return result.Result{}, err
	}

	envVars := map[string]string{
		"PULUMI_CONFIG_PASSPHRASE": "",
	}
	if hostProviderDir != "" {
		envVars["PATH"] = hostProviderDir + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	if runtimePaths.PulumiBackend != "" {
		envVars["PULUMI_BACKEND_URL"] = runtimePaths.PulumiBackend
	}

	// We need a program function for SelectStackInlineSource even though
	// destroy doesn't run it. Use the normal program so the stack can be found.
	metadata := pulumipkg.BuildMetadata(cfg, reconcilePlan, "destroy", false, runtimePaths.HeatParamsFile)
	acc := pulumipkg.NewRunAccumulator()
	program := pulumipkg.BuildProgram(ctx, runtimePaths.HeatParamsFile, reconcilePlan, metadata, req, acc, 1)

	stackOpts := []auto.LocalWorkspaceOption{
		auto.Pulumi(pulumiCmd),
		auto.WorkDir(workspaceDir),
		auto.EnvVars(envVars),
	}
	stack, err := auto.SelectStackInlineSource(ctx,
		cfg.StackName(), "magnum-bootstrap", program, stackOpts...)
	if err != nil {
		return result.Result{
			Status:  "failed",
			Step:    "destroy",
			Summary: fmt.Sprintf("stack not found: %v", err),
		}, fmt.Errorf("stack not found (nothing to destroy?): %w", err)
	}

	// Destroy all Pulumi-managed resources (Helm releases, K8s resources).
	if req.Logger != nil {
		req.Logger.Infof("destroying pulumi stack stack=%s", cfg.StackName())
	}
	start := time.Now()
	destroyOpts := []optdestroy.Option{
		optdestroy.ProgressStreams(io.Discard),
		optdestroy.ErrorProgressStreams(io.Discard),
	}
	if eventCh != nil {
		destroyOpts = append(destroyOpts, optdestroy.EventStreams(eventCh))
	}

	destroyRes, err := stack.Destroy(ctx, destroyOpts...)
	if err != nil {
		if req.Logger != nil {
			req.Logger.Errorf("pulumi destroy failed stack=%s duration=%s err=%v",
				cfg.StackName(), formatDuration(time.Since(start)), err)
		}
		return result.Result{
			Status:    "failed",
			Step:      "destroy",
			Summary:   fmt.Sprintf("pulumi destroy failed: %v", truncateError(err.Error(), 300)),
			ErrorCode: "destroy_failed",
		}, err
	}

	pulumiSummary := formatUpdateSummaryLine(destroyRes.Summary.ResourceChanges)
	if req.Logger != nil {
		req.Logger.Infof("pulumi destroy completed stack=%s duration=%s changes=%s",
			cfg.StackName(), formatDuration(time.Since(start)), formatUpdateChangeSummary(destroyRes.Summary.ResourceChanges))
	}

	// Run module-level Destroy() for modules implementing Destroyer.
	// Execute in reverse phase order so dependents clean up before dependencies.
	registry := module.BuildRegistry(cfg)
	var warnings []string
	for i := len(reconcilePlan.Phases) - 1; i >= 0; i-- {
		phase := reconcilePlan.Phases[i]
		mod, ok := registry[phase.ID]
		if !ok {
			continue
		}
		destroyer, ok := mod.(moduleapi.Destroyer)
		if !ok {
			continue
		}
		if req.Logger != nil {
			req.Logger.Infof("destroying phase=%s", phase.ID)
		}
		if err := destroyer.Destroy(ctx, cfg, req); err != nil {
			if req.Logger != nil {
				req.Logger.Warnf("phase=%s destroy failed: %v", phase.ID, err)
			}
			warnings = append(warnings, fmt.Sprintf("phase %s destroy: %v", phase.ID, err))
		}
	}

	return result.Result{
		Status:        "succeeded",
		Step:          "destroy",
		Summary:       fmt.Sprintf("destroyed stack %s", cfg.StackName()),
		PulumiSummary: pulumiSummary,
		Warnings:      warnings,
	}, nil
}

func ensureHostProviderPlugin(ctx context.Context, runtimePaths paths.Paths, logger *logging.Logger) (string, error) {
	// Respect an explicit MAGNUM_USE_HOST_PROVIDER=false: release builds
	// always carry a default provider URL, and without this gate every run
	// (preview included) downloads the plugin — and a download failure
	// (air-gap, GitHub outage) hard-fails the reconcile even for clusters
	// that have the provider disabled and no provider resource in state.
	// A locally cached or ambient plugin is still exposed: the stack state
	// may hold provider resources from earlier runs, and the engine needs
	// the plugin binary to process (no-op delete) them during the flip-off.
	if !hostsdk.Enabled() {
		pluginDir := filepath.Join(runtimePaths.PulumiStateDir, "providers")
		if _, err := os.Stat(filepath.Join(pluginDir, buildinfo.HostProviderAsset)); err == nil {
			return pluginDir, nil
		}
		if providerDir := hostsdk.ProviderDir(); providerDir != "" {
			return providerDir, nil
		}
		return "", nil
	}
	if providerDir := hostsdk.ProviderDir(); providerDir != "" {
		return providerDir, nil
	}
	providerURL := hostsdk.ProviderURL()
	if providerURL == "" {
		return "", nil
	}
	pluginDir := filepath.Join(runtimePaths.PulumiStateDir, "providers")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create host provider dir: %w", err)
	}
	pluginPath := filepath.Join(pluginDir, buildinfo.HostProviderAsset)
	executor := host.NewExecutor(true, logger)
	if _, err := executor.DownloadFileWithRetry(ctx, providerURL, pluginPath, 0o755, 3); err != nil {
		return "", fmt.Errorf("failed to download magnumhost provider: %w", err)
	}
	return pluginDir, nil
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
		LastAttemptedReconcilerVersion: cfg.EffectiveReconcilerVersion(),
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
		LastAttemptedReconcilerVersion:  cfg.EffectiveReconcilerVersion(),
		LastSuccessfulReconcilerVersion: cfg.EffectiveReconcilerVersion(),
		LastKubeTag:                     cfg.Shared.KubeTag,
		LastCARotationID:                effectiveCARotationStateID(cfg, req.PreviousCARotationID),
		LastRole:                        cfg.Role().String(),
		LastOperation:                   cfg.Operation().String(),
		LastInputChecksum:               cfg.InputChecksum,
		PlannedPhases:                   reconcilePlan.PhaseIDs(),
	}
}

func effectiveCARotationStateID(_ config.Config, previousID string) string {
	// The finalize marker is the authoritative completion signal: the
	// ca-rotation module writes it only after a rotation fully finalizes. Using
	// it here (instead of the in-flight CA_ROTATION_ID) means a rotation that
	// fails mid-protocol is not falsely recorded as applied, so the next run
	// re-enters and resumes it. Falling back to the previous state ID preserves
	// earlier completions when no marker is present.
	if marker := carotation.ReadMarker(); marker != "" {
		return marker
	}
	return strings.TrimSpace(previousID)
}

// formatPreviewSummaryLine builds a one-line summary from a preview change map:
//
//	"Pulumi: 3 to create, 1 to update, 2 to delete, 34 unchanged"
func formatPreviewSummaryLine(summary map[apitype.OpType]int) string {
	if len(summary) == 0 {
		return ""
	}
	parts := make([]string, 0, 4)
	if n := summary[apitype.OpCreate]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d to create", n))
	}
	if n := summary[apitype.OpUpdate]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d to update", n))
	}
	if n := summary[apitype.OpDelete]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d to delete", n))
	}
	if n := summary[apitype.OpReplace]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d to replace", n))
	}
	if n := summary[apitype.OpSame]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Pulumi: " + strings.Join(parts, ", ")
}

// formatUpdateSummaryLine builds a one-line summary from an up result change map.
func formatUpdateSummaryLine(summary *map[string]int) string {
	if summary == nil || len(*summary) == 0 {
		return ""
	}
	parts := make([]string, 0, 4)
	m := *summary
	if n := m["create"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d created", n))
	}
	if n := m["update"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", n))
	}
	if n := m["delete"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", n))
	}
	if n := m["replace"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d replaced", n))
	}
	if n := m["same"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d unchanged", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Pulumi: " + strings.Join(parts, ", ")
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
