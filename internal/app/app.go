package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto/events"
	"github.com/spf13/cobra"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/display"
	"github.com/ventus-ag/magnum-bootstrap/internal/journal"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/internal/paths"
	"github.com/ventus-ag/magnum-bootstrap/internal/plan"
	"github.com/ventus-ag/magnum-bootstrap/internal/reconcile"
	"github.com/ventus-ag/magnum-bootstrap/internal/result"
	"github.com/ventus-ag/magnum-bootstrap/internal/state"
)

// runFlags holds the flags shared by preview, up, run-once, and run-periodic.
type runFlags struct {
	diff           bool
	allowPartial   bool
	refresh        bool
	targetPhase    string
	parallelism    int
	debug          bool
	backendURL     string
	heatParamsFile string
	timeout        time.Duration

	// periodic is true only for the run-periodic command (the systemd drift
	// timer). It gates non-convergence "day-2" maintenance (etcd defrag, alarm
	// handling) so those disruptive-ish ops never run during a Heat-triggered
	// run-once (create/upgrade/resize), only on the steady-state timer.
	periodic bool
}

// defaultRunTimeout caps a single reconcile invocation when nothing else is
// configured. Picked to be longer than any legitimate node-level run (slow Helm
// rollouts, drain, etc.) while still bounding hangs caused by an unresponsive
// Kubernetes API.
const defaultRunTimeout = time.Hour

// runTimeoutEnv lets Heat drive the reconcile timeout. The launcher exports it
// from the RECONCILER_RUN_TIMEOUT_SECONDS heat-param. It MUST be set a safe
// margin below Heat's stack update_timeout so the reconciler self-cancels,
// reports a clean failure, and releases its flock BEFORE Heat gives up. If the
// reconciler instead runs to Heat's deadline, Heat marks the deployment failed
// while the process keeps running (oneshot units have no start timeout by
// default) — squatting the lock so every subsequent Heat-triggered run blocks
// on it and times out too. A Heat-aligned deadline is what keeps a single
// timeout from becoming a permanently-wedged cluster.
const runTimeoutEnv = "MAGNUM_RECONCILE_RUN_TIMEOUT_SECONDS"

// resolveDefaultRunTimeout reads runTimeoutEnv (seconds) and falls back to
// defaultRunTimeout. A value of 0 disables the timeout (matches --timeout=0).
func resolveDefaultRunTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv(runTimeoutEnv))
	if v == "" {
		return defaultRunTimeout
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return defaultRunTimeout
	}
	return time.Duration(secs) * time.Second
}

// Main is the top-level entry point. It builds the Cobra command tree, attaches
// stdout/stderr, and returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	code := 0
	root := buildRoot(ctx, &code, stdout, stderr)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(stderr, err)
		if code == 0 {
			code = 2
		}
	}
	return code
}

func buildRoot(ctx context.Context, code *int, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "bootstrap",
		Short:         "Magnum node reconciler",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newPreviewCmd(ctx, code, stdout, stderr))
	root.AddCommand(newUpCmd(ctx, code, stdout, stderr))
	root.AddCommand(newRunOnceCmd(ctx, code, stdout, stderr))
	root.AddCommand(newRunPeriodicCmd(ctx, code, stdout, stderr))
	root.AddCommand(newDestroyCmd(ctx, code, stdout, stderr))
	root.AddCommand(newCancelCmd(ctx, code, stdout, stderr))
	root.AddCommand(newValidateInputCmd(code, stdout, stderr))
	root.AddCommand(newPrintLastResultCmd(code, stdout, stderr))
	return root
}

