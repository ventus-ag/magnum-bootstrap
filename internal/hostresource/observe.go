package hostresource

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

type ProviderReadyResource[S any] interface {
	Apply(executor *host.Executor) (ApplyResult, error)
	Observe(executor *host.Executor) (S, error)
	Diff(state S) DriftResult
	Register(ctx *pulumi.Context, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error)
}

type DriftResult struct {
	Changed bool
	Reasons []string
}

func newDriftResult(reasons ...string) DriftResult {
	return DriftResult{Changed: len(reasons) > 0, Reasons: reasons}
}

func pulumiStringArray(values []string) pulumi.StringArray {
	result := make(pulumi.StringArray, 0, len(values))
	for _, value := range values {
		result = append(result, pulumi.String(value))
	}
	return result
}

func ChildResourceOptions(parent pulumi.Resource, inherited ...pulumi.ResourceOption) []pulumi.ResourceOption {
	options := append([]pulumi.ResourceOption{}, inherited...)
	options = append(options, pulumi.Parent(parent))
	return options
}

func ChildResourceOptionsWithDeps(parent pulumi.Resource, inherited []pulumi.ResourceOption, deps ...pulumi.Resource) []pulumi.ResourceOption {
	options := ChildResourceOptions(parent, inherited...)
	if len(deps) > 0 {
		options = append(options, pulumi.DependsOn(deps))
	}
	return options
}
