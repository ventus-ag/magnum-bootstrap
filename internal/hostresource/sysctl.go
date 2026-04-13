package hostresource

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type SysctlSpec struct {
	Path      string
	Content   []byte
	Mode      os.FileMode
	ReloadArg []string
}

type SysctlResource struct {
	pulumi.ResourceState
}

func (spec SysctlSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	change, err := executor.EnsureFile(spec.Path, spec.Content, spec.Mode)
	if err != nil {
		return ApplyResult{}, err
	}
	result := singleChange(change)
	if change != nil {
		args := spec.ReloadArg
		if len(args) == 0 {
			args = []string{"--system"}
		}
		_ = executor.Run("sysctl", args...)
	}
	return result, nil
}

func (spec SysctlSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &SysctlResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Sysctl", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"path":          pulumi.String(spec.Path),
		"mode":          pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
		"contentSha256": pulumi.String(BytesSHA256(spec.Content)),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
