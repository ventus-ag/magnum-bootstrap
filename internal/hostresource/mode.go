package hostresource

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type ModeSpec struct {
	Path          string
	Mode          os.FileMode
	SkipIfMissing bool
}

type ModeResource struct {
	pulumi.ResourceState
}

type ModeState struct {
	Exists bool
	Mode   string
}

func (spec ModeSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) && spec.SkipIfMissing {
			return ApplyResult{}, nil
		}
		return ApplyResult{}, err
	}
	if info.Mode().Perm() == spec.Mode.Perm() {
		return ApplyResult{}, nil
	}
	change := host.Change{
		Action:  host.ActionUpdate,
		Path:    spec.Path,
		Summary: fmt.Sprintf("set mode on %s to %04o", spec.Path, spec.Mode.Perm()),
	}
	if executor.Apply {
		if err := os.Chmod(spec.Path, spec.Mode); err != nil {
			return ApplyResult{}, err
		}
	}
	return ApplyResult{Changes: []host.Change{change}, Changed: true}, nil
}

func (spec ModeSpec) Observe(_ *host.Executor) (ModeState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return ModeState{}, nil
		}
		return ModeState{}, err
	}
	return ModeState{Exists: true, Mode: fmt.Sprintf("%04o", info.Mode().Perm())}, nil
}

func (spec ModeSpec) Diff(state ModeState) DriftResult {
	var reasons []string
	if !state.Exists {
		if !spec.SkipIfMissing {
			reasons = append(reasons, "path missing for mode enforcement")
		}
		return newDriftResult(reasons...)
	}
	desiredMode := fmt.Sprintf("%04o", spec.Mode.Perm())
	if state.Mode != desiredMode {
		reasons = append(reasons, "mode differs")
	}
	return newDriftResult(reasons...)
}

func (spec ModeSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &ModeResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Mode", name, res, opts...); err != nil {
		return nil, err
	}
	outputs := pulumi.Map{
		"path":          pulumi.String(spec.Path),
		"mode":          pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
		"skipIfMissing": pulumi.Bool(spec.SkipIfMissing),
	}
	executor := host.NewExecutor(false, nil)
	state, err := spec.Observe(executor)
	if err != nil {
		outputs["observeError"] = pulumi.String(err.Error())
	} else {
		drift := spec.Diff(state)
		outputs["observedExists"] = pulumi.Bool(state.Exists)
		outputs["observedMode"] = pulumi.String(state.Mode)
		outputs["drifted"] = pulumi.Bool(drift.Changed)
		outputs["driftReasons"] = pulumiStringArray(drift.Reasons)
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
