package clusterdashboard

import (
	"context"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cluster-dashboard" }
func (Module) Dependencies() []string { return []string{"cluster-metrics-server"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.KubeDashboardEnabled, "kubernetes-dashboard", "kubernetes-dashboard")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.KubeDashboardEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:Dashboard", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:Dashboard", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	roleName := cfg.Shared.LeadNodeRoleName
	if roleName == "" {
		roleName = "master"
	}

	chartVersion := strings.TrimPrefix(cfg.Shared.KubeDashboardChartTag, "v")
	if chartVersion == "" {
		chartVersion = "7.14.0"
	}

	nodeSelector := map[string]interface{}{
		"node-role.kubernetes.io/" + roleName: "",
	}
	tolerations := []interface{}{
		map[string]interface{}{
			"key":      "node-role.kubernetes.io/control-plane",
			"operator": "Exists",
			"effect":   "NoSchedule",
		},
		map[string]interface{}{
			"key":      "node-role.kubernetes.io/master",
			"operator": "Exists",
			"effect":   "NoSchedule",
		},
		map[string]interface{}{
			"key":      "CriticalAddonsOnly",
			"operator": "Exists",
		},
	}

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "kubernetes-dashboard",
		Namespace:   "kubernetes-dashboard",
		Chart:       "kubernetes-dashboard",
		Version:     chartVersion,
		RepoURL:     "https://kubernetes-retired.github.io/dashboard/",
		Values: map[string]interface{}{
			"app": map[string]interface{}{
				"scheduling": map[string]interface{}{
					"nodeSelector": nodeSelector,
				},
				"tolerations": tolerations,
			},
			"auth":           map[string]interface{}{"nodeSelector": nodeSelector},
			"api":            map[string]interface{}{"nodeSelector": nodeSelector},
			"web":            map[string]interface{}{"nodeSelector": nodeSelector},
			"metricsScraper": map[string]interface{}{"nodeSelector": nodeSelector},
			"kong": map[string]interface{}{
				"nodeSelector": nodeSelector,
				"tolerations":  tolerations,
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"dashboardEnabled": pulumi.Bool(true),
		"chartVersion":     pulumi.String(chartVersion),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
