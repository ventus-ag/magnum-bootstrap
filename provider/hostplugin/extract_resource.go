package hostplugin

import (
	"context"
	"os"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type ExtractTar struct{}

type ExtractTarArgs struct {
	ArchivePath      string   `pulumi:"archivePath"`
	Destination      string   `pulumi:"destination"`
	CheckPaths       []string `pulumi:"checkPaths"`
	ChmodExecutables bool     `pulumi:"chmodExecutables,optional"`
}

type ExtractTarState struct {
	ArchivePath       string   `pulumi:"archivePath"`
	Destination       string   `pulumi:"destination"`
	CheckPaths        []string `pulumi:"checkPaths,optional"`
	ChmodExecutables  bool     `pulumi:"chmodExecutables"`
	ObservedSatisfied bool     `pulumi:"observedSatisfied"`
	MissingCheckPaths []string `pulumi:"missingCheckPaths,optional"`
	Drifted           bool     `pulumi:"drifted"`
	DriftReasons      []string `pulumi:"driftReasons,optional"`
}

func (*ExtractTar) Create(_ context.Context, req infer.CreateRequest[ExtractTarArgs]) (infer.CreateResponse[ExtractTarState], error) {
	spec := extractTarSpec(req.Inputs)
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[ExtractTarState]{}, err
	}
	state, err := extractTarStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[ExtractTarState]{}, err
	}
	return infer.CreateResponse[ExtractTarState]{ID: spec.Destination, Output: state}, nil
}

func (*ExtractTar) Update(_ context.Context, req infer.UpdateRequest[ExtractTarArgs, ExtractTarState]) (infer.UpdateResponse[ExtractTarState], error) {
	spec := extractTarSpec(req.Inputs)
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[ExtractTarState]{}, err
	}
	state, err := extractTarStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[ExtractTarState]{}, err
	}
	return infer.UpdateResponse[ExtractTarState]{Output: state}, nil
}

func (*ExtractTar) Read(_ context.Context, req infer.ReadRequest[ExtractTarArgs, ExtractTarState]) (infer.ReadResponse[ExtractTarArgs, ExtractTarState], error) {
	inputs := req.Inputs
	if inputs.Destination == "" {
		inputs.Destination = req.ID
	}
	if inputs.ArchivePath == "" {
		inputs.ArchivePath = req.State.ArchivePath
	}
	if len(inputs.CheckPaths) == 0 {
		inputs.CheckPaths = append([]string(nil), req.State.CheckPaths...)
	}
	spec := extractTarSpec(inputs)
	state, err := extractTarStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[ExtractTarArgs, ExtractTarState]{}, err
	}
	return infer.ReadResponse[ExtractTarArgs, ExtractTarState]{ID: spec.Destination, Inputs: inputs, State: state}, nil
}

func (*ExtractTar) Diff(_ context.Context, req infer.DiffRequest[ExtractTarArgs, ExtractTarState]) (infer.DiffResponse, error) {
	spec := extractTarSpec(req.Inputs)
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.State.ArchivePath != spec.ArchivePath {
		detailed["archivePath"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.ID != spec.Destination {
		detailed["destination"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if !stringSliceEqual(req.State.CheckPaths, spec.CheckPaths) {
		detailed["checkPaths"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.ChmodExecutables != spec.ChmodExecutables {
		detailed["chmodExecutables"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !req.State.ObservedSatisfied {
		detailed["checkPaths"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*ExtractTar) Annotate(a infer.Annotator) {
	a.SetToken("index", "ExtractTar")
	a.Describe(&ExtractTar{}, "A tar extraction operation managed by the Magnum host provider.")
}

func extractTarSpec(args ExtractTarArgs) hostresource.ExtractTarSpec {
	return hostresource.ExtractTarSpec{ArchivePath: args.ArchivePath, Destination: args.Destination, CheckPaths: append([]string(nil), args.CheckPaths...), ChmodExecutables: args.ChmodExecutables}
}

func extractTarStateFromSpec(spec hostresource.ExtractTarSpec) (ExtractTarState, error) {
	state := ExtractTarState{ArchivePath: spec.ArchivePath, Destination: spec.Destination, CheckPaths: append([]string(nil), spec.CheckPaths...), ChmodExecutables: spec.ChmodExecutables}
	satisfied, missing, err := observeExtractTar(spec)
	if err != nil {
		return ExtractTarState{}, err
	}
	state.ObservedSatisfied = satisfied
	state.MissingCheckPaths = missing
	if !satisfied {
		state.Drifted = true
		state.DriftReasons = []string{"extracted files missing"}
	}
	return state, nil
}

func observeExtractTar(spec hostresource.ExtractTarSpec) (bool, []string, error) {
	missing := make([]string, 0)
	for _, path := range spec.CheckPaths {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				missing = append(missing, path)
				continue
			}
			return false, nil, err
		}
	}
	return len(missing) == 0, missing, nil
}

type fileObservedState struct {
	Exists        bool
	Mode          string
	ContentSHA256 string
}

func observeFileLike(path string) (fileObservedState, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileObservedState{}, nil
		}
		return fileObservedState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileObservedState{}, err
	}
	return fileObservedState{Exists: true, Mode: modeString(info.Mode()), ContentSHA256: hostresource.BytesSHA256(data)}, nil
}

func diffFileLike(path, desiredMode, desiredHash string, observed fileObservedState) hostresource.DriftResult {
	var reasons []string
	if !observed.Exists {
		reasons = append(reasons, "file missing")
		return hostresource.DriftResult{Changed: true, Reasons: reasons}
	}
	if observed.Mode != desiredMode {
		reasons = append(reasons, "file mode differs")
	}
	if observed.ContentSHA256 != desiredHash {
		reasons = append(reasons, "file content differs")
	}
	return hostresource.DriftResult{Changed: len(reasons) > 0, Reasons: reasons}
}

func stringSliceEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
