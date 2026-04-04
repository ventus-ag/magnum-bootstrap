package clusterhelm

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	helmv3 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// SkipResult returns an empty result for modules that should not run
// (e.g., not master-0, or feature disabled).
func SkipResult() (moduleapi.Result, error) {
	return moduleapi.Result{}, nil
}

// WaitForAPI waits up to 5 minutes for the Kubernetes API to become healthy.
// The API server may not be ready immediately after services start.
func WaitForAPI(executor *host.Executor) error {
	for i := 0; i < 60; i++ {
		err := executor.Run("kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "get", "--raw=/healthz")
		if err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("API server not healthy after 5 minutes")
}

// RunNoop is a standard Run implementation for Helm-based addons that only
// need Pulumi Register (no imperative host ops beyond API readiness check).
// releaseName and namespace identify the Helm release to adopt — if it already
// exists (from legacy bash scripts), it is uninstalled first so Pulumi can
// create a fresh managed release.
func RunNoop(_ context.Context, cfg config.Config, req moduleapi.Request, featureEnabled bool, releaseName, namespace string) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() || !featureEnabled {
		return SkipResult()
	}
	if req.Apply {
		executor := host.NewExecutor(req.Apply, req.Logger)
		if err := WaitForAPI(executor); err != nil {
			return moduleapi.Result{}, fmt.Errorf("cluster addon: API not ready: %w", err)
		}
		// Adopt existing Helm release: uninstall the old release so Pulumi
		// can create a managed one. This handles migration from bash scripts
		// that installed charts via `helm install`.
		if releaseName != "" {
			AdoptHelmRelease(executor, releaseName, namespace)
		}
	}
	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

// AdoptHelmRelease checks if a Helm release exists but is NOT yet managed by
// Pulumi (i.e. from legacy bash scripts). If so, it uninstalls it so Pulumi
// can create a fresh managed release. On subsequent runs where Pulumi already
// manages the release, this is a no-op (the marker file prevents re-uninstall).
func AdoptHelmRelease(executor *host.Executor, releaseName, namespace string) {
	markerFile := fmt.Sprintf("/var/lib/magnum/helm-adopted-%s-%s", namespace, releaseName)

	// If marker exists, Pulumi already adopted this release — skip.
	if _, err := os.Stat(markerFile); err == nil {
		return
	}

	// Check if release exists in Helm.
	_, err := executor.RunCapture("helm", "status", releaseName, "-n", namespace)
	if err != nil {
		// Release doesn't exist — write marker and let Pulumi create it.
		_ = os.WriteFile(markerFile, []byte("adopted"), 0o644)
		return
	}

	// Release exists from legacy bash — uninstall it.
	_ = executor.Run("helm", "uninstall", releaseName, "-n", namespace, "--wait")
	_ = os.WriteFile(markerFile, []byte("adopted"), 0o644)
}

// RegisterSkipped registers an empty component for phases that are skipped
// (not master-0 or feature disabled). On first run Pulumi shows "+ create"
// but on subsequent runs these are "= same" (no-op).
func RegisterSkipped(ctx *pulumi.Context, resourceType, name string, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	type empty struct {
		pulumi.ResourceState
	}
	res := &empty{}
	if err := ctx.RegisterComponentResource(resourceType, name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{"skipped": pulumi.Bool(true)}); err != nil {
		return nil, err
	}
	return res, nil
}

// HelmReleaseArgs holds common parameters for deploying a Helm chart.
type HelmReleaseArgs struct {
	ReleaseName string
	Namespace   string
	Chart       string
	Version     string
	RepoURL     string
	Values      map[string]interface{}
}

// DeployHelmRelease creates a Pulumi Helm Release resource. Pre-existing
// releases from legacy bash scripts are uninstalled by RunNoop/AdoptHelmRelease
// before this runs, so no Replace/ForceUpdate is needed.
func DeployHelmRelease(ctx *pulumi.Context, name string, args HelmReleaseArgs, opts ...pulumi.ResourceOption) (*helmv3.Release, error) {
	return helmv3.NewRelease(ctx, name, &helmv3.ReleaseArgs{
		Name:            pulumi.String(args.ReleaseName),
		Namespace:       pulumi.String(args.Namespace),
		Chart:           pulumi.String(args.Chart),
		Version:         pulumi.String(args.Version),
		CreateNamespace: pulumi.Bool(true),
		RepositoryOpts: &helmv3.RepositoryOptsArgs{
			Repo: pulumi.String(args.RepoURL),
		},
		Values: pulumi.Map(toStringMap(args.Values)),
	}, opts...)
}

func toStringMap(m map[string]interface{}) map[string]pulumi.Input {
	result := make(map[string]pulumi.Input, len(m))
	for k, v := range m {
		result[k] = toInput(v)
	}
	return result
}

func toInput(v interface{}) pulumi.Input {
	switch val := v.(type) {
	case string:
		return pulumi.String(val)
	case int:
		return pulumi.Int(val)
	case float64:
		return pulumi.Float64(val)
	case bool:
		return pulumi.Bool(val)
	case map[string]interface{}:
		return pulumi.Map(toStringMap(val))
	case []interface{}:
		arr := make(pulumi.Array, len(val))
		for i, item := range val {
			arr[i] = toInput(item)
		}
		return arr
	default:
		return pulumi.String(fmt.Sprintf("%v", v))
	}
}