// addRunFlags attaches the common reconcile flags to a command.
// refreshDefault controls whether --refresh defaults to true or false.
// Heat-triggered commands (run-once) default to false because modules
// already check actual state and refresh can fail when the K8s API is
// temporarily unavailable (e.g. during multi-master CA rotation).
// Periodic commands (run-periodic) default to true for drift detection.
func addRunFlags(cmd *cobra.Command, f *runFlags, refreshDefault bool) {
	cmd.Flags().BoolVar(&f.diff, "diff", false, "show diff-oriented output")
	cmd.Flags().BoolVar(&f.allowPartial, "allow-partial", false, "allow missing phase implementations and run only implemented modules")
	cmd.Flags().BoolVar(&f.refresh, "refresh", refreshDefault, "run pulumi refresh before preview or up to sync state with actual node state")
	cmd.Flags().StringVar(&f.targetPhase, "target-phase", "", "run only the specified phase (empty means all phases in the plan)")
	cmd.Flags().IntVar(&f.parallelism, "parallelism", 0, "maximum number of phases/Pulumi operations to run in parallel (0 = auto-scale to host RAM and CPU)")
	cmd.Flags().BoolVar(&f.debug, "debug", false, "enable Pulumi debug logging and verbose event output")
	cmd.Flags().StringVar(&f.backendURL, "backend-url", "", "override Pulumi backend URL (default: $MAGNUM_PULUMI_BACKEND_URL or file:///var/lib/magnum/pulumi)")
	cmd.Flags().StringVar(&f.heatParamsFile, "heat-params-file", "", "override heat-params file path (default: $MAGNUM_RECONCILE_HEAT_PARAMS_FILE or /etc/sysconfig/heat-params)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", resolveDefaultRunTimeout(), "overall timeout for the reconcile invocation (0 disables; default from "+runTimeoutEnv+" or 1h)")
}

func newPreviewCmd(ctx context.Context, code *int, stdout, stderr io.Writer) *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Show planned changes without applying them (dry run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = run(ctx, "preview", f, stdout, stderr)
			return nil
		},
	}
	addRunFlags(cmd, &f, true)
	return cmd
}

func newUpCmd(ctx context.Context, code *int, stdout, stderr io.Writer) *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Apply changes to reconcile node state",
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = run(ctx, "up", f, stdout, stderr)
			return nil
		},
	}
	addRunFlags(cmd, &f, true)
	return cmd
}

func newRunOnceCmd(ctx context.Context, code *int, stdout, stderr io.Writer) *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run-once",
		Short: "Apply changes once (alias for up, used by Heat-triggered invocations)",
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = run(ctx, "up", f, stdout, stderr)
			return nil
		},
	}
	addRunFlags(cmd, &f, false)
	return cmd
}

func newRunPeriodicCmd(ctx context.Context, code *int, stdout, stderr io.Writer) *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run-periodic",
		Short: "Apply changes periodically (alias for up, used by the reconcile timer)",
		RunE: func(cmd *cobra.Command, args []string) error {
			f.periodic = true
			*code = run(ctx, "up", f, stdout, stderr)
			return nil
		},
	}
	addRunFlags(cmd, &f, true)
	return cmd
}

func newDestroyCmd(ctx context.Context, code *int, stdout, stderr io.Writer) *cobra.Command {
	var heatParamsFile string
	var backendURL string
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Destroy all Pulumi-managed resources and run module cleanup",
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = runDestroy(ctx, heatParamsFile, backendURL, stdout, stderr)
			return nil
		},
	}
	cmd.Flags().StringVar(&heatParamsFile, "heat-params-file", "", "override heat-params file path")
	cmd.Flags().StringVar(&backendURL, "backend-url", "", "override Pulumi backend URL")
	return cmd
}

