package hostresource

import (
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type LineSpec struct {
	Path string
	Line string
	Mode os.FileMode
}

type LineResource struct {
	pulumi.ResourceState
}

type LineState struct {
	Exists       bool
	Mode         string
	ContainsLine bool
}

func (spec LineSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	change, err := executor.EnsureLine(spec.Path, spec.Line, spec.Mode)
	if err != nil {
		return ApplyResult{}, err
	}
	return singleChange(change), nil
}

func (spec LineSpec) Observe(_ *host.Executor) (LineState, error) {
	info, err := os.Stat(spec.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return LineState{}, nil
		}
		return LineState{}, err
	}
	content, err := os.ReadFile(spec.Path)
	if err != nil {
		return LineState{}, err
	}
	return LineState{
		Exists:       true,
		Mode:         fmt.Sprintf("%04o", info.Mode().Perm()),
		ContainsLine: strings.Contains(string(content), spec.Line),
	}, nil
}

func (spec LineSpec) Diff(state LineState) DriftResult {
	var reasons []string
	if !state.Exists {
		reasons = append(reasons, "line file missing")
		return newDriftResult(reasons...)
	}
	desiredMode := fmt.Sprintf("%04o", spec.Mode.Perm())
	if state.Mode != desiredMode {
		reasons = append(reasons, "line file mode differs")
	}
	if !state.ContainsLine {
		reasons = append(reasons, "line missing")
	}
	return newDriftResult(reasons...)
}

func (spec LineSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &LineResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Line", name, res, opts...); err != nil {
		return nil, err
	}
	outputs := pulumi.Map{
		"path":       pulumi.String(spec.Path),
		"mode":       pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
		"lineSha256": pulumi.String(BytesSHA256([]byte(spec.Line))),
	}
	executor := host.NewExecutor(false, nil)
	state, err := spec.Observe(executor)
	if err != nil {
		outputs["observeError"] = pulumi.String(err.Error())
	} else {
		drift := spec.Diff(state)
		outputs["observedExists"] = pulumi.Bool(state.Exists)
		outputs["observedMode"] = pulumi.String(state.Mode)
		outputs["observedContainsLine"] = pulumi.Bool(state.ContainsLine)
		outputs["drifted"] = pulumi.Bool(drift.Changed)
		outputs["driftReasons"] = pulumiStringArray(drift.Reasons)
	}
	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
