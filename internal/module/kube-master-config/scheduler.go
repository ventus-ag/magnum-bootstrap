package kubemasterconfig

import (
	"fmt"
	"strings"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/kubeletconfig"
)

const (
	schedulerConfigPath     = "/etc/kubernetes/scheduler-config.yaml"
	schedulerKubeconfigPath = "/etc/kubernetes/scheduler-kubeconfig.yaml"
)

// schedulerScoringStrategy normalises KUBE_SCHEDULER_SCORING_STRATEGY. An
// empty strategy keeps the scheduler on its built-in configuration.
func schedulerScoringStrategy(cfg config.Config) (string, string) {
	raw := strings.TrimSpace(cfg.Shared.KubeSchedulerScoringStrategy)
	var strategy string
	switch strings.ToLower(raw) {
	case "":
		return "", ""
	case "leastallocated":
		strategy = "LeastAllocated"
	case "mostallocated":
		strategy = "MostAllocated"
	default:
		return "", fmt.Sprintf("unknown kube_scheduler_scoring_strategy %q (want LeastAllocated or MostAllocated); keeping the scheduler default", raw)
	}
	if optionHasFlag(cfg.Shared.KubeSchedulerOptions, "config") {
		return "", "kube_scheduler_scoring_strategy ignored: kubescheduler_options sets its own --config"
	}
	return strategy, ""
}

func optionHasFlag(opts, flag string) bool {
	for _, field := range strings.Fields(opts) {
		if field == "--"+flag || strings.HasPrefix(field, "--"+flag+"=") {
			return true
		}
	}
	return false
}

func buildSchedulerArgs(cfg config.Config) string {
	args := "--leader-elect=true --kubeconfig=" + schedulerKubeconfigPath
	if strategy, _ := schedulerScoringStrategy(cfg); strategy != "" {
		// With --config the scheduler ignores --kubeconfig; the file carries it.
		args = "--leader-elect=true --config=" + schedulerConfigPath
	}
	if opts := strings.TrimSpace(cfg.Shared.KubeSchedulerOptions); opts != "" {
		args += " " + opts
	}
	return args
}

// buildSchedulerConfig renders the KubeSchedulerConfiguration for the scoring
// strategy, or "" when the scheduler should run on its defaults.
func buildSchedulerConfig(cfg config.Config) string {
	strategy, _ := schedulerScoringStrategy(cfg)
	if strategy == "" {
		return ""
	}
	// v1 is served from 1.25; v1beta3 was removed in 1.29.
	apiVersion := "kubescheduler.config.k8s.io/v1"
	if major, minor, ok := kubeletconfig.ParseKubeMajorMinor(cfg.Shared.KubeTag); ok && major == 1 && minor < 25 {
		apiVersion = "kubescheduler.config.k8s.io/v1beta3"
	}
	plugins := ""
	if strategy == "MostAllocated" {
		// Balanced allocation and the default spreading (weight 2) pull against
		// packing; drop the former and outweigh the latter.
		plugins = `  plugins:
    score:
      disabled:
      - name: NodeResourcesBalancedAllocation
      enabled:
      - name: NodeResourcesFit
        weight: 5
`
	}
	return fmt.Sprintf(`apiVersion: %s
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: %s
leaderElection:
  leaderElect: true
profiles:
- schedulerName: default-scheduler
%s  pluginConfig:
  - name: NodeResourcesFit
    args:
      scoringStrategy:
        type: %s
        resources:
        - name: cpu
          weight: 1
        - name: memory
          weight: 1
`, apiVersion, schedulerKubeconfigPath, plugins, strategy)
}
