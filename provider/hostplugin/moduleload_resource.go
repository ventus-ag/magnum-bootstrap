package hostplugin

import (
	"context"
	"strings"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type ModuleLoad struct{}

type ModuleLoadArgs struct {
	Path    string   `pulumi:"path"`
	Modules []string `pulumi:"modules"`
	Mode    string   `pulumi:"mode"`
}

type ModuleLoadState struct {
	Path                  string   `pulumi:"path"`
	Modules               []string `pulumi:"modules"`
	Mode                  string   `pulumi:"mode"`
	ContentSHA256         string   `pulumi:"contentSha256"`
	ObservedExists        bool     `pulumi:"observedExists"`
	ObservedMode          string   `pulumi:"observedMode"`
	ObservedContentSHA256 string   `pulumi:"observedContentSha256"`
	Drifted               bool     `pulumi:"drifted"`
	DriftReasons          []string `pulumi:"driftReasons"`
}

func (*ModuleLoad) Create(_ context.Context, req infer.CreateRequest[ModuleLoadArgs]) (infer.CreateResponse[ModuleLoadState], error) {
	spec, err := moduleLoadSpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[ModuleLoadState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[ModuleLoadState]{}, err
	}
	state, err := moduleLoadStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[ModuleLoadState]{}, err
	}
	return infer.CreateResponse[ModuleLoadState]{ID: spec.Path, Output: state}, nil
}

func (*ModuleLoad) Update(_ context.Context, req infer.UpdateRequest[ModuleLoadArgs, ModuleLoadState]) (infer.UpdateResponse[ModuleLoadState], error) {
	spec, err := moduleLoadSpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[ModuleLoadState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[ModuleLoadState]{}, err
	}
	state, err := moduleLoadStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[ModuleLoadState]{}, err
	}
	return infer.UpdateResponse[ModuleLoadState]{Output: state}, nil
}

func (*ModuleLoad) Read(_ context.Context, req infer.ReadRequest[ModuleLoadArgs, ModuleLoadState]) (infer.ReadResponse[ModuleLoadArgs, ModuleLoadState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	if inputs.Mode == "" {
		inputs.Mode = req.State.Mode
	}
	if len(inputs.Modules) == 0 {
		inputs.Modules = append([]string(nil), req.State.Modules...)
	}
	spec, err := moduleLoadSpec(inputs)
	if err != nil {
		return infer.ReadResponse[ModuleLoadArgs, ModuleLoadState]{}, err
	}
	state, err := moduleLoadStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[ModuleLoadArgs, ModuleLoadState]{}, err
	}
	return infer.ReadResponse[ModuleLoadArgs, ModuleLoadState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*ModuleLoad) Diff(_ context.Context, req infer.DiffRequest[ModuleLoadArgs, ModuleLoadState]) (infer.DiffResponse, error) {
	spec, err := moduleLoadSpec(req.Inputs)
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
	if !stringSliceEqual(req.State.Modules, spec.Modules) {
		detailed["modules"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	desiredHash := hostresource.BytesSHA256([]byte(strings.Join(spec.Modules, "\n") + "\n"))
	if req.State.ContentSHA256 != desiredHash || req.State.ObservedContentSHA256 != desiredHash {
		detailed["modules"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !req.State.ObservedExists {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*ModuleLoad) Annotate(a infer.Annotator) {
	a.SetToken("index", "ModuleLoad")
	a.Describe(&ModuleLoad{}, "A kernel module load configuration file managed by the Magnum host provider.")
}

func moduleLoadSpec(args ModuleLoadArgs) (hostresource.ModuleLoadSpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.ModuleLoadSpec{}, err
	}
	return hostresource.ModuleLoadSpec{Path: args.Path, Modules: append([]string(nil), args.Modules...), Mode: mode}, nil
}

func moduleLoadStateFromSpec(spec hostresource.ModuleLoadSpec) (ModuleLoadState, error) {
	contentHash := hostresource.BytesSHA256([]byte(strings.Join(spec.Modules, "\n") + "\n"))
	state := ModuleLoadState{Path: spec.Path, Modules: append([]string(nil), spec.Modules...), Mode: modeString(spec.Mode), ContentSHA256: contentHash}
	observed, err := observeFileLike(spec.Path)
	if err != nil {
		return ModuleLoadState{}, err
	}
	drift := diffFileLike(spec.Path, modeString(spec.Mode), contentHash, observed)
	state.ObservedExists = observed.Exists
	state.ObservedMode = observed.Mode
	state.ObservedContentSHA256 = observed.ContentSHA256
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}
