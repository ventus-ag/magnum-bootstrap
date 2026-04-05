package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

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
	cmd.Flags().IntVar(&f.parallelism, "parallelism", 10, "maximum number of phases to execute in parallel")
	cmd.Flags().BoolVar(&f.debug, "debug", false, "enable Pulumi debug logging and verbose event output")
	cmd.Flags().StringVar(&f.backendURL, "backend-url", "", "override Pulumi backend URL (default: $MAGNUM_PULUMI_BACKEND_URL or file:///var/lib/magnum/pulumi)")
	cmd.Flags().StringVar(&f.heatParamsFile, "heat-params-file", "", "override heat-params file path (default: $MAGNUM_RECONCILE_HEAT_PARAMS_FILE or /etc/sysconfig/heat-params)")
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
			*code = run(ctx, "up", f, stdout, stderr)
			return nil
		},
	}
	addRunFlags(cmd, &f, true)
	return cmd
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
	runtimePaths := paths.LoadFromEnv()
	if f.backendURL != "" {
		runtimePaths.PulumiBackend = f.backendURL
	}
	if f.heatParamsFile != "" {
		runtimePaths.HeatParamsFile = f.heatParamsFile
	}

	renderer := display.NewRenderer(stdout, f.debug)

	logger, err := logging.New(runtimePaths.LogFile, stderr, f.debug)
	if err != nil {
		writeFailure(runtimePaths.ResultFile, "logging", fmt.Sprintf("failed to initialize reconciler log: %v", err))
		fmt.Fprintf(stderr, "failed to initialize reconciler log: %v\n", err)
		return 1
	}
	defer logger.Close()

	logger.Infof("starting reconcile mode=%s diff=%t allowPartial=%t refresh=%t heatParamsFile=%s resultFile=%s logFile=%s",
		mode, f.diff, f.allowPartial, f.refresh, runtimePaths.HeatParamsFile, runtimePaths.ResultFile, runtimePaths.LogFile)

	cfg, err := config.Load(runtimePaths.HeatParamsFile)
	if err != nil {
		writeFailure(runtimePaths.ResultFile, "input", fmt.Sprintf("failed to load heat params: %v", err))
		logger.Errorf("failed to load heat params: %v", err)
		fmt.Fprintf(stderr, "failed to load heat params: %v\n", err)
		return 1
	}

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

	// Load previous successful state to detect actual changes (e.g. KUBE_TAG
	// changed vs stale IS_UPGRADE=true).
	previousState, _ := state.Load(runtimePaths.StateFile)

	// Stream Pulumi resource-level diff output only when requested.
	var eventCh chan events.EngineEvent
	done := make(chan struct{})
	if f.diff || f.debug {
		eventCh = make(chan events.EngineEvent, 100)
		go func() {
			renderer.StreamEvents(eventCh)
			close(done)
		}()
	} else {
		close(done)
	}

	runResult, reconcileState, err := reconcile.Run(ctx, mode, f.diff, f.refresh, f.debug, f.parallelism, cfg, runtimePaths, reconcilePlan, moduleapi.Request{
		Apply:                mode != "preview",
		AllowPartial:         f.allowPartial,
		Logger:               logger,
		Paths:                runtimePaths,
		PreviousKubeTag:      previousState.LastKubeTag,
		PreviousCARotationID: previousState.LastCARotationID,
	}, eventCh)
	safeCloseEngineEvents(eventCh)
	<-done // wait for event stream to drain
	if err != nil {
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

	if mode != "preview" {
		if err := state.Write(runtimePaths.StateFile, reconcileState); err != nil {
			writeFailure(runtimePaths.ResultFile, "state", fmt.Sprintf("failed to persist reconciler state: %v", err))
			logger.Errorf("failed to persist reconciler state: %v", err)
			fmt.Fprintf(stderr, "failed to persist reconciler state: %v\n", err)
			return 1
		}
		logger.Infof("persisted reconciler state stateFile=%s", runtimePaths.StateFile)
	} else {
		logger.Infof("skipped reconciler state persistence for preview mode")
	}

	if err := journal.MarkCompleted(runtimePaths.RunStateFile, mode, runResult.Summary); err != nil {
		writeFailure(runtimePaths.ResultFile, "journal", fmt.Sprintf("failed to persist completion state: %v", err))
		logger.Errorf("failed to persist completion state: %v", err)
		fmt.Fprintf(stderr, "failed to persist completion state: %v\n", err)
		return 1
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
