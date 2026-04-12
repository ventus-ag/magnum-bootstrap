package clustergpuoperator

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// gpuOperatorChartVersions maps K8s minor version to the NVIDIA GPU Operator
// Helm chart version.
// Source: https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/platform-support.html
//
//	chart v26.3.0 → K8s 1.32-1.35 (current)
//	chart v25.3.4 → K8s 1.29-1.33 (deprecated, critical fixes only)
//	chart v24.9.2 → K8s 1.24-1.31 (end of support)
var gpuOperatorChartVersions = map[string]string{
	"1.35": "v26.3.0",
	"1.34": "v26.3.0",
	"1.33": "v26.3.0",
	"1.32": "v26.3.0",
	"1.31": "v25.3.4",
	"1.30": "v25.3.4",
	"1.29": "v25.3.4",
	"1.28": "v24.9.2",
	"1.27": "v24.9.2",
}

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string     { return "cluster-gpu-operator" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.GPUOperatorEnabled, "gpu-operator", "gpu-operator")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || !cfg.Shared.GPUOperatorEnabled {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:GPUOperator", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:GPUOperator", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	chartVersion := config.LookupByKubeVersion(gpuOperatorChartVersions, cfg.Shared.KubeTag)

	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "gpu-operator",
		Namespace:   "gpu-operator",
		Chart:       "gpu-operator",
		Version:     chartVersion,
		RepoURL:     "https://helm.ngc.nvidia.com/nvidia",
		Values: map[string]interface{}{
			"operator": map[string]interface{}{
				"defaultRuntime": "containerd",
			},
			"driver": map[string]interface{}{
				"enabled": true,
			},
			"toolkit": map[string]interface{}{
				"enabled": true,
			},
			"dcgmExporter": map[string]interface{}{
				"enabled": true,
			},
			"nfd": map[string]interface{}{
				"enabled": true,
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"gpuOperatorEnabled": pulumi.Bool(true),
		"chartVersion":       pulumi.String(chartVersion),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
