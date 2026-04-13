package hostresource

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type FileSpec struct {
	Path    string
	Content []byte
	Mode    os.FileMode
	Absent  bool
}

type FileResource struct {
	pulumi.ResourceState
}

type FileState struct {
	Exists        bool
	IsRegularFile bool
	Mode          string
	ContentSHA256 string
}

func (spec FileSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	var (
		change *host.Change
		err    error
	)
	if spec.Absent {
		change, err = executor.EnsureAbsent(spec.Path)
	} else {
		change, err = executor.EnsureFile(spec.Path, spec.Content, spec.Mode)
	}
	if err != nil {
		return ApplyResult{}, err
	}
	return singleChange(change), nil
}

func (spec FileSpec) Observe(_ *host.Executor) (FileState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileState{}, nil
		}
		return FileState{}, err
	}
	state := FileState{
		Exists:        true,
		IsRegularFile: info.Mode().IsRegular(),
		Mode:          fmt.Sprintf("%04o", info.Mode().Perm()),
	}
	if info.Mode().IsRegular() {
		content, err := os.ReadFile(spec.Path)
		if err != nil {
			return FileState{}, err
		}
		state.ContentSHA256 = BytesSHA256(content)
	}
	return state, nil
}

func (spec FileSpec) Diff(state FileState) DriftResult {
	var reasons []string
	if spec.Absent {
		if state.Exists {
			reasons = append(reasons, "file should be absent")
		}
		return newDriftResult(reasons...)
	}
	if !state.Exists {
		reasons = append(reasons, "file missing")
		return newDriftResult(reasons...)
	}
	if !state.IsRegularFile {
		reasons = append(reasons, "path is not a regular file")
	}
	desiredMode := fmt.Sprintf("%04o", spec.Mode.Perm())
	if state.Mode != desiredMode {
		reasons = append(reasons, "file mode differs")
	}
	if state.ContentSHA256 != BytesSHA256(spec.Content) {
		reasons = append(reasons, "file content differs")
	}
	return newDriftResult(reasons...)
}

func (spec FileSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &FileResource{}
	if err := ctx.RegisterComponentResource("magnum:host:File", name, res, opts...); err != nil {
		return nil, err
	}

	outputs := pulumi.Map{
		"path":   pulumi.String(spec.Path),
		"mode":   pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
		"absent": pulumi.Bool(spec.Absent),
	}
	if !spec.Absent {
		outputs["contentSha256"] = pulumi.String(BytesSHA256(spec.Content))
	}
	executor := host.NewExecutor(false, nil)
	state, err := spec.Observe(executor)
	if err != nil {
		outputs["observeError"] = pulumi.String(err.Error())
	} else {
		drift := spec.Diff(state)
		outputs["observedExists"] = pulumi.Bool(state.Exists)
		outputs["observedIsRegularFile"] = pulumi.Bool(state.IsRegularFile)
		outputs["observedMode"] = pulumi.String(state.Mode)
		outputs["observedContentSha256"] = pulumi.String(state.ContentSHA256)
		outputs["drifted"] = pulumi.Bool(drift.Changed)
		outputs["driftReasons"] = pulumiStringArray(drift.Reasons)
	}

	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
