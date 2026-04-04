package clustermetricsserver

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "cluster-metrics-server" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.MetricsServerEnabled, "metrics-server", "kube-system")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.MetricsServerEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:MetricsServer", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:MetricsServer", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	roleName := cfg.Shared.LeadNodeRoleName
	if roleName == "" {
		roleName = "master"
	}

	prefix := cfg.Shared.ContainerInfraPrefix
	if prefix == "" {
		prefix = "registry.k8s.io/metrics-server/"
	}

	chartVersion := cfg.Shared.MetricsServerChartTag
	if chartVersion == "" {
		chartVersion = "3.11.0"
	}

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "metrics-server",
		Namespace:   "kube-system",
		Chart:       "metrics-server",
		Version:     chartVersion,
		RepoURL:     "https://kubernetes-sigs.github.io/metrics-server",
		Values: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": prefix + "metrics-server",
			},
			"nodeSelector": map[string]interface{}{
				"node-role.kubernetes.io/" + roleName: "",
			},
			"tolerations": []interface{}{
				map[string]interface{}{
					"effect":   "NoSchedule",
					"operator": "Exists",
				},
				map[string]interface{}{
					"key":      "CriticalAddonsOnly",
					"operator": "Exists",
				},
				map[string]interface{}{
					"effect":   "NoExecute",
					"operator": "Exists",
				},
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"metricsServerEnabled": pulumi.Bool(true),
		"chartVersion":         pulumi.String(chartVersion),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
