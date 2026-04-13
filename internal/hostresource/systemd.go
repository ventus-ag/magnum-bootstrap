package hostresource

import (
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type SystemdServiceSpec struct {
	Unit            string
	SkipIfMissing   bool
	Enabled         *bool
	Active          *bool
	Masked          *bool
	Restart         bool
	RestartReason   string
	DaemonReload    bool
	RestartOnChange bool
	RestartToken    string
}

type SystemdServiceResource struct {
	pulumi.ResourceState
}

type SystemdServiceState struct {
	Exists  bool
	Enabled bool
	Active  bool
	Masked  bool
}

func (spec SystemdServiceSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	if spec.SkipIfMissing && !executor.SystemctlExists(spec.Unit) {
		return ApplyResult{}, nil
	}

	changes := make([]host.Change, 0, 4)

	if spec.Masked != nil && !*spec.Masked && executor.SystemctlIsMasked(spec.Unit) {
		if err := executor.Run("systemctl", "unmask", spec.Unit); err != nil {
			return ApplyResult{}, fmt.Errorf("unmask %s: %w", spec.Unit, err)
		}
		changes = append(changes, host.Change{
			Action:  host.ActionUpdate,
			Path:    spec.Unit,
			Summary: fmt.Sprintf("unmask %s", spec.Unit),
		})
	}

	if spec.DaemonReload {
		if err := executor.Run("systemctl", "daemon-reload"); err != nil {
			return ApplyResult{}, err
		}
		changes = append(changes, host.Change{
			Action:  host.ActionReload,
			Path:    "systemd",
			Summary: "reload systemd manager configuration",
		})
	}

	if spec.Active != nil && !*spec.Active && executor.SystemctlIsActive(spec.Unit) {
		if err := executor.Run("systemctl", "stop", spec.Unit); err != nil {
			return ApplyResult{}, fmt.Errorf("stop %s: %w", spec.Unit, err)
		}
		changes = append(changes, host.Change{
			Action:  host.ActionOther,
			Path:    spec.Unit,
			Summary: fmt.Sprintf("stop %s", spec.Unit),
		})
	}

	if spec.Enabled != nil {
		desiredEnabled := *spec.Enabled
		currentEnabled := executor.SystemctlIsEnabled(spec.Unit)
		if desiredEnabled != currentEnabled {
			action := "enable"
			if !desiredEnabled {
				action = "disable"
			}
			if err := executor.Run("systemctl", action, spec.Unit); err != nil {
				return ApplyResult{}, fmt.Errorf("%s %s: %w", action, spec.Unit, err)
			}
			changes = append(changes, host.Change{
				Action:  host.ActionUpdate,
				Path:    spec.Unit,
				Summary: fmt.Sprintf("%s %s", action, spec.Unit),
			})
		}
	}

	if spec.Masked != nil && *spec.Masked && !executor.SystemctlIsMasked(spec.Unit) {
		if err := executor.Run("systemctl", "mask", spec.Unit); err != nil {
			return ApplyResult{}, fmt.Errorf("mask %s: %w", spec.Unit, err)
		}
		changes = append(changes, host.Change{
			Action:  host.ActionUpdate,
			Path:    spec.Unit,
			Summary: fmt.Sprintf("mask %s", spec.Unit),
		})
	}

	if spec.Restart {
		if err := executor.Run("systemctl", "restart", spec.Unit); err != nil {
			return ApplyResult{}, fmt.Errorf("restart %s: %w", spec.Unit, err)
		}
		reason := ""
		if spec.RestartReason != "" {
			reason = " (" + spec.RestartReason + ")"
		}
		changes = append(changes, host.Change{
			Action:  host.ActionRestart,
			Path:    spec.Unit,
			Summary: fmt.Sprintf("restart %s%s", spec.Unit, reason),
		})
	}

	if spec.Active != nil && *spec.Active && !executor.SystemctlIsActive(spec.Unit) {
		if err := executor.Run("systemctl", "start", spec.Unit); err != nil {
			return ApplyResult{}, fmt.Errorf("start %s: %w", spec.Unit, err)
		}
		changes = append(changes, host.Change{
			Action:  host.ActionCreate,
			Path:    spec.Unit,
			Summary: fmt.Sprintf("start %s", spec.Unit),
		})
	}

	return ApplyResult{Changes: changes, Changed: len(changes) > 0}, nil
}

func (spec SystemdServiceSpec) Observe(executor *host.Executor) (SystemdServiceState, error) {
	if !executor.SystemctlExists(spec.Unit) {
		return SystemdServiceState{}, nil
	}
	return SystemdServiceState{
		Exists:  true,
		Enabled: executor.SystemctlIsEnabled(spec.Unit),
		Active:  executor.SystemctlIsActive(spec.Unit),
		Masked:  executor.SystemctlIsMasked(spec.Unit),
	}, nil
}

func (spec SystemdServiceSpec) Diff(state SystemdServiceState) DriftResult {
	var reasons []string
	if !state.Exists {
		if spec.SkipIfMissing {
			return newDriftResult()
		}
		reasons = append(reasons, "systemd unit missing")
		return newDriftResult(reasons...)
	}
	if spec.Enabled != nil && state.Enabled != *spec.Enabled {
		reasons = append(reasons, "enabled state differs")
	}
	if spec.Active != nil && state.Active != *spec.Active {
		reasons = append(reasons, "active state differs")
	}
	if spec.Masked != nil && state.Masked != *spec.Masked {
		reasons = append(reasons, "masked state differs")
	}
	return newDriftResult(reasons...)
}

func (spec SystemdServiceSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &SystemdServiceResource{}
	if err := ctx.RegisterComponentResource("magnum:host:SystemdService", name, res, opts...); err != nil {
		return nil, err
	}

	outputs := pulumi.Map{
		"unit":            pulumi.String(spec.Unit),
		"skipIfMissing":   pulumi.Bool(spec.SkipIfMissing),
		"restart":         pulumi.Bool(spec.Restart),
		"daemonReload":    pulumi.Bool(spec.DaemonReload),
		"restartOnChange": pulumi.Bool(spec.RestartOnChange),
		"restartToken":    pulumi.String(spec.RestartToken),
	}
	if spec.Enabled != nil {
		outputs["enabled"] = pulumi.Bool(*spec.Enabled)
	}
	if spec.Active != nil {
		outputs["active"] = pulumi.Bool(*spec.Active)
	}
	if spec.Masked != nil {
		outputs["masked"] = pulumi.Bool(*spec.Masked)
	}
	if spec.RestartReason != "" {
		outputs["restartReason"] = pulumi.String(spec.RestartReason)
	}
	executor := host.NewExecutor(false, nil)
	state, err := spec.Observe(executor)
	if err != nil {
		outputs["observeError"] = pulumi.String(err.Error())
	} else {
		drift := spec.Diff(state)
		outputs["observedExists"] = pulumi.Bool(state.Exists)
		outputs["observedEnabled"] = pulumi.Bool(state.Enabled)
		outputs["observedActive"] = pulumi.Bool(state.Active)
		outputs["observedMasked"] = pulumi.Bool(state.Masked)
		outputs["drifted"] = pulumi.Bool(drift.Changed)
		outputs["driftReasons"] = pulumiStringArray(drift.Reasons)
	}

	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
