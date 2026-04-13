package hostplugin

import (
	"context"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Ownership struct{}

type OwnershipArgs struct {
	Path          string `pulumi:"path"`
	Owner         string `pulumi:"owner"`
	Group         string `pulumi:"group"`
	Recursive     bool   `pulumi:"recursive,optional"`
	SkipIfMissing bool   `pulumi:"skipIfMissing,optional"`
}

type OwnershipState struct {
	Path                   string   `pulumi:"path"`
	Owner                  string   `pulumi:"owner"`
	Group                  string   `pulumi:"group"`
	Recursive              bool     `pulumi:"recursive"`
	SkipIfMissing          bool     `pulumi:"skipIfMissing"`
	ObservedExists         bool     `pulumi:"observedExists"`
	ObservedOwner          string   `pulumi:"observedOwner"`
	ObservedGroup          string   `pulumi:"observedGroup"`
	ObservedRecursiveMatch bool     `pulumi:"observedRecursiveMatch"`
	Drifted                bool     `pulumi:"drifted"`
	DriftReasons           []string `pulumi:"driftReasons"`
}

func (*Ownership) Create(_ context.Context, req infer.CreateRequest[OwnershipArgs]) (infer.CreateResponse[OwnershipState], error) {
	spec := ownershipSpec(req.Inputs)
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[OwnershipState]{}, err
	}
	state, err := ownershipStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[OwnershipState]{}, err
	}
	return infer.CreateResponse[OwnershipState]{ID: spec.Path, Output: state}, nil
}

func (*Ownership) Update(_ context.Context, req infer.UpdateRequest[OwnershipArgs, OwnershipState]) (infer.UpdateResponse[OwnershipState], error) {
	spec := ownershipSpec(req.Inputs)
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[OwnershipState]{}, err
	}
	state, err := ownershipStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[OwnershipState]{}, err
	}
	return infer.UpdateResponse[OwnershipState]{Output: state}, nil
}

func (*Ownership) Read(_ context.Context, req infer.ReadRequest[OwnershipArgs, OwnershipState]) (infer.ReadResponse[OwnershipArgs, OwnershipState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	if inputs.Owner == "" {
		inputs.Owner = req.State.Owner
	}
	if inputs.Group == "" {
		inputs.Group = req.State.Group
	}
	spec := ownershipSpec(inputs)
	state, err := ownershipStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[OwnershipArgs, OwnershipState]{}, err
	}
	return infer.ReadResponse[OwnershipArgs, OwnershipState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*Ownership) Delete(_ context.Context, _ infer.DeleteRequest[OwnershipState]) (infer.DeleteResponse, error) {
	return infer.DeleteResponse{}, nil
}

func (*Ownership) Diff(_ context.Context, req infer.DiffRequest[OwnershipArgs, OwnershipState]) (infer.DiffResponse, error) {
	spec := ownershipSpec(req.Inputs)
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.ID != spec.Path {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.Owner != spec.Owner {
		detailed["owner"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Group != spec.Group {
		detailed["group"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Recursive != spec.Recursive {
		detailed["recursive"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.SkipIfMissing != spec.SkipIfMissing {
		detailed["skipIfMissing"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Drifted {
		detailed["observedOwner"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Ownership) Annotate(a infer.Annotator) {
	a.SetToken("index", "Ownership")
	a.Describe(&Ownership{}, "A host filesystem ownership enforcement resource.")
}

func ownershipSpec(args OwnershipArgs) hostresource.OwnershipSpec {
	return hostresource.OwnershipSpec{Path: args.Path, Owner: args.Owner, Group: args.Group, Recursive: args.Recursive, SkipIfMissing: args.SkipIfMissing}
}

func ownershipStateFromSpec(spec hostresource.OwnershipSpec) (OwnershipState, error) {
	state := OwnershipState{Path: spec.Path, Owner: spec.Owner, Group: spec.Group, Recursive: spec.Recursive, SkipIfMissing: spec.SkipIfMissing}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return OwnershipState{}, err
	}
	drift := spec.Diff(observed)
	state.ObservedExists = observed.Exists
	state.ObservedOwner = observed.Owner
	state.ObservedGroup = observed.Group
	state.ObservedRecursiveMatch = observed.RecursiveMatch
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}
