package health

import (
	"context"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState

	Checks pulumi.StringArrayOutput `pulumi:"checks"`
}

func (Module) PhaseID() string {
	return "health"
}

func (Module) Run(_ context.Context, cfg config.Config, _ moduleapi.Request) (moduleapi.Result, error) {
	checks := []string{
		"role=" + cfg.Role().String(),
		"operation=" + cfg.Operation().String(),
		"instance=" + cfg.Shared.InstanceName,
	}

	for _, path := range []string{"/etc/sysconfig/heat-params", "/etc/kubernetes"} {
		if _, err := os.Stat(path); err == nil {
			checks = append(checks, "exists="+path)
		}
	}

	return moduleapi.Result{
		Outputs: map[string]string{
			"checks": strings.Join(checks, ","),
		},
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Health", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"checks": pulumi.StringArray{
			pulumi.String("role=" + cfg.Role().String()),
			pulumi.String("operation=" + cfg.Operation().String()),
			pulumi.String("instance=" + cfg.Shared.InstanceName),
		},
	}); err != nil {
		return nil, err
	}
	return res, nil
}
