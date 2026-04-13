package clusterflannel

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

func (Module) PhaseID() string        { return "cluster-flannel" }
func (Module) Dependencies() []string { return []string{"cluster-cleanup-deprecated"} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	return clusterhelm.RunNoop(ctx, cfg, req, cfg.Shared.NetworkDriver == "flannel", "flannel", "kube-flannel")
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	if !cfg.IsFirstMaster() || cfg.Shared.NetworkDriver != "flannel" {
		return clusterhelm.RegisterSkipped(ctx, "magnum:cluster:Flannel", name, opts...)
	}

	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:Flannel", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := append(opts, pulumi.Parent(res))

	backend := cfg.Shared.FlannelBackend
	if backend == "" {
		backend = "vxlan"
	}

	podCIDR := cfg.Shared.FlannelNetworkCIDR
	if podCIDR == "" {
		podCIDR = cfg.Shared.PodsNetworkCIDR
	}

	// Only set podCidr and backend — let the chart handle image defaults.
	// Matches the bash script values.yaml which only sets these two fields.
	_, err := clusterhelm.DeployHelmRelease(ctx, name+"-chart", clusterhelm.HelmReleaseArgs{
		ReleaseName: "flannel",
		Namespace:   "kube-flannel",
		Chart:       "flannel",
		Version:     "v0.28.2",
		RepoURL:     "https://flannel-io.github.io/flannel/",
		Values: map[string]interface{}{
			"podCidr": podCIDR,
			"flannel": map[string]interface{}{
				"backend": backend,
			},
		},
	}, childOpts...)
	if err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"networkDriver": pulumi.String("flannel"),
		"flannelTag":    pulumi.String("v0.28.2"),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
