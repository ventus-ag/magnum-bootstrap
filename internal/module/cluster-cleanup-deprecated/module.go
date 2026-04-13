package clustercleanupdeprecated

import (
	"context"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	clusterhelm "github.com/ventus-ag/magnum-bootstrap/internal/module/cluster-helm"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "cluster-cleanup-deprecated" }
func (Module) Dependencies() []string { return []string{"cluster-rbac"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() {
		return moduleapi.Result{}, nil
	}
	if req.Apply {
		executor := host.NewExecutor(req.Apply, req.Logger)
		clusterhelm.CleanupVeryOldLegacyAddons(executor)
	}
	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:cluster:CleanupDeprecated", name, res, opts...); err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"firstMaster":          pulumi.Bool(heat.Cfg.IsFirstMaster()),
		"cleanupDeprecated":    pulumi.Bool(true),
		"bestEffortImperative": pulumi.Bool(true),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
