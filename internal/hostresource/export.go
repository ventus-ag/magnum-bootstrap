package hostresource

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type ExportSpec struct {
	Path    string
	VarName string
	Value   string
	Mode    os.FileMode
}

type ExportResource struct {
	pulumi.ResourceState
}

func (spec ExportSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	change, err := executor.UpsertExport(spec.Path, spec.VarName, spec.Value, spec.Mode)
	if err != nil {
		return ApplyResult{}, err
	}
	return singleChange(change), nil
}

func (spec ExportSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &ExportResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Export", name, res, opts...); err != nil {
		return nil, err
	}

	outputs := pulumi.Map{
		"path":        pulumi.String(spec.Path),
		"varName":     pulumi.String(spec.VarName),
		"mode":        pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
		"hasValue":    pulumi.Bool(spec.Value != ""),
		"valueSha256": pulumi.String(BytesSHA256([]byte(spec.Value))),
	}

	if err := ctx.RegisterResourceOutputs(res, outputs); err != nil {
		return nil, err
	}
	return res, nil
}
