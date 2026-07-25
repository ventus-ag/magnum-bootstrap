package heatcontaineragent

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
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

	wedgeChanges := healWedgedAgent(executor, req)
	changes = append(changes, wedgeChanges...)

	if req.Apply && !executor.WaitForSystemctlActive(unitName, 30*time.Second, 2*time.Second) {
		return moduleapi.Result{}, fmt.Errorf("service %s did not become active", unitName)
	}
	return moduleapi.Result{
		Changes: changes,
		Outputs: map[string]string{"service": unitName},
	}, nil
}

// agentWedgeGrace is both the minimum time the unit must have been active
// before it can be judged, and the window of container output examined. A node
// that just booted (or an agent restarted seconds ago) is never misread
// mid-startup.
const agentWedgeGrace = 10 * time.Minute

// healWedgedAgent restarts heat-container-agent when it is running but has
// stopped polling Heat.
//
// The container can stay "up" for years while the agent inside has silently
// died: os-collect-config never polls, so Heat's SoftwareDeployments are never
// collected and never signalled. Heat cannot see this -- it just waits out the
// whole stack timeout, cancels, and leaves stale convergence locks that fail
// every later update before it reaches a node. Observed live on golem-cs-02
// (central-switzerland): TWO of three legacy nodes had agent containers running
// since 2024-07-01 whose last processed deployment was dated 2023-09, so an
// upgrade hung until each was restarted by hand.
//
// Detection is BEHAVIOURAL: the agent's container exists but has produced no
// output for the whole grace window. A live agent logs an os-refresh-config
// cycle every poll, so prolonged total silence is the wedge -- and it is what
// was actually observed (`podman logs heat-container-agent` on both stuck cs-02
// nodes returned nothing at all).
//
// An earlier version keyed off a missing /var/run/heat-config/heat-config
// marker instead. That was wrong: absence of the file proves only that this
// agent does not write that path, not that it is stuck. It made the reconciler
// restart a healthy agent on EVERY periodic run -- caught by the e2e periodic
// scenario, whose node ships a deliberate no-op stub unit (Type=oneshot,
// ExecStart=/bin/true) that is permanently active with no container at all.
// Requiring positive evidence from a real container fixes both: the stub has no
// container, so there is nothing to heal.
//
// PERIODIC ONLY. A run-once executes *underneath* this agent (it is the Heat
// SoftwareDeployment), so restarting it there would kill the in-flight
// heat-config-notify signal and wedge the very update we are running -- the
// same reason the tag-convergence restart below is deferred. The systemd timer
// that drives run-periodic is independent of the agent, so a migrated node
// still self-heals within a day without any Heat involvement.
//
// This cannot rescue a node's FIRST migration: an unmigrated node has no
// reconciler installed, so nothing runs to notice. Those need the agent
// bounced before the upgrade.
func healWedgedAgent(executor *host.Executor, req moduleapi.Request) []host.Change {
	if !req.Periodic {
		return nil
	}
	if !executor.SystemctlIsActive(unitName) {
		// Not running at all: the systemd spec above already drives it active,
		// and an inactive unit is not the silent failure this heals.
		return nil
	}
	active, ok := unitActiveFor(executor, unitName)
	if !ok || active < agentWedgeGrace {
		return nil
	}
	if !agentContainerSilent(executor) {
		return nil
	}

	logf(req, "warn",
		"heat-container-agent has been active %s and its container has logged nothing for %s: it is not polling Heat, so SoftwareDeployments would never be collected. Restarting it.",
		active.Round(time.Second), agentWedgeGrace)
	if !executor.Apply {
		return []host.Change{{Action: host.ActionRestart, Path: unitName, Summary: "restart wedged heat-container-agent (not polling Heat)"}}
	}
	if err := executor.Systemctl(host.ActionRestart, unitName); err != nil {
		// Best-effort: a failed restart must not fail the reconcile, because
		// the agent is not needed for this run to converge the node.
		logf(req, "warn", "could not restart wedged heat-container-agent: %s", err)
		return nil
	}
	return []host.Change{{Action: host.ActionRestart, Path: unitName, Summary: "restarted wedged heat-container-agent (was not polling Heat)"}}
}

// agentContainerSilent reports whether a REAL agent container exists and has
// emitted nothing for the grace window.
//
// Every negative answer is fail-safe (no restart). In particular a node with no
// agent container at all -- a stub unit, a different container runtime, podman
// absent -- returns false, because there is no evidence of a wedge to act on.
// Only an existing container that is demonstrably silent qualifies.
func agentContainerSilent(executor *host.Executor) bool {
	if _, err := executor.RunCapture("podman", "container", "exists", unitName); err != nil {
		return false
	}
	// Both streams: os-refresh-config writes its cycle output to stderr.
	stdout, stderr, err := executor.RunCaptureBoth("podman", "logs",
		"--since", fmt.Sprintf("%dm", int(agentWedgeGrace.Minutes())), unitName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == ""
}

// unitActiveFor reports how long a unit has been active, using systemd's
// monotonic timestamp so a wall-clock step (NTP settling after boot, which is
// exactly when this runs) cannot produce a bogus duration.
func unitActiveFor(executor *host.Executor, unit string) (time.Duration, bool) {
	out, err := executor.RunCapture("systemctl", "show", unit,
		"-p", "ActiveEnterTimestampMonotonic", "--value")
	if err != nil {
		return 0, false
	}
	enteredUsec, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || enteredUsec <= 0 {
		return 0, false
	}
	uptime, ok := readUptime()
	if !ok {
		return 0, false
	}
	active := uptime - time.Duration(enteredUsec)*time.Microsecond
	if active < 0 {
		return 0, false
	}
	return active, true
}

func readUptime() (time.Duration, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(seconds * float64(time.Second)), true
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
