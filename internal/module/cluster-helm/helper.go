package clusterhelm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	helmv3 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

var helmMarkerRootDir = "/var/lib/magnum"

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
		if req.Logger != nil {
			req.Logger.Infof("helm migration: evaluating release %s/%s", namespace, releaseName)
		}
		executor := host.NewExecutor(req.Apply, req.Logger)
		AdoptHelmRelease(executor, releaseName, namespace)
	}
	return moduleapi.Result{
		Outputs: map[string]string{"firstMaster": "true"},
	}, nil
}

// adoptedMarkerPath returns the path for the "already adopted" marker file.
func adoptedMarkerPath(namespace, releaseName string) string {
	return filepath.Join(helmMarkerRootDir, fmt.Sprintf("helm-adopted-%s-%s", namespace, releaseName))
}

// importMarkerPath returns the path for the "needs import" marker file.
func importMarkerPath(namespace, releaseName string) string {
	return filepath.Join(helmMarkerRootDir, fmt.Sprintf("helm-import-%s-%s", namespace, releaseName))
}

// forceMarkerPath returns the path for the "needs force update" marker file.
func forceMarkerPath(namespace, releaseName string) string {
	return filepath.Join(helmMarkerRootDir, fmt.Sprintf("helm-force-%s-%s", namespace, releaseName))
}

// managedMarkerPath tracks Helm releases bootstrap intends to manage. The
// marker is written during Run() and only promoted to "adopted" after a
// successful pulumi up, which avoids stale adopted markers when an update fails.
func managedMarkerPath(namespace, releaseName string) string {
	return filepath.Join(helmMarkerRootDir, fmt.Sprintf("helm-managed-%s-%s", namespace, releaseName))
}

// NeedsForceUpdate returns true if a previous failed upgrade wrote a force
// marker for this release.
func NeedsForceUpdate(releaseName, namespace string) bool {
	_, err := os.Stat(forceMarkerPath(namespace, releaseName))
	return err == nil
}

// MarkForceUpdate writes a force-update marker so the next stack.Up() retries
// the Helm release with ForceUpdate enabled.
func MarkForceUpdate(releaseName, namespace string) {
	_ = os.WriteFile(forceMarkerPath(namespace, releaseName), []byte("force"), 0o644)
}

// ClearForceUpdate removes the force-update marker after a successful deploy.
func ClearForceUpdate(releaseName, namespace string) {
	_ = os.Remove(forceMarkerPath(namespace, releaseName))
}

// MarkManaged records that bootstrap intends to manage this release in the
// current run. Successful updates later promote this to an adopted marker.
func MarkManaged(releaseName, namespace string) {
	_ = os.WriteFile(managedMarkerPath(namespace, releaseName), []byte(fmt.Sprintf("%s/%s", namespace, releaseName)), 0o644)
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
	MarkManaged(releaseName, namespace)

	adopted := adoptedMarkerPath(namespace, releaseName)
	importing := importMarkerPath(namespace, releaseName)

	// Already adopted by Pulumi → skip.
	if _, err := os.Stat(adopted); err == nil {
		if executor != nil && executor.Logger != nil {
			executor.Logger.Infof("helm migration: release %s/%s already marked adopted, skipping import preparation", namespace, releaseName)
		}
		return
	}

	// Check if the release exists in Helm.
	_, err := executor.RunCapture("helm", "status", releaseName, "-n", namespace)
	if err != nil {
		// Release doesn't exist yet. Leave only the managed marker so a later
		// successful create can promote it to adopted state.
		if executor != nil && executor.Logger != nil {
			executor.Logger.Infof("helm migration: release %s/%s not found in Helm, will create fresh", namespace, releaseName)
		}
		_ = os.Remove(importing)
		return
	}

	// Release exists. Check if a previous import attempt failed.
	if _, err := os.Stat(importing); err == nil {
		// Import marker exists from a previous run → import already failed once.
		// Fallback: uninstall the release so Pulumi can create a fresh one.
		if executor != nil && executor.Logger != nil {
			executor.Logger.Warnf("helm migration: release %s/%s still has pending import marker, uninstalling legacy release for fresh create", namespace, releaseName)
		}
		_ = executor.Run("helm", "uninstall", releaseName, "-n", namespace)
		_ = os.WriteFile(adopted, []byte("adopted"), 0o644)
		_ = os.Remove(importing)
		return
	}

	// First attempt: write import marker. Register() will try pulumi.Import().
	if executor != nil && executor.Logger != nil {
		executor.Logger.Infof("helm migration: release %s/%s exists in Helm, preparing Pulumi import", namespace, releaseName)
	}
	_ = os.WriteFile(importing, []byte(fmt.Sprintf("%s/%s", namespace, releaseName)), 0o644)
}

