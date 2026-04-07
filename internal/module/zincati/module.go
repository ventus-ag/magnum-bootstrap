package zincati

import (
	"context"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

const (
	configPath  = "/etc/zincati/config.d/90-disable-auto-updates.toml"
	serviceName = "zincati.service"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "zincati" }
func (Module) Dependencies() []string { return []string{"prereq-validation"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)

	// Zincati is only available on Fedora CoreOS. Skip silently on other OS.
	if !executor.SystemctlExists(serviceName) {
		return moduleapi.Result{}, nil
	}

	var changes []host.Change

	enabled := cfg.Shared.OSAutoUpgradeEnabled
	content := fmt.Sprintf("[updates]\nenabled = %t\n", enabled)

	if ch, err := executor.EnsureFile(configPath, []byte(content), 0o644); err != nil {
		return moduleapi.Result{}, fmt.Errorf("failed to write zincati config: %w", err)
	} else if ch != nil {
		changes = append(changes, *ch)
		req.Restarts.Add(serviceName, "zincati config changed")
	}

	if enabled {
		if err := executor.Run("systemctl", "enable", serviceName); err != nil {
			return moduleapi.Result{}, fmt.Errorf("failed to enable %s: %w", serviceName, err)
		}
		if req.Restarts.NeedsRestart(serviceName) {
			if err := executor.Run("systemctl", "restart", serviceName); err != nil {
				return moduleapi.Result{}, fmt.Errorf("failed to restart %s: %w", serviceName, err)
			}
			changes = append(changes, host.Change{
				Action:  host.ActionRestart,
				Summary: fmt.Sprintf("restart %s (config changed)", serviceName),
			})
		}
	} else {
		if executor.SystemctlIsActive(serviceName) {
			if err := executor.Run("systemctl", "stop", serviceName); err != nil {
				return moduleapi.Result{}, fmt.Errorf("failed to stop %s: %w", serviceName, err)
			}
			changes = append(changes, host.Change{
				Action:  host.ActionOther,
				Summary: fmt.Sprintf("stop %s (OS auto-upgrade disabled)", serviceName),
			})
		}
		if err := executor.Run("systemctl", "disable", serviceName); err != nil {
			return moduleapi.Result{}, fmt.Errorf("failed to disable %s: %w", serviceName, err)
		}
	}

	return moduleapi.Result{Changes: changes}, nil
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:Zincati", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"osAutoUpgradeEnabled": pulumi.Bool(heat.Cfg.Shared.OSAutoUpgradeEnabled),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
