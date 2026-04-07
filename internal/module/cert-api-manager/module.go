package certapimanager

import (
	"context"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "cert-api-manager" }
func (Module) Dependencies() []string { return []string{"master-certificates"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if !cfg.Shared.CertManagerAPI {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	certDir := "/etc/kubernetes/certs"
	change, err := executor.EnsureDir(certDir, 0o550)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Write CA private key (only if it doesn't exist — matches bash idempotency).
	caKeyPath := certDir + "/ca.key"
	change, err = executor.EnsureFile(caKeyPath, []byte(cfg.Shared.CAKey+"\n"), 0o400)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"certManagerApi": "true"},
	}, nil
}

// Destroy removes the CA private key.
func (Module) Destroy(_ context.Context, _ config.Config, req moduleapi.Request) error {
	if req.Logger != nil {
		req.Logger.Infof("cert-api-manager destroy: removing /etc/kubernetes/certs/ca.key")
	}
	_ = os.Remove("/etc/kubernetes/certs/ca.key")

	return nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:CertApiManager", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"certManagerApi": pulumi.Bool(heat.Cfg.Shared.CertManagerAPI),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
