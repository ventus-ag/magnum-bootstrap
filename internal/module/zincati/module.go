package zincati

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
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
	legacyConfig := hostresource.FileSpec{Path: oldConfig, Absent: true}
	legacyResult, err := legacyConfig.Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, legacyResult.Changes...)

	enabled := cfg.Shared.OSAutoUpgradeEnabled
	content := buildConfig(enabled, cfg.ResolveNodeIP())
	configResource := hostresource.FileSpec{
		Path:    configPath,
		Content: []byte(content),
		Mode:    0o644,
	}

	configResult, err := configResource.Apply(executor)
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("failed to write zincati config: %w", err)
	}
	changes = append(changes, configResult.Changes...)

	serviceResource := hostresource.SystemdServiceSpec{Unit: serviceName, SkipIfMissing: true}
	if enabled {
		serviceResource.Enabled = hostresource.BoolPtr(true)
		serviceResource.Restart = configResult.Changed
		serviceResource.RestartReason = "zincati config changed"
		serviceResource.RestartOnChange = true
		serviceResource.RestartToken = hostresource.BytesSHA256([]byte(content))
	} else {
		serviceResource.Active = hostresource.BoolPtr(false)
		serviceResource.Enabled = hostresource.BoolPtr(false)
	}

	serviceResult, err := serviceResource.Apply(executor)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, serviceResult.Changes...)

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
	childOpts := hostresource.ChildResourceOptions(res, opts...)

	enabled := heat.Cfg.Shared.OSAutoUpgradeEnabled
	content := []byte(buildConfig(enabled, heat.Cfg.ResolveNodeIP()))
	legacyConfig := hostresource.FileSpec{Path: "/etc/zincati/config.d/90-disable-auto-updates.toml", Absent: true}
	legacyRes, err := hostsdk.RegisterFileSpec(ctx, name+"-legacy-config", legacyConfig, childOpts...)
	if err != nil {
		return nil, err
	}
	configResource := hostresource.FileSpec{Path: configPath, Content: content, Mode: 0o644}
	configRes, err := hostsdk.RegisterFileSpec(ctx, name+"-config", configResource, childOpts...)
	if err != nil {
		return nil, err
	}
	serviceResource := hostresource.SystemdServiceSpec{Unit: serviceName, SkipIfMissing: true}
	if enabled {
		serviceResource.Enabled = hostresource.BoolPtr(true)
		serviceResource.RestartOnChange = true
		serviceResource.RestartToken = hostresource.BytesSHA256(content)
		serviceResource.RestartReason = "zincati config changed"
	} else {
		serviceResource.Active = hostresource.BoolPtr(false)
		serviceResource.Enabled = hostresource.BoolPtr(false)
	}
	serviceOpts := hostresource.ChildResourceOptionsWithDeps(res, opts, legacyRes, configRes)
	if _, err := hostsdk.RegisterSystemdServiceSpec(ctx, name+"-service", serviceResource, serviceOpts...); err != nil {
		return nil, err
	}

	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"osAutoUpgradeEnabled": pulumi.Bool(heat.Cfg.Shared.OSAutoUpgradeEnabled),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
