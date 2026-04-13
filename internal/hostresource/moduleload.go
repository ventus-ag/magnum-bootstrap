package hostresource

import (
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type ModuleLoadSpec struct {
	Path    string
	Modules []string
	Mode    os.FileMode
}

type ModuleLoadResource struct {
	pulumi.ResourceState
}

func (spec ModuleLoadSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	if len(spec.Modules) == 0 {
		return ApplyResult{}, nil
	}
	args := make([]string, 0, len(spec.Modules)+1)
	if len(spec.Modules) > 1 {
		args = append(args, "-a")
	}
	args = append(args, spec.Modules...)
	_ = executor.Run("modprobe", args...)
	content := []byte(strings.Join(spec.Modules, "\n") + "\n")
	change, err := executor.EnsureFile(spec.Path, content, spec.Mode)
	if err != nil {
		return ApplyResult{}, err
	}
	return singleChange(change), nil
}

func (spec ModuleLoadSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &ModuleLoadResource{}
	if err := ctx.RegisterComponentResource("magnum:host:ModuleLoad", name, res, opts...); err != nil {
		return nil, err
	}
	modules := make(pulumi.StringArray, 0, len(spec.Modules))
	for _, module := range spec.Modules {
		modules = append(modules, pulumi.String(module))
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"path":    pulumi.String(spec.Path),
		"mode":    pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
		"modules": modules,
	}); err != nil {
		return nil, err
	}
	return res, nil
}
