package hostplugin

import (
	"context"
	"os"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Download struct{}

type DownloadArgs struct {
	URL     string `pulumi:"url"`
	Path    string `pulumi:"path"`
	Mode    string `pulumi:"mode"`
	Retries int    `pulumi:"retries,optional"`
}

type DownloadState struct {
	URL              string   `pulumi:"url"`
	Path             string   `pulumi:"path"`
	Mode             string   `pulumi:"mode"`
	Retries          int      `pulumi:"retries"`
	Checksum         string   `pulumi:"checksum"`
	ObservedExists   bool     `pulumi:"observedExists"`
	ObservedMode     string   `pulumi:"observedMode"`
	ObservedChecksum string   `pulumi:"observedChecksum"`
	Drifted          bool     `pulumi:"drifted"`
	DriftReasons     []string `pulumi:"driftReasons,optional"`
}

func (*Download) Create(ctx context.Context, req infer.CreateRequest[DownloadArgs]) (infer.CreateResponse[DownloadState], error) {
	spec, err := downloadSpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[DownloadState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[DownloadState]{}, err
	}
	state, err := downloadStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[DownloadState]{}, err
	}
	return infer.CreateResponse[DownloadState]{ID: spec.Path, Output: state}, nil
}

func (*Download) Update(ctx context.Context, req infer.UpdateRequest[DownloadArgs, DownloadState]) (infer.UpdateResponse[DownloadState], error) {
	spec, err := downloadSpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[DownloadState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[DownloadState]{}, err
	}
	state, err := downloadStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[DownloadState]{}, err
	}
	return infer.UpdateResponse[DownloadState]{Output: state}, nil
}

func (*Download) Read(_ context.Context, req infer.ReadRequest[DownloadArgs, DownloadState]) (infer.ReadResponse[DownloadArgs, DownloadState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.ID
	}
	if inputs.URL == "" {
		inputs.URL = req.State.URL
	}
	if inputs.Mode == "" {
		inputs.Mode = req.State.Mode
	}
	if inputs.Retries == 0 {
		inputs.Retries = req.State.Retries
	}
	spec, err := downloadSpec(inputs)
	if err != nil {
		return infer.ReadResponse[DownloadArgs, DownloadState]{}, err
	}
	state, err := downloadStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[DownloadArgs, DownloadState]{}, err
	}
	return infer.ReadResponse[DownloadArgs, DownloadState]{ID: spec.Path, Inputs: inputs, State: state}, nil
}

func (*Download) Diff(_ context.Context, req infer.DiffRequest[DownloadArgs, DownloadState]) (infer.DiffResponse, error) {
	spec, err := downloadSpec(req.Inputs)
	if err != nil {
		return infer.DiffResponse{}, err
	}
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.ID != spec.Path {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.URL != spec.URL {
		detailed["url"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Mode != modeString(spec.Mode) || req.State.ObservedMode != modeString(spec.Mode) {
		detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if req.State.Retries != spec.Retries {
		detailed["retries"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !req.State.ObservedExists || req.State.ObservedChecksum != req.State.Checksum {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: false}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Download) Annotate(a infer.Annotator) {
	a.SetToken("index", "Download")
	a.Describe(&Download{}, "A downloaded host file managed by the Magnum host provider.")
}

func downloadSpec(args DownloadArgs) (hostresource.DownloadSpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.DownloadSpec{}, err
	}
	return hostresource.DownloadSpec{URL: args.URL, Path: args.Path, Mode: mode, Retries: args.Retries}, nil
}

func downloadStateFromSpec(spec hostresource.DownloadSpec) (DownloadState, error) {
	state := DownloadState{URL: spec.URL, Path: spec.Path, Mode: modeString(spec.Mode), Retries: spec.Retries}
	observed, err := observeDownload(spec)
	if err != nil {
		return DownloadState{}, err
	}
	state.Checksum = observed.Checksum
	drift := diffDownload(spec, state.Checksum, observed)
	state.ObservedExists = observed.Exists
	state.ObservedMode = observed.Mode
	state.ObservedChecksum = observed.Checksum
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}

type downloadObservedState struct {
	Exists   bool
	Mode     string
	Checksum string
}

func observeDownload(spec hostresource.DownloadSpec) (downloadObservedState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return downloadObservedState{}, nil
		}
		return downloadObservedState{}, err
	}
	data, err := os.ReadFile(spec.Path)
	if err != nil {
		return downloadObservedState{}, err
	}
	return downloadObservedState{Exists: true, Mode: modeString(info.Mode()), Checksum: hostresource.BytesSHA256(data)}, nil
}

func diffDownload(spec hostresource.DownloadSpec, desiredChecksum string, observed downloadObservedState) hostresource.DriftResult {
	var reasons []string
	if !observed.Exists {
		reasons = append(reasons, "downloaded file missing")
		return hostresource.DriftResult{Changed: true, Reasons: reasons}
	}
	if observed.Mode != modeString(spec.Mode) {
		reasons = append(reasons, "downloaded file mode differs")
	}
	if desiredChecksum != "" && observed.Checksum != desiredChecksum {
		reasons = append(reasons, "downloaded file content differs")
	}
	return hostresource.DriftResult{Changed: len(reasons) > 0, Reasons: reasons}
}
