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
		// Warn, don't fail: `sysctl --system` exits non-zero if ANY file on
		// the host carries a key the kernel rejects — including files we do
		// not manage. But swallowing it silently reported "converged" when
		// our own keys were never applied.
		if err := executor.Run("sysctl", args...); err != nil && executor.Logger != nil {
			executor.Logger.Warnf("sysctl reload after writing %s failed (settings may not be applied until reboot): %v", spec.Path, err)
		}
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
