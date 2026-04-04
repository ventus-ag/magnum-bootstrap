package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/spf13/cobra"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/journal"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/paths"
)

type cancelFlags struct {
	backendURL     string
	heatParamsFile string
	stackName      string
}

func newCancelCmd(ctx context.Context, code *int, stdout, stderr io.Writer) *cobra.Command {
	var f cancelFlags
	cmd := &cobra.Command{
		Use:   "cancel",
		Short: "Cancel the current Pulumi update for the local node stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			*code = cancel(ctx, f, stdout, stderr)
			return nil
		},
	}
	cmd.Flags().StringVar(&f.stackName, "stack-name", "", "override the Pulumi stack name to cancel (default: derive from heat-params)")
	cmd.Flags().StringVar(&f.backendURL, "backend-url", "", "override Pulumi backend URL (default: $MAGNUM_PULUMI_BACKEND_URL or file:///var/lib/magnum/pulumi)")
	cmd.Flags().StringVar(&f.heatParamsFile, "heat-params-file", "", "override heat-params file path (default: $MAGNUM_RECONCILE_HEAT_PARAMS_FILE or /etc/sysconfig/heat-params)")
	return cmd
}

func cancel(ctx context.Context, f cancelFlags, stdout, stderr io.Writer) int {
	runtimePaths := paths.LoadFromEnv()
	if f.backendURL != "" {
		runtimePaths.PulumiBackend = f.backendURL
	}
	if f.heatParamsFile != "" {
		runtimePaths.HeatParamsFile = f.heatParamsFile
	}

	logger, err := logging.New(runtimePaths.LogFile, stderr, false)
	if err != nil {
		fmt.Fprintf(stderr, "failed to initialize reconciler log: %v\n", err)
		return 1
	}
	defer logger.Close()

	stackName, err := resolveCancelStackName(f, runtimePaths)
	if err != nil {
		logger.Errorf("failed to resolve cancel stack: %v", err)
		fmt.Fprintf(stderr, "failed to resolve cancel stack: %v\n", err)
		return 1
	}

	logger.Warnf("starting manual cancel stack=%s backend=%s", stackName, runtimePaths.PulumiBackend)

	stack, err := selectControlStack(ctx, stackName, runtimePaths)
	if err != nil {
		logger.Errorf("failed to select pulumi stack stack=%s err=%v", stackName, err)
		fmt.Fprintf(stderr, "failed to select pulumi stack %s: %v\n", stackName, err)
		return 1
	}

	if err := stack.Cancel(ctx); err != nil {
		if isNoRunningUpdateError(err) {
			logger.Infof("manual cancel found no running update stack=%s", stackName)
			recoverInterruptedRun(logger, runtimePaths.RunStateFile)
			fmt.Fprintf(stdout, "no running update for stack=%s\n", stackName)
			return 0
		}
		logger.Errorf("manual cancel failed stack=%s err=%v", stackName, err)
		fmt.Fprintf(stderr, "failed to cancel stack %s: %v\n", stackName, err)
		return 1
	}

	logger.Warnf("manual cancel completed stack=%s", stackName)
	recoverInterruptedRun(logger, runtimePaths.RunStateFile)
	fmt.Fprintf(stdout, "canceled stack=%s\n", stackName)
	return 0
}

func resolveCancelStackName(f cancelFlags, runtimePaths paths.Paths) (string, error) {
	if strings.TrimSpace(f.stackName) != "" {
		return strings.TrimSpace(f.stackName), nil
	}

	cfg, err := config.Load(runtimePaths.HeatParamsFile)
	if err != nil {
		return "", fmt.Errorf("failed to load heat params from %s: %w", runtimePaths.HeatParamsFile, err)
	}
	return cfg.StackName(), nil
}

func selectControlStack(ctx context.Context, stackName string, runtimePaths paths.Paths) (auto.Stack, error) {
	workspaceDir := filepath.Join(runtimePaths.PulumiStateDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return auto.Stack{}, fmt.Errorf("failed to create pulumi workspace dir: %w", err)
	}

	pulumiRoot := filepath.Join(runtimePaths.PulumiStateDir, "cli")
	pulumiCmd, err := auto.InstallPulumiCommand(ctx, &auto.PulumiCommandOptions{
		Root: pulumiRoot,
	})
	if err != nil {
		return auto.Stack{}, fmt.Errorf("failed to install pulumi cli: %w", err)
	}

	envVars := map[string]string{
		"PULUMI_CONFIG_PASSPHRASE": "",
	}
	if runtimePaths.PulumiBackend != "" {
		envVars["PULUMI_BACKEND_URL"] = runtimePaths.PulumiBackend
	}

	ws, err := auto.NewLocalWorkspace(ctx,
		auto.Pulumi(pulumiCmd),
		auto.WorkDir(workspaceDir),
		auto.EnvVars(envVars),
	)
	if err != nil {
		return auto.Stack{}, fmt.Errorf("failed to initialize pulumi workspace: %w", err)
	}

	stack, err := auto.SelectStack(ctx, stackName, ws)
	if err != nil {
		return auto.Stack{}, err
	}
	return stack, nil
}

func recoverInterruptedRun(logger *logging.Logger, runStateFile string) {
	recoveredState, recovered, err := journal.RecoverInterrupted(runStateFile)
	if err != nil {
		if logger != nil {
			logger.Errorf("failed to recover interrupted run state after cancel: %v", err)
		}
		return
	}
	if recovered && logger != nil {
		logger.Warnf("marked previous run as interrupted after cancel mode=%s instance=%s role=%s operation=%s",
			recoveredState.Mode, recoveredState.Instance, recoveredState.Role, recoveredState.Operation)
	}
}

func isNoRunningUpdateError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	patterns := []string{
		"no update is currently running",
		"no update in progress",
		"no stack update in progress",
		"no in-progress update",
	}
	for _, pattern := range patterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