// CleanupFailedRelease checks if a Helm release is in "failed" or
// "pending-install" state and uninstalls it so the next Pulumi up can
// recreate it cleanly. This avoids the need for global ForceUpdate which
// would disrupt healthy releases during normal value changes.
func CleanupFailedRelease(executor *host.Executor, releaseName, namespace string) {
	out, err := executor.RunCapture("helm", "status", releaseName, "-n", namespace, "-o", "json")
	if err != nil {
		return // release doesn't exist
	}
	// Quick check for failed/pending states without full JSON parsing.
	if strings.Contains(out, `"status":"failed"`) || strings.Contains(out, `"status":"pending-install"`) {
		_ = executor.Run("helm", "uninstall", releaseName, "-n", namespace)
	}
}

// MarkAdopted writes the adopted marker and removes the import marker after a
// successful Pulumi run.
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

// PendingImportReleases lists releases whose Run() phase detected a legacy Helm
// release and wrote an import marker, but Register() has not yet marked them as
// successfully adopted.
func PendingImportReleases() []HelmReleasePair {
	markerPaths, err := filepath.Glob(filepath.Join(helmMarkerRootDir, "helm-import-*"))
	if err != nil {
		return nil
	}

	var releases []HelmReleasePair
	seen := make(map[string]bool, len(markerPaths))
	for _, markerPath := range markerPaths {
		content, err := os.ReadFile(markerPath)
		if err != nil {
			continue
		}
		pair, ok := parseHelmReleasePair(strings.TrimSpace(string(content)))
		if !ok {
			continue
		}
		key := pair.Namespace + "/" + pair.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		releases = append(releases, pair)
	}
	return releases
}

func parseHelmReleasePair(value string) (HelmReleasePair, bool) {
	namespace, name, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || namespace == "" || name == "" {
		return HelmReleasePair{}, false
	}
	return HelmReleasePair{Namespace: namespace, Name: name}, true
}

// CleanupPendingImportReleases uninstalls any legacy Helm releases that still
// have import markers and marks them as adopted so the retry can create them
// fresh under Pulumi management.
func CleanupPendingImportReleases(executor *host.Executor) []HelmReleasePair {
	pending := PendingImportReleases()
	var cleaned []HelmReleasePair
	for _, rel := range pending {
		if executor != nil && executor.Logger != nil {
			executor.Logger.Warnf("helm adoption fallback: uninstalling legacy release %s/%s after import conflict", rel.Namespace, rel.Name)
		}
		_ = executor.Run("helm", "uninstall", rel.Name, "-n", rel.Namespace)
		MarkAdopted(rel.Name, rel.Namespace)
		cleaned = append(cleaned, rel)
	}
	return cleaned
}

// PrepareManagedImports re-arms import markers for managed releases that still
// exist in Helm. This repairs stale adopted markers left behind by older
// bootstrap versions or failed migration attempts so the next pulumi up retries
// import before escalating to uninstall/recreate.
func PrepareManagedImports(executor *host.Executor) []HelmReleasePair {
	managed := ManagedReleases()
	var prepared []HelmReleasePair
	for _, rel := range managed {
		if executor == nil {
			continue
		}
		if _, err := executor.RunCapture("helm", "status", rel.Name, "-n", rel.Namespace); err != nil {
			continue
		}
		if NeedsImport(rel.Name, rel.Namespace) {
			continue
		}
		if executor.Logger != nil {
			executor.Logger.Warnf("helm adoption repair: preparing import marker for existing release %s/%s", rel.Namespace, rel.Name)
		}
		_ = os.Remove(adoptedMarkerPath(rel.Namespace, rel.Name))
		_ = os.WriteFile(importMarkerPath(rel.Namespace, rel.Name), []byte(rel.Namespace+"/"+rel.Name), 0o644)
		prepared = append(prepared, rel)
	}
	return prepared
}

