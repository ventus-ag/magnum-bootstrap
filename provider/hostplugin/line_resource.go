package hostplugin

import (
	"context"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Line struct{}

type LineArgs struct {
	Path string `pulumi:"path"`
	Line string `pulumi:"line"`
	Mode string `pulumi:"mode"`
}

type LineState struct {
	Path                 string   `pulumi:"path"`
	Line                 string   `pulumi:"line"`
	Mode                 string   `pulumi:"mode"`
	ObservedExists       bool     `pulumi:"observedExists"`
	ObservedMode         string   `pulumi:"observedMode"`
	ObservedContainsLine bool     `pulumi:"observedContainsLine"`
	Drifted              bool     `pulumi:"drifted"`
	DriftReasons         []string `pulumi:"driftReasons,optional"`
}

func (*Line) Create(_ context.Context, req infer.CreateRequest[LineArgs]) (infer.CreateResponse[LineState], error) {
	spec, err := lineSpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[LineState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[LineState]{}, err
	}
	state, err := lineStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[LineState]{}, err
	}
	return infer.CreateResponse[LineState]{ID: spec.Path, Output: state}, nil
}

func (*Line) Update(_ context.Context, req infer.UpdateRequest[LineArgs, LineState]) (infer.UpdateResponse[LineState], error) {
	spec, err := lineSpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[LineState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[LineState]{}, err
	}
	state, err := lineStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[LineState]{}, err
	}
	return infer.UpdateResponse[LineState]{Output: state}, nil
}

func (*Line) Read(_ context.Context, req infer.ReadRequest[LineArgs, LineState]) (infer.ReadResponse[LineArgs, LineState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	if inputs.Line == "" {
		inputs.Line = req.State.Line
	}
	if inputs.Mode == "" {
		inputs.Mode = req.State.Mode
	}
	spec, err := lineSpec(inputs)
	if err != nil {
		return infer.ReadResponse[LineArgs, LineState]{}, err
	}
	state, err := lineStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[LineArgs, LineState]{}, err
	}
	return infer.ReadResponse[LineArgs, LineState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*Line) Diff(_ context.Context, req infer.DiffRequest[LineArgs, LineState]) (infer.DiffResponse, error) {
	spec, err := lineSpec(req.Inputs)
	if err != nil {
		return infer.DiffResponse{}, err
	}
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.ID != spec.Path {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.Line != req.Inputs.Line || !req.State.ObservedContainsLine {
		detailed["line"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Mode != modeString(spec.Mode) || req.State.ObservedMode != modeString(spec.Mode) {
		detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !req.State.ObservedExists {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Line) Annotate(a infer.Annotator) {
	a.SetToken("index", "Line")
	a.Describe(&Line{}, "A required line in a host file managed by the Magnum host provider.")
}

func lineSpec(args LineArgs) (hostresource.LineSpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.LineSpec{}, err
	}
	return hostresource.LineSpec{Path: args.Path, Line: args.Line, Mode: mode}, nil
}

func lineStateFromSpec(spec hostresource.LineSpec) (LineState, error) {
	state := LineState{Path: spec.Path, Line: spec.Line, Mode: modeString(spec.Mode)}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return LineState{}, err
	}
	drift := spec.Diff(observed)
	state.ObservedExists = observed.Exists
	state.ObservedMode = observed.Mode
	state.ObservedContainsLine = observed.ContainsLine
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}
