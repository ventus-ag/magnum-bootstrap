package zincati

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

const (
	configPath  = "/etc/zincati/config.d/90-magnum-updates.toml"
	serviceName = "zincati.service"

	// Each node gets a 60-minute window staggered by 10-minute offsets
	// derived from its IP, so nodes don't all reboot at the same time.
	windowLengthMinutes = 60
	staggerIntervalMin  = 10
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

	// Remove old config file from previous versions.
	oldConfig := "/etc/zincati/config.d/90-disable-auto-updates.toml"
	if ch, _ := executor.EnsureAbsent(oldConfig); ch != nil {
		changes = append(changes, *ch)
	}

	enabled := cfg.Shared.OSAutoUpgradeEnabled
	content := buildConfig(enabled, cfg.ResolveNodeIP())

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

// nodeOffset returns a per-node offset in minutes derived from the node IP.
// Nodes with IPs like .5, .6, .7 get offsets 50, 60, 70 (last_octet * 10 mod 360).
// This spreads reboot windows across a 6-hour range so nodes don't all
// upgrade at the same time.
func nodeOffset(nodeIP string) int {
	ip := net.ParseIP(nodeIP)
	if ip == nil {
		return 0
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return (int(ip4[3]) * staggerIntervalMin) % 360
}

func buildConfig(enabled bool, nodeIP string) string {
	if !enabled {
		return "[updates]\nenabled = false\n"
	}

	offset := nodeOffset(nodeIP)
	startHour := offset / 60
	startMin := offset % 60
	startTime := fmt.Sprintf("%02d:%02d", startHour, startMin)

	return fmt.Sprintf(`[updates]
enabled = true
strategy = "periodic"

[updates.periodic]
time_zone = "UTC"

[[updates.periodic.window]]
days = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]
start_time = "%s"
length_minutes = %s
`, startTime, strconv.Itoa(windowLengthMinutes))
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