// ManagedReleases returns releases that bootstrap attempted to manage.
func ManagedReleases() []HelmReleasePair {
	markerPaths, err := filepath.Glob(filepath.Join(helmMarkerRootDir, "helm-managed-*"))
	if err != nil {
		return nil
	}

	var releases []HelmReleasePair
	seen := make(map[string]bool, len(markerPaths))
	for _, markerPath := range markerPaths {
		content, err := os.ReadFile(markerPath)
		if err != nil {
			continue
		}
		pair, ok := parseHelmReleasePair(strings.TrimSpace(string(content)))
		if !ok {
			continue
		}
		key := pair.Namespace + "/" + pair.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		releases = append(releases, pair)
	}
	return releases
}

// PromoteManagedReleases marks all currently managed releases as adopted after
// a successful pulumi up.
func PromoteManagedReleases() {
	for _, rel := range ManagedReleases() {
		if _, err := os.Stat(importMarkerPath(rel.Namespace, rel.Name)); err == nil {
			// Import marker still present means this release still needs a later
			// repair or recreate path; don't promote it yet.
			continue
		}
		MarkAdopted(rel.Name, rel.Namespace)
	}
}

// ClearAllForceUpdateMarkers removes all force-update markers after a
// successful update.
func ClearAllForceUpdateMarkers() {
	markerPaths, err := filepath.Glob(filepath.Join(helmMarkerRootDir, "helm-force-*"))
	if err != nil {
		return
	}
	for _, markerPath := range markerPaths {
		_ = os.Remove(markerPath)
	}
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
	// ForceUpdate tells Helm to replace resources instead of patching during
	// upgrades. Required for charts whose templates change enough across
	// versions that strategic merge patches produce invalid K8s objects
	// (e.g. CoreDNS Service missing spec.ports after a major chart bump).
	ForceUpdate bool
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

	releaseArgs := &helmv3.ReleaseArgs{
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
	}
	// ForceUpdate is enabled either explicitly by the module or automatically
	// when a previous stack.Up() failed with a Helm patch error and wrote a
	// force marker. This lets the first attempt use normal patching and only
	// escalates to replace-on-upgrade for the retry.
	if args.ForceUpdate || NeedsForceUpdate(args.ReleaseName, args.Namespace) {
		releaseArgs.ForceUpdate = pulumi.BoolPtr(true)
	}

	rel, err := helmv3.NewRelease(ctx, name, releaseArgs, opts...)
	if err != nil {
		return nil, err
	}
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

// HelmReleasePair is a namespace/name pair identifying a Helm release.
type HelmReleasePair struct {
	Namespace string
	Name      string
}

// helmReleaseRe matches Helm release identifiers in Pulumi error output.
// Patterns: `Helm release "NAMESPACE/NAME"` or `Helm Release NAMESPACE/NAME:`
var helmReleaseRe = regexp.MustCompile(`Helm [Rr]elease "?([a-z0-9-]+)/([a-z0-9-]+)"?`)

// ParseHelmPatchFailures extracts Helm releases that failed due to patch
// errors (e.g. "cannot patch", "is invalid") from a Pulumi error message.
// Returns nil if the error is not a Helm patch failure.
func ParseHelmPatchFailures(errMsg string) []HelmReleasePair {
	if !strings.Contains(errMsg, "cannot patch") && !strings.Contains(errMsg, "is invalid") {
		return nil
	}
	matches := helmReleaseRe.FindAllStringSubmatch(errMsg, -1)
	seen := make(map[string]bool)
	var releases []HelmReleasePair
	for _, m := range matches {
		key := m[1] + "/" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		releases = append(releases, HelmReleasePair{Namespace: m[1], Name: m[2]})
	}
	return releases
}

// HasHelmNameReuseConflict reports whether a Helm release create failed because
// the release name already exists outside Pulumi state.
func HasHelmNameReuseConflict(errMsg string) bool {
	return strings.Contains(errMsg, "cannot re-use a name that is still in use")
}
