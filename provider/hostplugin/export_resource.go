package hostplugin

import (
	"context"
	"fmt"
	"os"
	"strings"

	providerpkg "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
)

type Export struct{}

type ExportArgs struct {
	Path    string `pulumi:"path"`
	VarName string `pulumi:"varName"`
	Value   string `pulumi:"value"`
	Mode    string `pulumi:"mode"`
}

type ExportState struct {
	Path               string   `pulumi:"path"`
	VarName            string   `pulumi:"varName"`
	Mode               string   `pulumi:"mode"`
	HasValue           bool     `pulumi:"hasValue"`
	ValueSHA256        string   `pulumi:"valueSha256"`
	ObservedExists     bool     `pulumi:"observedExists"`
	ObservedMode       string   `pulumi:"observedMode"`
	ObservedHasDesired bool     `pulumi:"observedHasDesiredValue"`
	Drifted            bool     `pulumi:"drifted"`
	DriftReasons       []string `pulumi:"driftReasons,optional"`
}

func (*Export) Create(_ context.Context, req infer.CreateRequest[ExportArgs]) (infer.CreateResponse[ExportState], error) {
	spec, err := exportSpec(req.Inputs)
	if err != nil {
		return infer.CreateResponse[ExportState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.CreateResponse[ExportState]{}, err
	}
	state, err := exportStateFromSpec(spec)
	if err != nil {
		return infer.CreateResponse[ExportState]{}, err
	}
	return infer.CreateResponse[ExportState]{ID: exportID(spec.Path, spec.VarName), Output: state}, nil
}

func (*Export) Update(_ context.Context, req infer.UpdateRequest[ExportArgs, ExportState]) (infer.UpdateResponse[ExportState], error) {
	spec, err := exportSpec(req.Inputs)
	if err != nil {
		return infer.UpdateResponse[ExportState]{}, err
	}
	if _, err := spec.Apply(newExecutor(!req.DryRun)); err != nil {
		return infer.UpdateResponse[ExportState]{}, err
	}
	state, err := exportStateFromSpec(spec)
	if err != nil {
		return infer.UpdateResponse[ExportState]{}, err
	}
	return infer.UpdateResponse[ExportState]{Output: state}, nil
}

func (*Export) Read(_ context.Context, req infer.ReadRequest[ExportArgs, ExportState]) (infer.ReadResponse[ExportArgs, ExportState], error) {
	inputs := req.Inputs
	if inputs.Path == "" {
		inputs.Path = req.State.Path
	}
	if inputs.VarName == "" {
		inputs.VarName = req.State.VarName
	}
	if inputs.Mode == "" {
		inputs.Mode = req.State.Mode
	}
	spec, err := exportSpec(inputs)
	if err != nil {
		return infer.ReadResponse[ExportArgs, ExportState]{}, err
	}
	state, err := exportStateFromSpec(spec)
	if err != nil {
		return infer.ReadResponse[ExportArgs, ExportState]{}, err
	}
	return infer.ReadResponse[ExportArgs, ExportState]{ID: exportID(spec.Path, spec.VarName), Inputs: inputs, State: state}, nil
}

func (*Export) Delete(_ context.Context, req infer.DeleteRequest[ExportState]) (infer.DeleteResponse, error) {
	mode, err := parseMode(req.State.Mode)
	if err != nil {
		mode = 0o644
	}
	spec := hostresource.ExportSpec{Path: req.State.Path, VarName: req.State.VarName, Value: "", Mode: mode}
	_, err = spec.Apply(newExecutor(true))
	if os.IsNotExist(err) {
		return infer.DeleteResponse{}, nil
	}
	return infer.DeleteResponse{}, err
}

func (*Export) Diff(_ context.Context, req infer.DiffRequest[ExportArgs, ExportState]) (infer.DiffResponse, error) {
	spec, err := exportSpec(req.Inputs)
	if err != nil {
		return infer.DiffResponse{}, err
	}
	detailed := map[string]providerpkg.PropertyDiff{}
	if req.State.Path != spec.Path {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.VarName != spec.VarName {
		detailed["varName"] = providerpkg.PropertyDiff{Kind: providerpkg.UpdateReplace, InputDiff: true}
	}
	if req.State.Mode != modeString(spec.Mode) || req.State.ObservedMode != modeString(spec.Mode) {
		detailed["mode"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	desiredHash := hostresource.BytesSHA256([]byte(spec.Value))
	if req.State.ValueSHA256 != desiredHash || !req.State.ObservedHasDesired {
		detailed["value"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	if !req.State.ObservedExists && spec.Value != "" {
		detailed["path"] = providerpkg.PropertyDiff{Kind: providerpkg.Update, InputDiff: true}
	}
	return infer.DiffResponse{HasChanges: len(detailed) > 0, DetailedDiff: detailed}, nil
}

func (*Export) Annotate(a infer.Annotator) {
	a.SetToken("index", "Export")
	a.Describe(&Export{}, "A shell export line in a host file managed by the Magnum host provider.")
}

func exportSpec(args ExportArgs) (hostresource.ExportSpec, error) {
	mode, err := parseMode(args.Mode)
	if err != nil {
		return hostresource.ExportSpec{}, err
	}
	return hostresource.ExportSpec{Path: args.Path, VarName: args.VarName, Value: args.Value, Mode: mode}, nil
}

func exportStateFromSpec(spec hostresource.ExportSpec) (ExportState, error) {
	state := ExportState{Path: spec.Path, VarName: spec.VarName, Mode: modeString(spec.Mode), HasValue: spec.Value != ""}
	state.ValueSHA256 = hostresource.BytesSHA256([]byte(spec.Value))
	observed, err := observeExport(spec)
	if err != nil {
		return ExportState{}, err
	}
	drift := diffExport(spec, observed)
	state.ObservedExists = observed.Exists
	state.ObservedMode = observed.Mode
	state.ObservedHasDesired = observed.HasDesiredValue
	state.Drifted = drift.Changed
	state.DriftReasons = append([]string(nil), drift.Reasons...)
	return state, nil
}

type exportObservedState struct {
	Exists          bool
	Mode            string
	HasDesiredValue bool
}

func observeExport(spec hostresource.ExportSpec) (exportObservedState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return exportObservedState{}, nil
		}
		return exportObservedState{}, err
	}
	content, err := os.ReadFile(spec.Path)
	if err != nil {
		return exportObservedState{}, err
	}
	desiredLine := renderedExportLine(spec.VarName, spec.Value)
	state := exportObservedState{Exists: true, Mode: modeString(info.Mode()), HasDesiredValue: false}
	for _, line := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		if line == desiredLine {
			state.HasDesiredValue = true
			break
		}
	}
	return state, nil
}

func diffExport(spec hostresource.ExportSpec, observed exportObservedState) hostresource.DriftResult {
	var reasons []string
	if !observed.Exists {
		if spec.Value != "" {
			reasons = append(reasons, "export file missing")
		}
		return hostresource.DriftResult{Changed: len(reasons) > 0, Reasons: reasons}
	}
	if observed.Mode != modeString(spec.Mode) {
		reasons = append(reasons, "export file mode differs")
	}
	if spec.Value != "" && !observed.HasDesiredValue {
		reasons = append(reasons, "export value differs")
	}
	if spec.Value == "" && observed.HasDesiredValue {
		reasons = append(reasons, "export should be absent")
	}
	return hostresource.DriftResult{Changed: len(reasons) > 0, Reasons: reasons}
}

func renderedExportLine(varName, value string) string {
	return fmt.Sprintf("export %s='%s'", varName, strings.ReplaceAll(value, "'", "'\\''"))
}

func exportID(path, varName string) string {
	return path + "::" + varName
}
