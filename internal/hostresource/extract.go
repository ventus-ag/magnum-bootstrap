package hostresource

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type ExtractTarSpec struct {
	ArchivePath      string
	Destination      string
	CheckPaths       []string
	ChmodExecutables bool
}

type ExtractTarResource struct {
	pulumi.ResourceState
}

func (spec ExtractTarSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	if spec.isSatisfied() {
		return ApplyResult{}, nil
	}
	if err := executor.Run("tar", "-C", spec.Destination, "-xzf", spec.ArchivePath); err != nil {
		return ApplyResult{}, fmt.Errorf("extract tar %s: %w", spec.ArchivePath, err)
	}
	if spec.ChmodExecutables {
		if err := executor.Run("chmod", "+x", spec.Destination+"/."); err != nil {
			return ApplyResult{}, fmt.Errorf("chmod extracted files in %s: %w", spec.Destination, err)
		}
	}
	return ApplyResult{
		Changes: []host.Change{{
			Action:  host.ActionUpdate,
			Path:    spec.Destination,
			Summary: fmt.Sprintf("extract %s into %s", spec.ArchivePath, spec.Destination),
		}},
		Changed: true,
	}, nil
}

func (spec ExtractTarSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &ExtractTarResource{}
	if err := ctx.RegisterComponentResource("magnum:host:ExtractTar", name, res, opts...); err != nil {
		return nil, err
	}
	checkPaths := make(pulumi.StringArray, 0, len(spec.CheckPaths))
	for _, path := range spec.CheckPaths {
		checkPaths = append(checkPaths, pulumi.String(path))
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"archivePath":      pulumi.String(spec.ArchivePath),
		"destination":      pulumi.String(spec.Destination),
		"checkPaths":       checkPaths,
		"chmodExecutables": pulumi.Bool(spec.ChmodExecutables),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func (spec ExtractTarSpec) isSatisfied() bool {
	if len(spec.CheckPaths) == 0 {
		return false
	}
	for _, path := range spec.CheckPaths {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}
