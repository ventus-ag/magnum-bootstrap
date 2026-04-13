package hostplugin

import (
	"context"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type SystemdService struct{}

type SystemdServiceArgs struct {
	Unit            string `pulumi:"unit"`
	SkipIfMissing   bool   `pulumi:"skipIfMissing,optional"`
	Enabled         *bool  `pulumi:"enabled,optional"`
	Active          *bool  `pulumi:"active,optional"`
	Masked          *bool  `pulumi:"masked,optional"`
	Restart         bool   `pulumi:"restart,optional"`
	RestartReason   string `pulumi:"restartReason,optional"`
	DaemonReload    bool   `pulumi:"daemonReload,optional"`
	RestartOnChange bool   `pulumi:"restartOnChange,optional"`
	RestartToken    string `pulumi:"restartToken,optional"`
}

type SystemdServiceState struct {
	Unit            string   `pulumi:"unit"`
	SkipIfMissing   bool     `pulumi:"skipIfMissing"`
	Enabled         *bool    `pulumi:"enabled,optional"`
	Active          *bool    `pulumi:"active,optional"`
	Masked          *bool    `pulumi:"masked,optional"`
	Restart         bool     `pulumi:"restart"`
	RestartReason   string   `pulumi:"restartReason"`
	DaemonReload    bool     `pulumi:"daemonReload"`
	RestartOnChange bool     `pulumi:"restartOnChange"`
	RestartToken    string   `pulumi:"restartToken"`
	ObservedExists  bool     `pulumi:"observedExists"`
	ObservedEnabled bool     `pulumi:"observedEnabled"`
	ObservedActive  bool     `pulumi:"observedActive"`
	ObservedMasked  bool     `pulumi:"observedMasked"`
	Drifted         bool     `pulumi:"drifted"`
	DriftReasons    []string `pulumi:"driftReasons"`
}

func (*SystemdService) Create(_ context.Context, req infer.CreateRequest[SystemdServiceArgs]) (infer.CreateResponse[SystemdServiceState], error) {
	spec := systemdSpec(req.Inputs)
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[SystemdServiceState]{}, err
	}
	state, err := systemdStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[SystemdServiceState]{}, err
	}
	return infer.CreateResponse[SystemdServiceState]{ID: spec.Unit, Output: state}, nil
}

func (*SystemdService) Update(_ context.Context, req infer.UpdateRequest[SystemdServiceArgs, SystemdServiceState]) (infer.UpdateResponse[SystemdServiceState], error) {
	spec := systemdSpec(req.Inputs)
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[SystemdServiceState]{}, err
	}
	state, err := systemdStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[SystemdServiceState]{}, err
	}
	return infer.UpdateResponse[SystemdServiceState]{Output: state}, nil
}

func (*SystemdService) Read(_ context.Context, req infer.ReadRequest[SystemdServiceArgs, SystemdServiceState]) (infer.ReadResponse[SystemdServiceArgs, SystemdServiceState], error) {
	inputs := req.Inputs
	if inputs.Unit == "" {
		inputs.Unit = req.ID
	}
	if inputs.Enabled == nil {
		inputs.Enabled = req.State.Enabled
	}
	if inputs.Active == nil {
		inputs.Active = req.State.Active
	}
	if inputs.Masked == nil {
		inputs.Masked = req.State.Masked
	}
	if inputs.RestartReason == "" {
		inputs.RestartReason = req.State.RestartReason
	}
	if inputs.RestartToken == "" {
		inputs.RestartToken = req.State.RestartToken
	}
	spec := systemdSpec(inputs)
	state, err := systemdStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[SystemdServiceArgs, SystemdServiceState]{}, err
	}
	return infer.ReadResponse[SystemdServiceArgs, SystemdServiceState]{ID: spec.Unit, Inputs: inputs, State: state}, nil
}

func (*SystemdService) Diff(_ context.Context, req infer.DiffRequest[SystemdServiceArgs, SystemdServiceState]) (infer.DiffResponse, error) {
	spec := systemdSpec(req.Inputs)
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.ID != spec.Unit {
		detailed["unit"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if boolPtrDiff(req.State.Enabled, spec.Enabled) {
		detailed["enabled"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if boolPtrDiff(req.State.Active, spec.Active) {
		detailed["active"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if boolPtrDiff(req.State.Masked, spec.Masked) {
		detailed["masked"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.SkipIfMissing != spec.SkipIfMissing {
		detailed["skipIfMissing"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Restart != spec.Restart || req.State.RestartReason != spec.RestartReason || req.State.DaemonReload != spec.DaemonReload || req.State.RestartOnChange != spec.RestartOnChange || req.State.RestartToken != spec.RestartToken {
		detailed["restart"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Drifted {
		detailed["observedActive"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*SystemdService) Annotate(a infer.Annotator) {
	a.SetToken("index", "SystemdService")
	a.Describe(&SystemdService{}, "A systemd unit state managed by the Magnum host provider.")
}

func systemdSpec(args SystemdServiceArgs) hostresource.SystemdServiceSpec {
	return hostresource.SystemdServiceSpec{
		Unit:            args.Unit,
		SkipIfMissing:   args.SkipIfMissing,
		Enabled:         args.Enabled,
		Active:          args.Active,
		Masked:          args.Masked,
		Restart:         args.Restart,
		RestartReason:   args.RestartReason,
		DaemonReload:    args.DaemonReload,
		RestartOnChange: args.RestartOnChange,
		RestartToken:    args.RestartToken,
	}
}

func systemdStateFromSpec(spec hostresource.SystemdServiceSpec) (SystemdServiceState, error) {
	state := SystemdServiceState{
		Unit:            spec.Unit,
		SkipIfMissing:   spec.SkipIfMissing,
		Enabled:         spec.Enabled,
		Active:          spec.Active,
		Masked:          spec.Masked,
		Restart:         spec.Restart,
		RestartReason:   spec.RestartReason,
		DaemonReload:    spec.DaemonReload,
		RestartOnChange: spec.RestartOnChange,
		RestartToken:    spec.RestartToken,
	}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return SystemdServiceState{}, err
	}
	drift := spec.Diff(observed)
	state.ObservedExists = observed.Exists
	state.ObservedEnabled = observed.Enabled
	state.ObservedActive = observed.Active
	state.ObservedMasked = observed.Masked
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}

func boolPtrDiff(left, right *bool) bool {
	if left == nil || right == nil {
		return left != right
	}
	return *left != *right
}
