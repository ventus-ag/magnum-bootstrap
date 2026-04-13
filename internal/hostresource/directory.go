package hostresource

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type DirectorySpec struct {
	Path string
	Mode os.FileMode
}

type DirectoryResource struct {
	pulumi.ResourceState
}

type DirectoryState struct {
	Exists bool
	IsDir  bool
	Mode   string
}

func (spec DirectorySpec) Apply(executor *host.Executor) (ApplyResult, error) {
	change, err := executor.EnsureDir(spec.Path, spec.Mode)
	if err != nil {
		return ApplyResult{}, err
	}
	return singleChange(change), nil
}

func (spec DirectorySpec) Observe(_ *host.Executor) (DirectoryState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return DirectoryState{}, nil
		}
		return DirectoryState{}, err
	}
	return DirectoryState{
		Exists: true,
		IsDir:  info.IsDir(),
		Mode:   fmt.Sprintf("%04o", info.Mode().Perm()),
	}, nil
}

func (spec DirectorySpec) Diff(state DirectoryState) DriftResult {
	var reasons []string
	if !state.Exists {
		reasons = append(reasons, "directory missing")
		return newDriftResult(reasons...)
	}
	if !state.IsDir {
		reasons = append(reasons, "path is not a directory")
	}
	desiredMode := fmt.Sprintf("%04o", spec.Mode.Perm())
	if state.Mode != desiredMode {
		reasons = append(reasons, "directory mode differs")
	}
	return newDriftResult(reasons...)
}

func (spec DirectorySpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &DirectoryResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Directory", name, res, opts...); err != nil {
		return nil, err
	}
	outputs := pulumi.Map{
		"path": pulumi.String(spec.Path),
		"mode": pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
	}
	executor := host.NewExecutor(false, nil)
	state, err := spec.Observe(executor)
	if err != nil {
		outputs["observeError"] = pulumi.String(err.Error())
	} else {
		drift := spec.Diff(state)
		outputs["observedExists"] = pulumi.Bool(state.Exists)
		outputs["observedIsDir"] = pulumi.Bool(state.IsDir)
		outputs["observedMode"] = pulumi.String(state.Mode)
		outputs["drifted"] = pulumi.Bool(drift.Changed)
		outputs["driftReasons"] = pulumiStringArray(drift.Reasons)
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
