package clusterhelm

import (
	"context"
	"fmt"
	"os"

	helmv3 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

// SkipResult returns an empty result for modules that should not run
// (e.g., not master-0, or feature disabled).
func SkipResult() (moduleapi.Result, error) {
	return moduleapi.Result{}, nil
}

// RunNoop is a standard Run implementation for Helm-based addons that only
// need Pulumi Register.
// releaseName and namespace identify the Helm release to adopt — if it already
// exists (from legacy bash scripts), it is prepared for Pulumi import.
func RunNoop(_ context.Context, cfg config.Config, req moduleapi.Request, featureEnabled bool, releaseName, namespace string) (moduleapi.Result, error) {
	if !cfg.IsFirstMaster() || !featureEnabled {
		return SkipResult()
	}
	if req.Apply && releaseName != "" {
		executor := host.NewExecutor(req.Apply, req.Logger)
		AdoptHelmRelease(executor, releaseName, namespace)
	}
	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

// adoptedMarkerPath returns the path for the "already adopted" marker file.
func adoptedMarkerPath(namespace, releaseName string) string {
	return fmt.Sprintf("/var/lib/magnum/helm-adopted-%s-%s", namespace, releaseName)
}

// importMarkerPath returns the path for the "needs import" marker file.
func importMarkerPath(namespace, releaseName string) string {
	return fmt.Sprintf("/var/lib/magnum/helm-import-%s-%s", namespace, releaseName)
}

// AdoptHelmRelease prepares a legacy Helm release for Pulumi adoption.
//
// On first migration run:
//   - If the release exists in Helm but not yet managed by Pulumi, writes an
//     import marker. Register() will use pulumi.Import() to adopt it in-place.
//
// On second run (if import failed):
//   - Import marker still present, release still exists → uninstalls the release
//     (fallback) so Pulumi can create a fresh managed release.
//
// On subsequent runs:
//   - Adopted marker exists → no-op.
func AdoptHelmRelease(executor *host.Executor, releaseName, namespace string) {
	adopted := adoptedMarkerPath(namespace, releaseName)
	importing := importMarkerPath(namespace, releaseName)

	// Already adopted by Pulumi. Still check for stale releases left behind
	// by a previous Pulumi up that crashed after Helm install but before
	// state was saved — the release exists in Helm but not in Pulumi state.
	if _, err := os.Stat(adopted); err == nil {
		if _, helmErr := executor.RunCapture("helm", "status", releaseName, "-n", namespace); helmErr == nil {
			// Release exists despite adopted marker — uninstall the stale
			// copy so Pulumi can create a fresh managed release.
			_ = executor.Run("helm", "uninstall", releaseName, "-n", namespace, "--no-hooks")
		}
		return
	}

	// Check if the release exists in Helm.
	_, err := executor.RunCapture("helm", "status", releaseName, "-n", namespace)
	if err != nil {
		// Release doesn't exist → write adopted marker, Pulumi will create fresh.
		_ = os.WriteFile(adopted, []byte("adopted"), 0o644)
		_ = os.Remove(importing)
		return
	}

	// Release exists. Check if a previous import attempt failed.
	if _, err := os.Stat(importing); err == nil {
		// Import marker exists from a previous run → import already failed once.
		// Fallback: uninstall the release so Pulumi can create a fresh one.
		_ = executor.Run("helm", "uninstall", releaseName, "-n", namespace)
		_ = os.WriteFile(adopted, []byte("adopted"), 0o644)
		_ = os.Remove(importing)
		return
	}

	// First attempt: write import marker. Register() will try pulumi.Import().
	_ = os.WriteFile(importing, []byte(fmt.Sprintf("%s/%s", namespace, releaseName)), 0o644)
}

// MarkAdopted writes the adopted marker and removes the import marker after a
// successful Pulumi run. Called from Register() when import succeeds.
func MarkAdopted(releaseName, namespace string) {
	_ = os.WriteFile(adoptedMarkerPath(namespace, releaseName), []byte("adopted"), 0o644)
	_ = os.Remove(importMarkerPath(namespace, releaseName))
}

// NeedsImport returns true if the release should be imported into Pulumi state
// (import marker exists but adopted marker does not).
func NeedsImport(releaseName, namespace string) bool {
	if _, err := os.Stat(adoptedMarkerPath(namespace, releaseName)); err == nil {
		return false
	}
	if _, err := os.Stat(importMarkerPath(namespace, releaseName)); err == nil {
		return true
	}
	return false
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

// DeployHelmRelease creates a Pulumi Helm Release resource. If the release
// needs adoption from legacy bash scripts (import marker exists), it uses
// pulumi.Import() to adopt the existing release in-place. On success the
// adopted marker is written.
func DeployHelmRelease(ctx *pulumi.Context, name string, args HelmReleaseArgs, opts ...pulumi.ResourceOption) (*helmv3.Release, error) {
	if NeedsImport(args.ReleaseName, args.Namespace) {
		importID := fmt.Sprintf("%s/%s", args.Namespace, args.ReleaseName)
		opts = append(opts, pulumi.Import(pulumi.ID(importID)))
	}

	rel, err := helmv3.NewRelease(ctx, name, &helmv3.ReleaseArgs{
		Name:            pulumi.String(args.ReleaseName),
		Namespace:       pulumi.String(args.Namespace),
		Chart:           pulumi.String(args.Chart),
		Version:         pulumi.String(args.Version),
		CreateNamespace: pulumi.Bool(true),
		SkipAwait:       pulumi.BoolPtr(true),
		WaitForJobs:     pulumi.BoolPtr(false),
		// Replace handles the case where a Helm release exists but Pulumi
		// state doesn't know about it (e.g. previous up crashed after Helm
		// install but before state was saved). Without this, Helm rejects
		// the install with "cannot re-use a name that is still in use".
		Replace: pulumi.BoolPtr(true),
		RepositoryOpts: &helmv3.RepositoryOptsArgs{
			Repo: pulumi.String(args.RepoURL),
		},
		Values: pulumi.Map(toStringMap(args.Values)),
	}, opts...)
	if err != nil {
		return nil, err
	}

	// Mark as adopted after successful registration.
	MarkAdopted(args.ReleaseName, args.Namespace)
	return rel, err
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
