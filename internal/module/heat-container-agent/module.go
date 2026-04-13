package heatcontaineragent

import (
	"context"
	"fmt"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

const unitName = "heat-container-agent"

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "heat-container-agent" }
func (Module) Dependencies() []string { return []string{"start-services"} }

func (Module) Run(_ context.Context, _ config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	result, err := (hostresource.SystemdServiceSpec{
		Unit:    unitName,
		Enabled: hostresource.BoolPtr(true),
		Active:  hostresource.BoolPtr(true),
	}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("reconcile %s: %w", unitName, err)
	}
	if req.Apply && !executor.WaitForSystemctlActive(unitName, 30*time.Second, 2*time.Second) {
		return moduleapi.Result{}, fmt.Errorf("service %s did not become active", unitName)
	}
	return moduleapi.Result{
		Changes: result.Changes,
		Outputs: map[string]string{"service": unitName},
	}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, _ *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:HeatContainerAgent", name, res, opts...); err != nil {
		return nil, err
	}
	childOpts := hostresource.ChildResourceOptions(res, opts...)
	if _, err := hostsdk.RegisterSystemdServiceSpec(ctx, name+"-service", hostresource.SystemdServiceSpec{
		Unit:    unitName,
		Enabled: hostresource.BoolPtr(true),
		Active:  hostresource.BoolPtr(true),
	}, childOpts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"service": pulumi.String(unitName),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
