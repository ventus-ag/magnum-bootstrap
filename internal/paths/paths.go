package paths

import (
	"os"
	"path/filepath"
)

type Paths struct {
	HeatParamsFile  string
	ResultFile      string
	LogFile         string
	StateFile       string
	RunStateFile    string
	StateBackupDir  string
	PulumiStateDir  string
	PulumiBackend   string
	PulumiBackupDir string
}

func LoadFromEnv() Paths {
	return Paths{
		HeatParamsFile:  envOrDefault("MAGNUM_RECONCILE_HEAT_PARAMS_FILE", "/etc/sysconfig/heat-params"),
		ResultFile:      envOrDefault("MAGNUM_RECONCILE_RESULT_FILE", "/var/lib/magnum/reconciler-last-run.json"),
		LogFile:         envOrDefault("MAGNUM_RECONCILE_LOG_FILE", "/var/log/magnum-reconcile.log"),
		StateFile:       envOrDefault("MAGNUM_RECONCILE_STATE_FILE", "/var/lib/magnum/reconciler-state.json"),
		RunStateFile:    envOrDefault("MAGNUM_RECONCILE_RUN_STATE_FILE", "/var/lib/magnum/reconciler-run.json"),
		StateBackupDir:  envOrDefault("MAGNUM_RECONCILE_STATE_BACKUP_DIR", "/var/lib/magnum/reconciler-state-backups"),
		PulumiStateDir:  envOrDefault("MAGNUM_PULUMI_BACKEND_DIR", "/var/lib/magnum/pulumi"),
		PulumiBackend:   envOrDefault("MAGNUM_PULUMI_BACKEND_URL", "file:///var/lib/magnum/pulumi"),
		PulumiBackupDir: envOrDefault("MAGNUM_PULUMI_BACKUP_DIR", "/var/lib/magnum/pulumi-backups"),
	}
}

// EnsureDirs creates all directories required by the reconciler.
func (p Paths) EnsureDirs() error {
	dirs := []string{
		filepath.Dir(p.ResultFile),
		filepath.Dir(p.LogFile),
		filepath.Dir(p.StateFile),
		filepath.Dir(p.RunStateFile),
		p.StateBackupDir,
		p.PulumiStateDir,
		p.PulumiBackupDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
