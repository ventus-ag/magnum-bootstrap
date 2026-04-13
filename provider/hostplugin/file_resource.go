package hostplugin

import (
	"context"
	"os"

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
	DriftReasons          []string `pulumi:"driftReasons"`
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

func (*File) Delete(_ context.Context, req infer.DeleteRequest[FileState]) (infer.DeleteResponse, error) {
	mode, err := parseMode(req.State.Mode)
	if err != nil {
		mode = 0o644
	}
	spec := hostresource.FileSpec{Path: req.State.Path, Mode: mode, Absent: true}
	_, err = spec.Apply(newExecutor(true))
	if os.IsNotExist(err) {
		return infer.DeleteResponse{}, nil
	}
	return infer.DeleteResponse{}, err
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
	if req.State.Mode != modeString(spec.Mode) || req.State.ObservedMode != modeString(spec.Mode) {
		detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Absent != spec.Absent {
		detailed["absent"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !spec.Absent {
		desiredHash := hostresource.BytesSHA256(spec.Content)
		if req.State.ObservedContentSHA256 != desiredHash {
			detailed["content"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
		}
	}
	if spec.Absent && req.State.ObservedExists {
		detailed["absent"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*File) Annotate(a infer.Annotator) {
	a.SetToken("index", "File")
	a.Describe(&File{}, "A host file managed by the Magnum host provider.")
	args := &FileArgs{}
	a.Describe(&args.Path, "Absolute file path on the host.")
	a.Describe(&args.Content, "Desired file content.")
	a.Describe(&args.Mode, "Desired file mode as an octal string like 0644.")
	a.Describe(&args.Absent, "Whether the file should be absent.")
	state := &FileState{}
	a.Describe(&state.ContentSHA256, "SHA256 of the desired content stored in state instead of raw content.")
	a.Describe(&state.DriftReasons, "Observed reasons the host file differs from desired state.")
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