func runDestroy(ctx context.Context, heatParamsFileOverride, backendURLOverride string, stdout, stderr io.Writer) int {
	runtimePaths := paths.LoadFromEnv()
	if backendURLOverride != "" {
		runtimePaths.PulumiBackend = backendURLOverride
	}
	if heatParamsFileOverride != "" {
		runtimePaths.HeatParamsFile = heatParamsFileOverride
	}

	renderer := display.NewRenderer(stdout, false)

	logger, err := logging.New(runtimePaths.LogFile, stderr, false)
	if err != nil {
		fmt.Fprintf(stderr, "failed to initialize log: %v\n", err)
		return 1
	}
	defer logger.Close()

	cfg, err := config.Load(runtimePaths.HeatParamsFile)
	if err != nil {
		logger.Errorf("failed to load heat params: %v", err)
		fmt.Fprintf(stderr, "failed to load heat params: %v\n", err)
		return 1
	}

	reconcilePlan := plan.Build(cfg)
	logger.Infof("starting destroy instance=%s role=%s stack=%s", cfg.Shared.InstanceName, cfg.Role(), cfg.StackName())

	eventCh := make(chan events.EngineEvent, 5000)
	done := make(chan struct{})
	go func() {
		renderer.StreamEvents(eventCh)
		close(done)
	}()

	destroyResult, err := reconcile.Destroy(ctx, cfg, runtimePaths, reconcilePlan, moduleapi.Request{
		Apply:  true,
		Logger: logger,
		Paths:  runtimePaths,
	}, eventCh)
	safeCloseEngineEvents(eventCh)
	<-done

	renderer.PrintResult(destroyResult)

	if err != nil {
		logger.Errorf("destroy failed: %v", err)
		return 1
	}

	logger.Infof("destroy completed summary=%s", destroyResult.Summary)
	return 0
}

func newValidateInputCmd(code *int, stdout, stderr io.Writer) *cobra.Command {
	var heatParamsFile string
	cmd := &cobra.Command{
		Use:   "validate-input",
		Short: "Validate heat-params input and print parsed role/operation",
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = validate(heatParamsFile, stdout, stderr)
			return nil
		},
	}
	cmd.Flags().StringVar(&heatParamsFile, "heat-params-file", "", "override heat-params file path")
	return cmd
}

func newPrintLastResultCmd(code *int, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "print-last-result",
		Short: "Print the result of the last reconcile run as JSON",
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = printLastResult(stdout, stderr)
			return nil
		},
	}
}

