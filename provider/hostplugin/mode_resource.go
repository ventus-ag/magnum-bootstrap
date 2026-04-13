package hostplugin

import (
	"context"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Mode struct{}

type ModeArgs struct {
	Path          string `pulumi:"path"`
	Mode          string `pulumi:"mode"`
	SkipIfMissing bool   `pulumi:"skipIfMissing,optional"`
}

type ModeState struct {
	Path           string   `pulumi:"path"`
	Mode           string   `pulumi:"mode"`
	SkipIfMissing  bool     `pulumi:"skipIfMissing"`
	ObservedExists bool     `pulumi:"observedExists"`
	ObservedMode   string   `pulumi:"observedMode"`
	Drifted        bool     `pulumi:"drifted"`
	DriftReasons   []string `pulumi:"driftReasons,optional"`
}

func (*Mode) Create(_ context.Context, req infer.CreateRequest[ModeArgs]) (infer.CreateResponse[ModeState], error) {
	spec, err := modeSpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[ModeState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[ModeState]{}, err
	}
	state, err := modeStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[ModeState]{}, err
	}
	return infer.CreateResponse[ModeState]{ID: spec.Path, Output: state}, nil
}

func (*Mode) Update(_ context.Context, req infer.UpdateRequest[ModeArgs, ModeState]) (infer.UpdateResponse[ModeState], error) {
	spec, err := modeSpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[ModeState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[ModeState]{}, err
	}
	state, err := modeStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[ModeState]{}, err
	}
	return infer.UpdateResponse[ModeState]{Output: state}, nil
}

func (*Mode) Read(_ context.Context, req infer.ReadRequest[ModeArgs, ModeState]) (infer.ReadResponse[ModeArgs, ModeState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	if inputs.Mode == "" {
		inputs.Mode = req.State.Mode
	}
	spec, err := modeSpec(inputs)
	if err != nil {
		return infer.ReadResponse[ModeArgs, ModeState]{}, err
	}
	state, err := modeStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[ModeArgs, ModeState]{}, err
	}
	return infer.ReadResponse[ModeArgs, ModeState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*Mode) Delete(_ context.Context, _ infer.DeleteRequest[ModeState]) (infer.DeleteResponse, error) {
	return infer.DeleteResponse{}, nil
}

func (*Mode) Diff(_ context.Context, req infer.DiffRequest[ModeArgs, ModeState]) (infer.DiffResponse, error) {
	spec, err := modeSpec(req.Inputs)
	if err != nil {
		return infer.DiffResponse{}, err
	}
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.ID != spec.Path {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.Mode != modeString(spec.Mode) || req.State.ObservedMode != modeString(spec.Mode) {
		detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.SkipIfMissing != spec.SkipIfMissing {
		detailed["skipIfMissing"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Drifted {
		detailed["observedMode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Mode) Annotate(a infer.Annotator) {
	a.SetToken("index", "Mode")
	a.Describe(&Mode{}, "A host filesystem mode enforcement resource.")
}

func modeSpec(args ModeArgs) (hostresource.ModeSpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.ModeSpec{}, err
	}
	return hostresource.ModeSpec{Path: args.Path, Mode: mode, SkipIfMissing: args.SkipIfMissing}, nil
}

func modeStateFromSpec(spec hostresource.ModeSpec) (ModeState, error) {
	state := ModeState{Path: spec.Path, Mode: modeString(spec.Mode), SkipIfMissing: spec.SkipIfMissing}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return ModeState{}, err
	}
	drift := spec.Diff(observed)
	state.ObservedExists = observed.Exists
	state.ObservedMode = observed.Mode
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}
