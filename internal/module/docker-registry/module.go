package dockerregistry

import (
	"context"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string { return "registry" }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	if cfg.Worker == nil || !cfg.Worker.RegistryEnabled {
		return moduleapi.Result{}, nil
	}

	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	// Write registry config.
	registryConfig := buildRegistryConfig(cfg)
	change, err := executor.EnsureFile("/etc/sysconfig/registry-config.yml", []byte(registryConfig), 0o644)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	// Write registry systemd unit.
	serviceContent := buildRegistryService(cfg)
	change, err = executor.EnsureFile("/etc/systemd/system/registry.service", []byte(serviceContent), 0o644)
	if err != nil {
		return moduleapi.Result{}, err
	}
	if change != nil {
		changes = append(changes, *change)
	}

	if err := executor.Run("systemctl", "daemon-reload"); err != nil {
		return moduleapi.Result{}, err
	}

	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{
			"registryEnabled": "true",
			"registryPort":    fmt.Sprintf("%d", cfg.Worker.RegistryPort),
		},
	}, nil
}

func buildRegistryConfig(cfg config.Config) string {
	insecure := "false"
	if cfg.Worker.RegistryInsecure {
		insecure = "true"
	}
	chunksize := cfg.Worker.RegistryChunksize
	if chunksize == 0 {
		chunksize = 5242880
	}
	return fmt.Sprintf(`version: 0.1
log:
  fields:
    service: registry
storage:
  cache:
    layerinfo: inmemory
  swift:
    authurl: "%s"
    region: "%s"
    username: "%s"
    password: "%s"
    domainid: "%s"
    trustid: "%s"
    container: "%s"
    insecureskipverify: %s
    chunksize: %d
http:
    addr: :5000
`, cfg.Shared.AuthURL, cfg.Worker.SwiftRegion, cfg.Worker.TrusteeUsername,
		cfg.Shared.TrusteePassword, cfg.Worker.TrusteeDomainID, cfg.Shared.TrustID,
		cfg.Worker.RegistryContainer, insecure, chunksize)
}

func buildRegistryService(cfg config.Config) string {
	return fmt.Sprintf(`[Unit]
Description=Docker registry v2
Requires=docker.service
After=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/docker run -d -p %d:5000 --restart=always --name registry -v /etc/sysconfig/registry-config.yml:/etc/docker/registry/config.yml registry:2
ExecStop=/usr/bin/docker rm -f registry

[Install]
WantedBy=multi-user.target
`, cfg.Worker.RegistryPort)
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Registry", name, res, opts...); err != nil {
		return nil, err
	}
	outputs := pulumi.Map{
		"registryEnabled": pulumi.Bool(heat.Cfg.Worker != nil && heat.Cfg.Worker.RegistryEnabled),
	}
	if heat.Cfg.Worker != nil {
		outputs["registryPort"] = pulumi.Int(heat.Cfg.Worker.RegistryPort)
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
