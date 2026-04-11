package clustermetricsserver

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// metricsServerChartVersions maps K8s minor version to the metrics-server
// Helm chart version.
// Source: helm search repo metrics-server/metrics-server --versions
var metricsServerChartVersions = map[string]string{
	"1.35": "3.13.0",
	"1.34": "3.13.0",
	"1.33": "3.13.0",
	"1.32": "3.12.2",
	"1.31": "3.12.2",
	"1.30": "3.12.2",
	"1.29": "3.12.2",
	"1.28": "3.12.2",
	"1.27": "3.12.2",
	"1.26": "3.12.2",
	"1.25": "3.12.2",
	"1.24": "3.11.0",
}

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
		chartVersion = config.LookupByKubeVersion(metricsServerChartVersions, cfg.Shared.KubeVersion)
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
			"args": []interface{}{
				"--kubelet-insecure-tls",
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
