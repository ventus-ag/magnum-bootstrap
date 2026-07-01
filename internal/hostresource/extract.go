package hostresource

import (
	"fmt"
	"os"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type ExtractTarSpec struct {
	ArchivePath      string
	Destination      string
	CheckPaths       []string
	ChmodExecutables bool
	// StampPath, when set, records which archive was last extracted. Without
	// it, satisfaction is judged only by CheckPaths existing — binaries left
	// by an OLD archive version satisfy the check and a version bump never
	// re-extracts (the CNI plugins bug). The stamp content is the archive
	// path, which callers make version-specific.
	StampPath string
}

type ExtractTarResource struct {
	pulumi.ResourceState
}

func (spec ExtractTarSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	if spec.isSatisfied() {
		return ApplyResult{}, nil
	}
	// Plain extract: callers stage into a scratch dir and install the binary
	// via an atomic CopySpec, so nothing here overwrites a live daemon binary.
	// `--unlink-first` must NOT be used: archives carrying a directory member
	// (e.g. helm's linux-amd64/) fail on re-extract over an existing non-empty
	// dir with "Cannot unlink: Directory not empty".
	if err := executor.Run("tar", "-C", spec.Destination, "-xzf", spec.ArchivePath); err != nil {
		return ApplyResult{}, fmt.Errorf("extract tar %s: %w", spec.ArchivePath, err)
	}
	if spec.ChmodExecutables {
		if err := executor.Run("chmod", "+x", spec.Destination+"/."); err != nil {
			return ApplyResult{}, fmt.Errorf("chmod extracted files in %s: %w", spec.Destination, err)
		}
	}
	if spec.StampPath != "" {
		if _, err := executor.EnsureFile(spec.StampPath, []byte(spec.ArchivePath+"\n"), 0o644); err != nil {
			return ApplyResult{}, fmt.Errorf("write extract stamp %s: %w", spec.StampPath, err)
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
	if spec.StampPath != "" {
		data, err := os.ReadFile(spec.StampPath)
		if err != nil || strings.TrimSpace(string(data)) != spec.ArchivePath {
			return false
		}
	}
	return true
}
