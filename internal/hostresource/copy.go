package hostresource

import (
	"bytes"
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
		// Dry-run: report a change only if the destination is actually out of
		// sync with the source (content or mode). Mirror EnsureFile so preview
		// matches what an apply would do — otherwise every preview falsely
		// reports a replace even when dest already equals source.
		content, err := os.ReadFile(spec.Source)
		if err != nil {
			// Source not present at plan time (may be produced later in the
			// run): report a planned copy rather than erroring.
			return ApplyResult{Changes: []host.Change{{Action: host.ActionCreate, Path: spec.Path,
				Summary: fmt.Sprintf("copy %s to %s", spec.Source, spec.Path)}}, Changed: true}, nil
		}
		current, readErr := os.ReadFile(spec.Path)
		if readErr != nil {
			if !os.IsNotExist(readErr) {
				return ApplyResult{}, readErr
			}
			return ApplyResult{Changes: []host.Change{{Action: host.ActionCreate, Path: spec.Path,
				Summary: fmt.Sprintf("copy %s to %s", spec.Source, spec.Path)}}, Changed: true}, nil
		}
		if info, statErr := os.Stat(spec.Path); statErr == nil &&
			bytes.Equal(current, content) && info.Mode().Perm() == spec.Mode.Perm() {
			return ApplyResult{}, nil
		}
		return ApplyResult{Changes: []host.Change{{Action: host.ActionReplace, Path: spec.Path,
			Summary: fmt.Sprintf("replace %s from %s", spec.Path, spec.Source)}}, Changed: true}, nil
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
