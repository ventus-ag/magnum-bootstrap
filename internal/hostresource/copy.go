package hostresource

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type CopySpec struct {
	Source string
	Path   string
	Mode   os.FileMode
}

type CopyResource struct {
	pulumi.ResourceState
}

func (spec CopySpec) Apply(executor *host.Executor) (ApplyResult, error) {
	if !executor.Apply {
		action := host.ActionCreate
		summary := fmt.Sprintf("copy %s to %s", spec.Source, spec.Path)
		if _, err := os.Stat(spec.Path); err == nil {
			action = host.ActionReplace
			summary = fmt.Sprintf("replace %s from %s", spec.Path, spec.Source)
		} else if err != nil && !os.IsNotExist(err) {
			return ApplyResult{}, err
		}
		return ApplyResult{Changes: []host.Change{{Action: action, Path: spec.Path, Summary: summary}}, Changed: true}, nil
	}

	change, err := executor.EnsureCopy(spec.Source, spec.Path, spec.Mode)
	if err != nil {
		return ApplyResult{}, err
	}
	return singleChange(change), nil
}

func (spec CopySpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &CopyResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Copy", name, res, opts...); err != nil {
		return nil, err
	}

	outputs := pulumi.Map{
		"source": pulumi.String(spec.Source),
		"path":   pulumi.String(spec.Path),
		"mode":   pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
	}
	if content, err := os.ReadFile(spec.Source); err == nil {
		outputs["sourceSha256"] = pulumi.String(BytesSHA256(content))
	}

	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
