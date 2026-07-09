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

	// Converge the agent unit toward desired state without replacing the node:
	// the image tag follows the heat-params value (e.g. ussuri -> victoria) and
	// the REQUESTS_CA_BUNDLE path is normalized to a version-stable CA bundle
	// (FCoS 44 dropped the legacy symlink the ignition unit points at).
	unitChanges, err := reconcileAgentUnit(executor, cfg, req)
	if err != nil {
		return moduleapi.Result{}, err
	}
	changes = append(changes, unitChanges...)

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

// canonicalCABundle is the version-stable system trust bundle. update-ca-trust
// regenerates it on every FCoS/RHEL release; the legacy compat symlink
// /etc/pki/tls/certs/ca-bundle.crt that the ignition unit points at was dropped
// in FCoS 44, wedging os-collect-config (no CA to verify the Heat API over TLS).
const canonicalCABundle = "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"

// agentCABundleEnv matches the REQUESTS_CA_BUNDLE=<path> token on the podman run
// line so it can be normalized to canonicalCABundle. Ubuntu units already point
// at /etc/ssl/certs/ca-certificates.crt (also valid), so only rewrite the FCoS
// legacy /etc/pki path.
var agentCABundleEnv = regexp.MustCompile(`REQUESTS_CA_BUNDLE=/etc/pki/tls/certs/ca-bundle\.crt`)

// reconcileAgentUnit converges the agent unit toward desired state without
// replacing the node: it rewrites the image reference to the desired
// "<ContainerInfraPrefix>heat-container-agent:<HeatContainerAgentTag>" (pulled
// first) and normalizes the REQUESTS_CA_BUNDLE path to canonicalCABundle. Both
// are applied in a single read/write so an existing node self-heals the FCoS 44
// CA-path breakage on its next periodic run. The restart is deferred to a
// periodic run: run-once executes UNDER the heat-container-agent (a Heat
// SoftwareDeployment), so restarting it mid-run would kill the in-flight
// heat-config-notify signal and wedge the Heat update.
func reconcileAgentUnit(executor *host.Executor, cfg config.Config, req moduleapi.Request) ([]host.Change, error) {
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
		logf(req, "warn", "agent unit not found at any of %v; skipping convergence", agentUnitReadPaths)
		return nil, nil
	}

	desired := content

	// CA bundle path normalization — always applied, independent of the image
	// tag, so it heals even when heat-params carries no tag or the pull fails.
	desired = agentCABundleEnv.ReplaceAll(desired, []byte("REQUESTS_CA_BUNDLE="+canonicalCABundle))

	// Image tag convergence — only when heat-params carries a tag.
	var desiredRef string
	if tag := cfg.Shared.HeatContainerAgentTag; tag != "" {
		if !agentImageRef.Match(desired) {
			// An unexpected unit format (no recognizable image token) should be
			// visible but must not wedge the reconcile — the agent may be fine.
			logf(req, "warn", "agent unit %s has no recognizable heat-container-agent image reference; skipping tag convergence", readPath)
		} else if replaced := agentImageRef.ReplaceAll(desired, []byte(cfg.Shared.ContainerInfraPrefix+"heat-container-agent:"+tag)); string(replaced) != string(desired) {
			desiredRef = cfg.Shared.ContainerInfraPrefix + "heat-container-agent:" + tag
			// Pull first; on failure leave the image reference untouched so a
			// missing image can never wedge the agent on a later restart. The
			// CA-bundle fix above is still applied.
			if err := executor.Run("podman", "pull", desiredRef); err != nil {
				logf(req, "warn", "podman pull %s failed; leaving agent image unchanged: %v", desiredRef, err)
				desiredRef = ""
			} else {
				desired = replaced
			}
		}
	}

	if string(desired) == string(content) {
		return nil, nil // already converged
	}

	ch, err := executor.EnsureFile(agentUnitPath, desired, 0o644)
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
			return nil, fmt.Errorf("service %s did not become active after unit rewrite", unitName)
		}
		logf(req, "info", "heat-container-agent unit converged (image=%q, ca-bundle normalized) and restarted", desiredRef)
	} else {
		logf(req, "info", "heat-container-agent unit converged (image=%q, ca-bundle normalized); restart deferred to periodic run", desiredRef)
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
