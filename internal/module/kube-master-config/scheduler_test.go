package kubemasterconfig

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
)

func schedCfg(tag, strategy, opts string) config.Config {
	var cfg config.Config
	cfg.Shared.KubeTag = tag
	cfg.Shared.KubeSchedulerScoringStrategy = strategy
	cfg.Shared.KubeSchedulerOptions = opts
	return cfg
}

func TestBuildSchedulerArgs(t *testing.T) {
	for _, tc := range []struct {
		cfg  config.Config
		want string
	}{
		{schedCfg("v1.33.1", "", ""), "--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml"},
		{schedCfg("v1.33.1", "", "--v=4"), "--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml --v=4"},
		{schedCfg("v1.33.1", "mostallocated", ""), "--leader-elect=true --config=/etc/kubernetes/scheduler-config.yaml"},
		{schedCfg("v1.33.1", "MostAllocated", "--config=/etc/kubernetes/files/sched"), "--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml --config=/etc/kubernetes/files/sched"},
		{schedCfg("v1.33.1", "Bogus", ""), "--leader-elect=true --kubeconfig=/etc/kubernetes/scheduler-kubeconfig.yaml"},
	} {
		if got := buildSchedulerArgs(tc.cfg); got != tc.want {
			t.Errorf("buildSchedulerArgs(%+v) = %q, want %q", tc.cfg.Shared, got, tc.want)
		}
	}
	if _, warning := schedulerScoringStrategy(schedCfg("", "Bogus", "")); warning == "" {
		t.Error("an unknown strategy must warn")
	}
}

func TestBuildSchedulerConfig(t *testing.T) {
	if got := buildSchedulerConfig(schedCfg("v1.33.1", "", "")); got != "" {
		t.Fatalf("no strategy must render no config, got %q", got)
	}
	for _, tc := range []struct {
		tag, strategy, apiVersion string
		packing                   bool
	}{
		{"v1.24.17", "MostAllocated", "kubescheduler.config.k8s.io/v1beta3", true},
		{"v1.25.0", "MostAllocated", "kubescheduler.config.k8s.io/v1", true},
		{"v1.36.2", "LeastAllocated", "kubescheduler.config.k8s.io/v1", false},
		{"", "MostAllocated", "kubescheduler.config.k8s.io/v1", true},
	} {
		rendered := buildSchedulerConfig(schedCfg(tc.tag, tc.strategy, ""))
		var doc struct {
			APIVersion       string `json:"apiVersion"`
			Kind             string `json:"kind"`
			ClientConnection struct {
				Kubeconfig string `json:"kubeconfig"`
			} `json:"clientConnection"`
			LeaderElection struct {
				LeaderElect bool `json:"leaderElect"`
			} `json:"leaderElection"`
			Profiles []struct {
				SchedulerName string `json:"schedulerName"`
				Plugins       *struct {
					Score struct {
						Disabled []struct{ Name string } `json:"disabled"`
						Enabled  []struct {
							Name   string
							Weight int
						} `json:"enabled"`
					} `json:"score"`
				} `json:"plugins"`
				PluginConfig []struct {
					Name string `json:"name"`
					Args struct {
						ScoringStrategy struct {
							Type      string `json:"type"`
							Resources []struct {
								Name   string `json:"name"`
								Weight int    `json:"weight"`
							} `json:"resources"`
						} `json:"scoringStrategy"`
					} `json:"args"`
				} `json:"pluginConfig"`
			} `json:"profiles"`
		}
		if err := yaml.UnmarshalStrict([]byte(rendered), &doc); err != nil {
			t.Fatalf("%s/%s: invalid YAML: %v\n%s", tc.tag, tc.strategy, err, rendered)
		}
		if doc.APIVersion != tc.apiVersion || doc.Kind != "KubeSchedulerConfiguration" {
			t.Errorf("%s: apiVersion/kind = %s/%s", tc.tag, doc.APIVersion, doc.Kind)
		}
		if doc.ClientConnection.Kubeconfig != schedulerKubeconfigPath || !doc.LeaderElection.LeaderElect {
			t.Errorf("%s: --config drops --kubeconfig/--leader-elect, the file must carry both", tc.tag)
		}
		profile := doc.Profiles[0]
		if profile.PluginConfig[0].Args.ScoringStrategy.Type != tc.strategy {
			t.Errorf("%s: strategy = %q", tc.tag, profile.PluginConfig[0].Args.ScoringStrategy.Type)
		}
		hasPacking := profile.Plugins != nil && len(profile.Plugins.Score.Disabled) == 1 &&
			profile.Plugins.Score.Disabled[0].Name == "NodeResourcesBalancedAllocation" &&
			profile.Plugins.Score.Enabled[0].Weight == 5
		if hasPacking != tc.packing {
			t.Errorf("%s/%s: bin-packing plugin overrides = %v, want %v", tc.tag, tc.strategy, hasPacking, tc.packing)
		}
		if strings.Contains(rendered, "\t") {
			t.Errorf("tabs in YAML")
		}
	}
}

func BenchmarkBuildSchedulerArgs(b *testing.B) {
	cfg := schedCfg("v1.33.1", "MostAllocated", "--v=2")
	for b.Loop() {
		buildSchedulerArgs(cfg)
		buildSchedulerConfig(cfg)
	}
}
