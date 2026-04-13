package hostplugin

import (
	"context"
	"os"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Sysctl struct{}

type SysctlArgs struct {
	Path      string   `pulumi:"path"`
	Content   string   `pulumi:"content"`
	Mode      string   `pulumi:"mode"`
	ReloadArg []string `pulumi:"reloadArg,optional"`
}

type SysctlState struct {
	Path                  string   `pulumi:"path"`
	Mode                  string   `pulumi:"mode"`
	ContentSHA256         string   `pulumi:"contentSha256"`
	ReloadArg             []string `pulumi:"reloadArg"`
	ObservedExists        bool     `pulumi:"observedExists"`
	ObservedMode          string   `pulumi:"observedMode"`
	ObservedContentSHA256 string   `pulumi:"observedContentSha256"`
	Drifted               bool     `pulumi:"drifted"`
	DriftReasons          []string `pulumi:"driftReasons"`
}

func (*Sysctl) Create(_ context.Context, req infer.CreateRequest[SysctlArgs]) (infer.CreateResponse[SysctlState], error) {
	spec, err := sysctlSpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[SysctlState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[SysctlState]{}, err
	}
	state, err := sysctlStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[SysctlState]{}, err
	}
	return infer.CreateResponse[SysctlState]{ID: spec.Path, Output: state}, nil
}

func (*Sysctl) Update(_ context.Context, req infer.UpdateRequest[SysctlArgs, SysctlState]) (infer.UpdateResponse[SysctlState], error) {
	spec, err := sysctlSpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[SysctlState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[SysctlState]{}, err
	}
	state, err := sysctlStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[SysctlState]{}, err
	}
	return infer.UpdateResponse[SysctlState]{Output: state}, nil
}

func (*Sysctl) Read(_ context.Context, req infer.ReadRequest[SysctlArgs, SysctlState]) (infer.ReadResponse[SysctlArgs, SysctlState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	if inputs.Mode == "" {
		inputs.Mode = req.State.Mode
	}
	if inputs.Content == "" {
		if data, err := os.ReadFile(inputs.Path); err == nil {
			inputs.Content = string(data)
		}
	}
	if len(inputs.ReloadArg) == 0 {
		inputs.ReloadArg = req.State.ReloadArg
	}
	spec, err := sysctlSpec(inputs)
	if err != nil {
		return infer.ReadResponse[SysctlArgs, SysctlState]{}, err
	}
	state, err := sysctlStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[SysctlArgs, SysctlState]{}, err
	}
	return infer.ReadResponse[SysctlArgs, SysctlState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*Sysctl) Diff(_ context.Context, req infer.DiffRequest[SysctlArgs, SysctlState]) (infer.DiffResponse, error) {
	spec, err := sysctlSpec(req.Inputs)
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
	desiredHash := hostresource.BytesSHA256(spec.Content)
	if req.State.ContentSHA256 != desiredHash || req.State.ObservedContentSHA256 != desiredHash {
		detailed["content"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !stringSliceEqual(req.State.ReloadArg, spec.ReloadArg) {
		detailed["reloadArg"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !req.State.ObservedExists {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Sysctl) Annotate(a infer.Annotator) {
	a.SetToken("index", "Sysctl")
	a.Describe(&Sysctl{}, "A sysctl configuration file managed by the Magnum host provider.")
}

func sysctlSpec(args SysctlArgs) (hostresource.SysctlSpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.SysctlSpec{}, err
	}
	return hostresource.SysctlSpec{Path: args.Path, Content: []byte(args.Content), Mode: mode, ReloadArg: append([]string(nil), args.ReloadArg...)}, nil
}

func sysctlStateFromSpec(spec hostresource.SysctlSpec) (SysctlState, error) {
	state := SysctlState{Path: spec.Path, Mode: modeString(spec.Mode), ContentSHA256: hostresource.BytesSHA256(spec.Content), ReloadArg: append([]string(nil), spec.ReloadArg...)}
	observed, err := observeFileLike(spec.Path)
	if err != nil {
		return SysctlState{}, err
	}
	drift := diffFileLike(spec.Path, modeString(spec.Mode), state.ContentSHA256, observed)
	state.ObservedExists = observed.Exists
	state.ObservedMode = observed.Mode
	state.ObservedContentSHA256 = observed.ContentSHA256
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}
