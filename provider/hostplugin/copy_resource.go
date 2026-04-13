package hostplugin

import (
	"context"
	"os"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Copy struct{}

type CopyArgs struct {
	Source string `pulumi:"source"`
	Path   string `pulumi:"path"`
	Mode   string `pulumi:"mode"`
}

type CopyState struct {
	Source              string   `pulumi:"source"`
	Path                string   `pulumi:"path"`
	Mode                string   `pulumi:"mode"`
	SourceSHA256        string   `pulumi:"sourceSha256"`
	ObservedExists      bool     `pulumi:"observedExists"`
	ObservedMode        string   `pulumi:"observedMode"`
	ObservedMatchesCopy bool     `pulumi:"observedMatchesSource"`
	Drifted             bool     `pulumi:"drifted"`
	DriftReasons        []string `pulumi:"driftReasons,optional"`
}

func (*Copy) Create(_ context.Context, req infer.CreateRequest[CopyArgs]) (infer.CreateResponse[CopyState], error) {
	spec, err := copySpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[CopyState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[CopyState]{}, err
	}
	state, err := copyStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[CopyState]{}, err
	}
	return infer.CreateResponse[CopyState]{ID: spec.Path, Output: state}, nil
}

func (*Copy) Update(_ context.Context, req infer.UpdateRequest[CopyArgs, CopyState]) (infer.UpdateResponse[CopyState], error) {
	spec, err := copySpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[CopyState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[CopyState]{}, err
	}
	state, err := copyStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[CopyState]{}, err
	}
	return infer.UpdateResponse[CopyState]{Output: state}, nil
}

func (*Copy) Read(_ context.Context, req infer.ReadRequest[CopyArgs, CopyState]) (infer.ReadResponse[CopyArgs, CopyState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	if inputs.Source == "" {
		inputs.Source = req.State.Source
	}
	if inputs.Mode == "" {
		inputs.Mode = req.State.Mode
	}
	spec, err := copySpec(inputs)
	if err != nil {
		return infer.ReadResponse[CopyArgs, CopyState]{}, err
	}
	state, err := copyStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[CopyArgs, CopyState]{}, err
	}
	return infer.ReadResponse[CopyArgs, CopyState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*Copy) Diff(_ context.Context, req infer.DiffRequest[CopyArgs, CopyState]) (infer.DiffResponse, error) {
	spec, err := copySpec(req.Inputs)
	if err != nil {
		return infer.DiffResponse{}, err
	}
	sourceData, err := os.ReadFile(spec.Source)
	if err != nil {
		return infer.DiffResponse{}, err
	}
	desiredHash := hostresource.BytesSHA256(sourceData)
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.ID != spec.Path {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.Source != spec.Source || req.State.SourceSHA256 != desiredHash {
		detailed["source"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Mode != modeString(spec.Mode) || req.State.ObservedMode != modeString(spec.Mode) {
		detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !req.State.ObservedExists || !req.State.ObservedMatchesCopy {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Copy) Annotate(a infer.Annotator) {
	a.SetToken("index", "Copy")
	a.Describe(&Copy{}, "A copied host file managed by the Magnum host provider.")
}

func copySpec(args CopyArgs) (hostresource.CopySpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.CopySpec{}, err
	}
	return hostresource.CopySpec{Source: args.Source, Path: args.Path, Mode: mode}, nil
}

func copyStateFromSpec(spec hostresource.CopySpec) (CopyState, error) {
	state := CopyState{Source: spec.Source, Path: spec.Path, Mode: modeString(spec.Mode)}
	sourceData, err := os.ReadFile(spec.Source)
	if err != nil {
		return CopyState{}, err
	}
	state.SourceSHA256 = hostresource.BytesSHA256(sourceData)
	observed, err := observeCopy(spec, sourceData)
	if err != nil {
		return CopyState{}, err
	}
	drift := diffCopy(spec, observed)
	state.ObservedExists = observed.Exists
	state.ObservedMode = observed.Mode
	state.ObservedMatchesCopy = observed.MatchesSource
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}

type copyObservedState struct {
	Exists        bool
	Mode          string
	MatchesSource bool
}

func observeCopy(spec hostresource.CopySpec, sourceData []byte) (copyObservedState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return copyObservedState{}, nil
		}
		return copyObservedState{}, err
	}
	data, err := os.ReadFile(spec.Path)
	if err != nil {
		return copyObservedState{}, err
	}
	return copyObservedState{
		Exists:        true,
		Mode:          modeString(info.Mode()),
		MatchesSource: hostresource.BytesSHA256(data) == hostresource.BytesSHA256(sourceData),
	}, nil
}

func diffCopy(spec hostresource.CopySpec, observed copyObservedState) hostresource.DriftResult {
	var reasons []string
	if !observed.Exists {
		reasons = append(reasons, "copied file missing")
		return hostresource.DriftResult{Changed: true, Reasons: reasons}
	}
	if observed.Mode != modeString(spec.Mode) {
		reasons = append(reasons, "copied file mode differs")
	}
	if !observed.MatchesSource {
		reasons = append(reasons, "copied file content differs")
	}
	return hostresource.DriftResult{Changed: len(reasons) > 0, Reasons: reasons}
}
