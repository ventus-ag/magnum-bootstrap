package hostplugin

import (
	"context"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type File struct{}

type FileArgs struct {
	Path    string `pulumi:"path"`
	Content string `pulumi:"content"`
	Mode    string `pulumi:"mode"`
	Absent  bool   `pulumi:"absent,optional"`
}

type FileState struct {
	Path                  string   `pulumi:"path"`
	Mode                  string   `pulumi:"mode"`
	Absent                bool     `pulumi:"absent"`
	ContentSHA256         string   `pulumi:"contentSha256"`
	ObservedExists        bool     `pulumi:"observedExists"`
	ObservedIsRegular     bool     `pulumi:"observedIsRegularFile"`
	ObservedMode          string   `pulumi:"observedMode"`
	ObservedContentSHA256 string   `pulumi:"observedContentSha256"`
	Drifted               bool     `pulumi:"drifted"`
	DriftReasons          []string `pulumi:"driftReasons,optional"`
}

func (*File) Create(_ context.Context, req infer.CreateRequest[FileArgs]) (infer.CreateResponse[FileState], error) {
	spec, state, err := fileSpecAndState(req.Inputs, req.DryRun)
	if err != nil {
		return infer.CreateResponse[FileState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[FileState]{}, err
	}
	state, err = fileStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[FileState]{}, err
	}
	return infer.CreateResponse[FileState]{ID: spec.Path, Output: state}, nil
}

func (*File) Update(_ context.Context, req infer.UpdateRequest[FileArgs, FileState]) (infer.UpdateResponse[FileState], error) {
	spec, _, err := fileSpecAndState(req.Inputs, req.DryRun)
	if err != nil {
		return infer.UpdateResponse[FileState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[FileState]{}, err
	}
	state, err := fileStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[FileState]{}, err
	}
	return infer.UpdateResponse[FileState]{Output: state}, nil
}

func (*File) Read(_ context.Context, req infer.ReadRequest[FileArgs, FileState]) (infer.ReadResponse[FileArgs, FileState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	spec, _, err := fileSpecAndState(inputs, false)
	if err != nil {
		return infer.ReadResponse[FileArgs, FileState]{}, err
	}
	state, err := fileStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[FileArgs, FileState]{}, err
	}
	return infer.ReadResponse[FileArgs, FileState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

// Delete is deliberately a state-only no-op. Modules register File resources
// conditionally on observed host state (e.g. cert-api-manager registers
// ca.key only when shouldWriteCAKey(), cert modules only when the file is
// readable), so a resource dropping out of the program is routine and must
// not remove the live file — deleting a working ca.key crashloops
// kube-controller-manager. Disabling the provider (MAGNUM_USE_HOST_PROVIDER=
// false) would likewise unregister every managed file at once. Real removal
// is owned by module Run()/Destroy() logic, matching legacy component behavior.
func (*File) Delete(_ context.Context, _ infer.DeleteRequest[FileState]) (infer.DeleteResponse, error) {
	return infer.DeleteResponse{}, nil
}

func (*File) Diff(_ context.Context, req infer.DiffRequest[FileArgs, FileState]) (infer.DiffResponse, error) {
	spec, _, err := fileSpecAndState(req.Inputs, false)
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
	if req.State.Absent != spec.Absent {
		detailed["absent"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !spec.Absent {
		desiredHash := hostresource.BytesSHA256(spec.Content)
		if req.State.ContentSHA256 != desiredHash {
			detailed["content"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
		}
	}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return infer.DiffResponse{}, err
	}
	if drift := spec.Diff(observed); drift.Changed {
		if spec.Absent {
			if observed.Exists {
				detailed["absent"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
			}
		} else {
			if !observed.Exists || !observed.IsRegularFile {
				detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
			} else {
				if observed.Mode != modeString(spec.Mode) {
					detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
				}
				if observed.ContentSHA256 != hostresource.BytesSHA256(spec.Content) {
					detailed["content"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
				}
			}
		}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*File) Annotate(a infer.Annotator) {
	a.SetToken("index", "File")
	a.Describe(&File{}, "A host file managed by the Magnum host provider.")
}

func fileSpecAndState(args FileArgs, _ bool) (hostresource.FileSpec, FileState, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.FileSpec{}, FileState{}, err
	}
	spec := hostresource.FileSpec{Path: args.Path, Content: []byte(args.Content), Mode: mode, Absent: args.Absent}
	state := FileState{Path: args.Path, Mode: modeString(mode), Absent: args.Absent}
	if !args.Absent {
		state.ContentSHA256 = hostresource.BytesSHA256([]byte(args.Content))
	}
	return spec, state, nil
}

func fileStateFromSpec(spec hostresource.FileSpec) (FileState, error) {
	state := FileState{Path: spec.Path, Mode: modeString(spec.Mode), Absent: spec.Absent}
	if !spec.Absent {
		state.ContentSHA256 = hostresource.BytesSHA256(spec.Content)
	}
	observed, err := spec.Observe(newExecutor(false))
	if err != nil {
		return FileState{}, err
	}
	drift := spec.Diff(observed)
	state.ObservedExists = observed.Exists
	state.ObservedIsRegular = observed.IsRegularFile
	state.ObservedMode = observed.Mode
	state.ObservedContentSHA256 = observed.ContentSHA256
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}
