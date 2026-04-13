package hostresource

import (
	"context"
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type DownloadSpec struct {
	URL     string
	Path    string
	Mode    os.FileMode
	Retries int
}

type DownloadResource struct {
	pulumi.ResourceState
}

func (spec DownloadSpec) Apply(executor *host.Executor) (ApplyResult, error) {
	return spec.ApplyContext(context.Background(), executor)
}

func (spec DownloadSpec) ApplyContext(ctx context.Context, executor *host.Executor) (ApplyResult, error) {
	result, err := spec.ApplyWithResultContext(ctx, executor)
	if err != nil {
		return ApplyResult{}, err
	}
	if result.Change == nil {
		return ApplyResult{Changed: result.Changed}, nil
	}
	return ApplyResult{Changes: []host.Change{*result.Change}, Changed: result.Changed}, nil
}

func (spec DownloadSpec) ApplyWithResult(executor *host.Executor) (host.DownloadResult, error) {
	return spec.ApplyWithResultContext(context.Background(), executor)
}

func (spec DownloadSpec) ApplyWithResultContext(ctx context.Context, executor *host.Executor) (host.DownloadResult, error) {
	if spec.URL == "" {
		return host.DownloadResult{}, nil
	}
	if spec.Retries < 1 {
		spec.Retries = 1
	}
	if !executor.Apply {
		action := host.ActionCreate
		summary := fmt.Sprintf("download file %s from %s", spec.Path, spec.URL)
		if _, err := os.Stat(spec.Path); err == nil {
			action = host.ActionReplace
			summary = fmt.Sprintf("replace file %s from %s", spec.Path, spec.URL)
		} else if err != nil && !os.IsNotExist(err) {
			return host.DownloadResult{}, err
		}
		change := &host.Change{Action: action, Path: spec.Path, Summary: summary}
		return host.DownloadResult{Changed: true, Change: change}, nil
	}

	dl, err := executor.DownloadFileWithRetry(ctx, spec.URL, spec.Path, spec.Mode, spec.Retries)
	if err != nil {
		return host.DownloadResult{}, err
	}
	return dl, nil
}

func (spec DownloadSpec) Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	res := &DownloadResource{}
	if err := ctx.RegisterComponentResource("magnum:host:Download", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"url":     pulumi.String(spec.URL),
		"path":    pulumi.String(spec.Path),
		"mode":    pulumi.String(fmt.Sprintf("%04o", spec.Mode.Perm())),
		"retries": pulumi.Int(spec.Retries),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
