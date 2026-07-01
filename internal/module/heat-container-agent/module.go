package heatcontaineragent

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
	"github.com/ventus-ag/magnum-bootstrap/provider/hostsdk"
)

const unitName = "heat-container-agent"

// agentUnitPath is the systemd unit ignition writes at first boot on Fedora
// CoreOS. ignition runs only once (user_data_update_policy: IGNORE in the Heat
// templates), so the baked image tag never changes on its own — the reconciler
// converges it here. Converged content is always written to this /etc path:
// it either updates the FCoS unit in place or shadows the Ubuntu vendor unit
// (systemd gives /etc priority over /lib).
const agentUnitPath = "/etc/systemd/system/heat-container-agent.service"

// agentUnitReadPaths are probed in order for the current unit content. Ubuntu
// cloud-init installs the unit under /lib/systemd/system instead of /etc.
var agentUnitReadPaths = []string{
	agentUnitPath,
	"/lib/systemd/system/heat-container-agent.service",
	"/usr/lib/systemd/system/heat-container-agent.service",
}

// agentImageRef matches the "<prefix>heat-container-agent:<tag>" token on the
// podman pull + run lines. The token is whitespace/quote/backslash delimited;
// the "--name heat-container-agent" / "podman stop heat-container-agent" lines
// carry no colon, so they are not matched.
var agentImageRef = regexp.MustCompile(`[^\s'"\\]*heat-container-agent:[^\s'"\\]+`)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

func (Module) PhaseID() string        { return "heat-container-agent" }
func (Module) Dependencies() []string { return []string{"start-services"} }

func (Module) Run(_ context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	executor := host.NewExecutor(req.Apply, req.Logger)
	var changes []host.Change

	// The agent unit is installed by the image bootstrap (ignition on FCoS,
	// cloud-init on Ubuntu), never by the reconciler. A node without it (e.g.
	// a custom image) must not fail the whole phase — there is nothing to
	// converge and nothing to wait on.
	if !executor.SystemctlExists(unitName) {
		logf(req, "warn", "unit %s not known to systemd; skipping heat-container-agent phase", unitName)
		return moduleapi.Result{
			Warnings: []string{fmt.Sprintf("unit %s not present on this node; heat-container-agent convergence skipped", unitName)},
		}, nil
	}

	// Converge the agent image tag toward the desired heat-params value so an
	// agent version bump (e.g. ussuri -> victoria) lands without replacing the
	// node. No-op when the tag is empty (older heat-params lack the key).
	tagChanges, err := reconcileAgentImage(executor, cfg, req)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, tagChanges...)

	result, err := (hostresource.SystemdServiceSpec{
		Unit:    unitName,
		Enabled: hostresource.BoolPtr(true),
		Active:  hostresource.BoolPtr(true),
	}).Apply(executor)
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("reconcile %s: %w", unitName, err)
	}
	changes = append(changes, result.Changes...)

	if req.Apply && !executor.WaitForSystemctlActive(unitName, 30*time.Second, 2*time.Second) {
		return moduleapi.Result{}, fmt.Errorf("service %s did not become active", unitName)
	}
	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"service": unitName},
	}, nil
}

// reconcileAgentImage rewrites the agent unit's image reference to the desired
// "<ContainerInfraPrefix>heat-container-agent:<HeatContainerAgentTag>" and pulls
// it. The restart is deferred to a periodic run: run-once executes UNDER the
// heat-container-agent (a Heat SoftwareDeployment), so restarting it mid-run
// would kill the in-flight heat-config-notify signal and wedge the Heat update.
func reconcileAgentImage(executor *host.Executor, cfg config.Config, req moduleapi.Request) ([]host.Change, error) {
	tag := cfg.Shared.HeatContainerAgentTag
	if tag == "" {
		return nil, nil
	}
	desiredRef := cfg.Shared.ContainerInfraPrefix + "heat-container-agent:" + tag

	var content []byte
	var readPath string
	for _, p := range agentUnitReadPaths {
		data, err := os.ReadFile(p)
		if err == nil {
			content, readPath = data, p
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
	}
	if readPath == "" {
		logf(req, "warn", "agent unit not found at any of %v; skipping image convergence", agentUnitReadPaths)
		return nil, nil
	}

	// An unexpected unit format (no recognizable image token) should be visible
	// but must not wedge the whole node reconcile — the agent may be running fine.
	if !agentImageRef.Match(content) {
		logf(req, "warn", "agent unit %s has no recognizable heat-container-agent image reference; skipping tag convergence", readPath)
		return nil, nil
	}

	newContent := agentImageRef.ReplaceAll(content, []byte(desiredRef))
	if string(newContent) == string(content) {
		return nil, nil // already at the desired image reference
	}

	// Pull first; on failure leave the unit untouched so a missing image can
	// never wedge the agent on a later restart.
	if err := executor.Run("podman", "pull", desiredRef); err != nil {
		logf(req, "warn", "podman pull %s failed; leaving agent image unchanged: %v", desiredRef, err)
		return nil, nil
	}

	ch, err := executor.EnsureFile(agentUnitPath, newContent, 0o644)
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", agentUnitPath, err)
	}
	if err := executor.Systemctl("daemon-reload"); err != nil {
		return nil, fmt.Errorf("daemon-reload after agent unit rewrite: %w", err)
	}

	var changes []host.Change
	if ch != nil {
		changes = append(changes, *ch)
	}

	if req.Periodic {
		if err := executor.Systemctl(host.ActionRestart, unitName); err != nil {
			return nil, fmt.Errorf("restart %s: %w", unitName, err)
		}
		if req.Apply && !executor.WaitForSystemctlActive(unitName, 30*time.Second, 2*time.Second) {
			return nil, fmt.Errorf("service %s did not become active after image update", unitName)
		}
		logf(req, "info", "heat-container-agent image updated to %s and restarted", desiredRef)
	} else {
		logf(req, "info", "heat-container-agent image staged to %s; restart deferred to periodic run", desiredRef)
	}

	return changes, nil
}

func logf(req moduleapi.Request, level, format string, args ...any) {
	if req.Logger == nil {
		return
	}
	if level == "warn" {
		req.Logger.Warnf(format, args...)
		return
	}
	req.Logger.Infof(format, args...)
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