func validate(heatParamsFileOverride string, stdout, stderr io.Writer) int {
	runtimePaths := paths.LoadFromEnv()
	if heatParamsFileOverride != "" {
		runtimePaths.HeatParamsFile = heatParamsFileOverride
	}
	cfg, err := config.Load(runtimePaths.HeatParamsFile)
	if err != nil {
		fmt.Fprintf(stderr, "failed to load heat params: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "role=%s operation=%s instance=%s inputChecksum=%s\n",
		cfg.Role(), cfg.Operation(), cfg.Shared.InstanceName, cfg.InputChecksum)
	return 0
}

func printLastResult(stdout, stderr io.Writer) int {
	runtimePaths := paths.LoadFromEnv()
	data, err := os.ReadFile(runtimePaths.ResultFile)
	if err != nil {
		fmt.Fprintf(stderr, "failed to read last result: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "failed to write result: %v\n", err)
		return 1
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		fmt.Fprintln(stdout)
	}
	return 0
}

func run(ctx context.Context, mode string, f runFlags, stdout, stderr io.Writer) int {
	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}

	runtimePaths := paths.LoadFromEnv()
	if f.backendURL != "" {
		runtimePaths.PulumiBackend = f.backendURL
	}
	if f.heatParamsFile != "" {
		runtimePaths.HeatParamsFile = f.heatParamsFile
	}

	if err := runtimePaths.EnsureDirs(); err != nil {
		fmt.Fprintf(stderr, "failed to create required directories: %v\n", err)
		return 1
	}

	renderer := display.NewRenderer(stdout, f.debug)

	logger, err := logging.New(runtimePaths.LogFile, stderr, f.debug)
	if err != nil {
		writeFailure(runtimePaths.ResultFile, "logging", fmt.Sprintf("failed to initialize reconciler log: %v", err))
		fmt.Fprintf(stderr, "failed to initialize reconciler log: %v\n", err)
		return 1
	}
	defer logger.Close()

	logger.Infof("starting reconcile mode=%s diff=%t allowPartial=%t refresh=%t heatParamsFile=%s resultFile=%s logFile=%s timeout=%s",
		mode, f.diff, f.allowPartial, f.refresh, runtimePaths.HeatParamsFile, runtimePaths.ResultFile, runtimePaths.LogFile, formatTimeout(f.timeout))

	cfg, err := config.Load(runtimePaths.HeatParamsFile)
	if err != nil {
		writeFailure(runtimePaths.ResultFile, "input", fmt.Sprintf("failed to load heat params: %v", err))
		logger.Errorf("failed to load heat params: %v", err)
		fmt.Fprintf(stderr, "failed to load heat params: %v\n", err)
		return 1
	}

	// Load previous state early so Operation() can detect already-applied
	// CA rotations and fall back to normal reconcile instead of re-running
	// the full ca-rotate plan on every periodic timer tick.
	previousState, _ := state.Load(runtimePaths.StateFile)
	cfg.Trigger.AppliedCARotationID = previousState.LastCARotationID

	reconcilePlan := plan.Build(cfg)
	if f.targetPhase != "" {
		reconcilePlan = reconcilePlan.FilterToPhase(f.targetPhase)
	}

	logger.Infof("loaded desired input instance=%s role=%s operation=%s checksum=%s",
		cfg.Shared.InstanceName, cfg.Role(), cfg.Operation(), cfg.InputChecksum)
	logger.Infof("built reconcile plan phases=%s", strings.Join(reconcilePlan.PhaseIDs(), ","))

	recoveredState, recovered, err := journal.RecoverInterrupted(runtimePaths.RunStateFile)
	if err != nil {
		writeFailure(runtimePaths.ResultFile, "journal", fmt.Sprintf("failed to recover interrupted run state: %v", err))
		logger.Errorf("failed to recover interrupted run state: %v", err)
		fmt.Fprintf(stderr, "failed to recover interrupted run state: %v\n", err)
		return 1
	}
	if recovered {
		logger.Warnf("marked previous run as interrupted mode=%s instance=%s role=%s operation=%s",
			recoveredState.Mode, recoveredState.Instance, recoveredState.Role, recoveredState.Operation)
	}

	if err := journal.MarkRunning(runtimePaths.RunStateFile, mode, cfg.Shared.InstanceName, cfg.Role().String(), cfg.Operation().String(), reconcilePlan.PhaseIDs()); err != nil {
		writeFailure(runtimePaths.ResultFile, "journal", fmt.Sprintf("failed to record running state: %v", err))
		logger.Errorf("failed to record running state: %v", err)
		fmt.Fprintf(stderr, "failed to record running state: %v\n", err)
		return 1
	}
	logger.Infof("recorded running state runStateFile=%s", runtimePaths.RunStateFile)

	// previousState was already loaded above for Operation() detection.

	// Resolve parallelism. 0 (the default) means auto-scale to host RAM/CPU so
	// small nodes (2 GiB single-master) serialize the Pulumi/Helm work instead of
	// OOM-killing the node mid-run, while large nodes stay fast.
	if f.parallelism <= 0 {
		f.parallelism = config.AutoParallelism()
		logger.Infof("resolved parallelism=%d (auto)", f.parallelism)
	} else {
		logger.Infof("resolved parallelism=%d (explicit)", f.parallelism)
	}

	// Always stream Pulumi resource-level events so create/update/delete
	// operations are visible. The --diff flag controls property-level diffs.
	eventCh := make(chan events.EngineEvent, 5000)
	done := make(chan struct{})
	go func() {
		renderer.StreamEvents(eventCh)
		close(done)
	}()

	runResult, reconcileState, err := reconcile.Run(ctx, mode, f.diff, f.refresh, f.debug, f.parallelism, cfg, runtimePaths, reconcilePlan, moduleapi.Request{
		Apply:                        mode != "preview",
		AllowPartial:                 f.allowPartial,
		Logger:                       logger,
		Paths:                        runtimePaths,
		PreviousSuccessfulGeneration: previousState.LastSuccessfulGeneration,
		PreviousKubeTag:              previousState.LastKubeTag,
		PreviousCARotationID:         previousState.LastCARotationID,
		Periodic:                     f.periodic,
	}, eventCh)
	safeCloseEngineEvents(eventCh)
	<-done // wait for event stream to drain
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr == context.DeadlineExceeded {
			timeoutMsg := fmt.Sprintf("reconcile exceeded timeout %s: %v", formatTimeout(f.timeout), err)
			err = fmt.Errorf("%s", timeoutMsg)
			if runResult.Step == "" {
				runResult.Step = mode + "-timeout"
			}
			if runResult.Summary == "" {
				runResult.Summary = timeoutMsg
				runResult.Reason = timeoutMsg
			}
			if runResult.ErrorCode == "" {
				runResult.ErrorCode = "timeout"
			}
		}
		_ = journal.MarkFailed(runtimePaths.RunStateFile, mode, err.Error())
		if runResult.Step == "" {
			runResult.Step = mode
		}
		if runResult.Summary == "" {
			runResult.Summary = err.Error()
			runResult.Reason = err.Error()
		}
		if runResult.Status == "" {
			runResult.Status = "failed"
		}
		runResult = result.Normalize(runResult)
		_ = result.Write(runtimePaths.ResultFile, runResult)
		logger.Errorf("reconcile failed step=%s err=%v", runResult.Step, err)
		renderer.PrintResult(runResult)
		return 1
	}

	// Bookkeeping failures after a successful reconcile are warnings, not
	// failures: the node IS converged, and reporting exit 1 here makes Heat
	// mark a healthy node failed. Lost state only means the next run
	// re-derives intent and re-converges (reconciliation is idempotent); a
	// stale journal is overwritten by the next MarkRunning.
	if mode != "preview" {
		if err := state.Write(runtimePaths.StateFile, reconcileState); err != nil {
			logger.Warnf("failed to persist reconciler state (node converged; next run will re-converge): %v", err)
			runResult.Warnings = append(runResult.Warnings, fmt.Sprintf("failed to persist reconciler state: %v", err))
		} else {
			logger.Infof("persisted reconciler state stateFile=%s", runtimePaths.StateFile)
		}
	} else {
		logger.Infof("skipped reconciler state persistence for preview mode")
	}

	if err := journal.MarkCompleted(runtimePaths.RunStateFile, mode, runResult.Summary); err != nil {
		logger.Warnf("failed to persist completion state (node converged): %v", err)
		runResult.Warnings = append(runResult.Warnings, fmt.Sprintf("failed to persist completion state: %v", err))
	}

	runResult = result.Normalize(runResult)
	if err := result.Write(runtimePaths.ResultFile, runResult); err != nil {
		logger.Errorf("failed to persist result: %v", err)
		fmt.Fprintf(stderr, "failed to persist result: %v\n", err)
		return 1
	}
	logger.Infof("persisted reconcile result resultFile=%s status=%s", runtimePaths.ResultFile, runResult.Status)

	renderer.PrintResult(runResult)
	logger.Infof("reconcile completed summary=%s", runResult.Summary)
	return 0
}

func formatTimeout(d time.Duration) string {
	if d <= 0 {
		return "disabled"
	}
	return d.String()
}

func safeCloseEngineEvents(ch chan events.EngineEvent) {
	if ch == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	close(ch)
}

func writeFailure(path, step, summary string) {
	_ = result.Write(path, result.Result{
		Status:    "failed",
		Step:      step,
		Summary:   summary,
		Reason:    summary,
		ErrorCode: "bootstrap_error",
	})
}
