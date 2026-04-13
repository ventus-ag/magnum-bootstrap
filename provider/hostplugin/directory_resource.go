package hostplugin

import (
	"context"
	"os"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Directory struct{}

type DirectoryArgs struct {
	Path string `pulumi:"path"`
	Mode string `pulumi:"mode"`
}

type DirectoryState struct {
	Path           string   `pulumi:"path"`
	Mode           string   `pulumi:"mode"`
	ObservedExists bool     `pulumi:"observedExists"`
	ObservedIsDir  bool     `pulumi:"observedIsDir"`
	ObservedMode   string   `pulumi:"observedMode"`
	Drifted        bool     `pulumi:"drifted"`
	DriftReasons   []string `pulumi:"driftReasons,optional"`
}

func (*Directory) Create(_ context.Context, req infer.CreateRequest[DirectoryArgs]) (infer.CreateResponse[DirectoryState], error) {
	spec, err := directorySpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[DirectoryState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[DirectoryState]{}, err
	}
	state, err := directoryStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[DirectoryState]{}, err
	}
	return infer.CreateResponse[DirectoryState]{ID: spec.Path, Output: state}, nil
}

func (*Directory) Update(_ context.Context, req infer.UpdateRequest[DirectoryArgs, DirectoryState]) (infer.UpdateResponse[DirectoryState], error) {
	spec, err := directorySpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[DirectoryState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[DirectoryState]{}, err
	}
	state, err := directoryStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[DirectoryState]{}, err
	}
	return infer.UpdateResponse[DirectoryState]{Output: state}, nil
}

func (*Directory) Read(_ context.Context, req infer.ReadRequest[DirectoryArgs, DirectoryState]) (infer.ReadResponse[DirectoryArgs, DirectoryState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	spec, err := directorySpec(inputs)
	if err != nil {
		return infer.ReadResponse[DirectoryArgs, DirectoryState]{}, err
	}
	state, err := directoryStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[DirectoryArgs, DirectoryState]{}, err
	}
	return infer.ReadResponse[DirectoryArgs, DirectoryState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*Directory) Delete(_ context.Context, req infer.DeleteRequest[DirectoryState]) (infer.DeleteResponse, error) {
	err := os.RemoveAll(req.State.Path)
	if os.IsNotExist(err) {
		return infer.DeleteResponse{}, nil
	}
	return infer.DeleteResponse{}, err
}

func (*Directory) Diff(_ context.Context, req infer.DiffRequest[DirectoryArgs, DirectoryState]) (infer.DiffResponse, error) {
	spec, err := directorySpec(req.Inputs)
	if err != nil {
		return infer.DiffResponse{}, err
	}
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.ID != spec.Path {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.Mode != modeString(spec.Mode) {
		detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return infer.DiffResponse{}, err
	}
	if drift := spec.Diff(observed); drift.Changed {
		if !observed.Exists || !observed.IsDir {
			detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
		} else if observed.Mode != modeString(spec.Mode) {
			detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
		}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Directory) Annotate(a infer.Annotator) {
	a.SetToken("index", "Directory")
	a.Describe(&Directory{}, "A host directory managed by the Magnum host provider.")
}

func directorySpec(args DirectoryArgs) (hostresource.DirectorySpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.DirectorySpec{}, err
	}
	return hostresource.DirectorySpec{Path: args.Path, Mode: mode}, nil
}

func directoryStateFromSpec(spec hostresource.DirectorySpec) (DirectoryState, error) {
	state := DirectoryState{Path: spec.Path, Mode: modeString(spec.Mode)}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return DirectoryState{}, err
	}
	drift := spec.Diff(observed)
	state.ObservedExists = observed.Exists
	state.ObservedIsDir = observed.IsDir
	state.ObservedMode = observed.Mode
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}
