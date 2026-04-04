package prereqvalidation

import (
	"context"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState

	Role      pulumi.StringOutput `pulumi:"role"`
	Operation pulumi.StringOutput `pulumi:"operation"`
	Instance  pulumi.StringOutput `pulumi:"instance"`
	Validated pulumi.StringOutput `pulumi:"validated"`
}

func (Module) PhaseID() string {
	return "prereq-validation"
}
func (Module) Dependencies() []string { return nil }

func (Module) Run(_ context.Context, cfg config.Config, _ moduleapi.Request) (moduleapi.Result, error) {
	validated := []string{"INSTANCE_NAME", "NODEGROUP_ROLE", "KUBE_TAG", "ARCH"}
	switch cfg.Role() {
	case config.RoleMaster:
		if cfg.Master == nil {
			return moduleapi.Result{}, fmt.Errorf("master role selected without master configuration")
		}
		if cfg.Master.KubeAPIPublicAddress == "" && cfg.Master.KubeAPIPrivateAddress == "" {
			return moduleapi.Result{}, fmt.Errorf("master configuration is missing API addresses")
		}
	case config.RoleWorker:
		if cfg.Worker == nil {
			return moduleapi.Result{}, fmt.Errorf("worker role selected without worker configuration")
		}
		if cfg.Worker.KubeMasterIP == "" {
			return moduleapi.Result{}, fmt.Errorf("worker configuration is missing KUBE_MASTER_IP")
		}
	default:
		return moduleapi.Result{}, fmt.Errorf("unable to determine node role from heat-params")
	}

	if cfg.Shared.InstanceName == "" {
		return moduleapi.Result{}, fmt.Errorf("INSTANCE_NAME is required")
	}
	if cfg.Shared.KubeTag == "" {
		return moduleapi.Result{}, fmt.Errorf("KUBE_TAG is required")
	}
	if cfg.Shared.Arch == "" {
		return moduleapi.Result{}, fmt.Errorf("ARCH is required")
	}

	return moduleapi.Result{
		Outputs: map[string]string{
			"validated": strings.Join(validated, ","),
		},
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:PrereqValidation", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"role":      pulumi.String(cfg.Role().String()),
		"operation": pulumi.String(cfg.Operation().String()),
		"instance":  pulumi.String(cfg.Shared.InstanceName),
		"validated": pulumi.String("INSTANCE_NAME,NODEGROUP_ROLE,KUBE_TAG,ARCH"),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
