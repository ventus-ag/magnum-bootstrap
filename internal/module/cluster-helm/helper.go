package clusterhelm

import (
	"context"
	"encoding/json"
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

// HelmOwnershipConflict describes a live Kubernetes object that blocks a Helm
// release update because it exists without the expected Helm ownership labels
// and annotations.
type HelmOwnershipConflict struct {
	ReleaseNamespace  string
	ReleaseName       string
	ResourceKind      string
	ResourceNamespace string
	ResourceName      string
}

// helmReleaseRe matches Helm release identifiers in Pulumi error output.
// Patterns: `Helm release "NAMESPACE/NAME"` or `Helm Release NAMESPACE/NAME:`
var helmReleaseRe = regexp.MustCompile(`Helm [Rr]elease "?([a-z0-9-]+)/([a-z0-9-]+)"?`)

// Helm reports two ownership-conflict phrasings, and only the MISMATCHED keys
// appear — so a single conflict may carry release-name only, release-namespace
// only, or both:
//
//	missing metadata (resource was never Helm-managed):
//	  ... missing key "meta.helm.sh/release-name": must be set to "X"
//	wrong owner (resource adopted by a DIFFERENT release):
//	  ... key "meta.helm.sh/release-name" must equal "X": current value is "Z"
//
// helmOwnershipHeaderRe finds each conflicting resource header; the two
// want-regexes extract the REQUIRED release-name/namespace from that resource's
// block, tolerating either phrasing and either field being absent. The original
// single regex only matched the "must be set to" phrasing AND required both
// fields, so the common cross-release case (e.g. a metrics-server ClusterRole
// still annotated release-name=coredns, namespace already correct) parsed to
// nothing and the auto-repair never fired.
var helmOwnershipHeaderRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9]+) "([^"]+)"(?: in namespace "([^"]*)")? exists and cannot be imported into the current release: invalid ownership metadata`)
var helmReleaseNameWantRe = regexp.MustCompile(`meta\.helm\.sh/release-name":?\s*(?:must be set to|must equal)\s*"([^"]+)"`)
var helmReleaseNamespaceWantRe = regexp.MustCompile(`meta\.helm\.sh/release-namespace":?\s*(?:must be set to|must equal)\s*"([^"]+)"`)

// ParseHelmPatchFailures extracts Helm releases that failed due to patch
// errors (e.g. "cannot patch", "is invalid") from a Pulumi error message.
// Returns nil if the error is not a Helm patch failure.
func ParseHelmPatchFailures(errMsg string) []HelmReleasePair {
	if !strings.Contains(errMsg, "cannot patch") && !strings.Contains(errMsg, "is invalid") {
		return nil
	}
	// Only force-mark releases named on a LINE that carries the failure
	// phrase. Matching the whole message marked every release mentioned
	// anywhere in a multi-error report, and those force markers persist until
	// the next fully-green up — unrelated releases then get replace-on-upgrade
	// churn for someone else's patch failure.
	seen := make(map[string]bool)
	var releases []HelmReleasePair
	for _, line := range strings.Split(errMsg, "\n") {
		if !strings.Contains(line, "cannot patch") && !strings.Contains(line, "is invalid") {
			continue
		}
		for _, m := range helmReleaseRe.FindAllStringSubmatch(line, -1) {
			key := m[1] + "/" + m[2]
			if seen[key] {
				continue
			}
			seen[key] = true
			releases = append(releases, HelmReleasePair{Namespace: m[1], Name: m[2]})
		}
	}
	return releases
}

// HasHelmNameReuseConflict reports whether a Helm release create failed because
// the release name already exists outside Pulumi state.
func HasHelmNameReuseConflict(errMsg string) bool {
	return strings.Contains(errMsg, "cannot re-use a name that is still in use")
}

// helmNoDeployedReleasesRe matches the release name in a Helm "has no deployed
// releases" upgrade error, e.g. `"coredns" has no deployed releases`.
var helmNoDeployedReleasesRe = regexp.MustCompile(`"([a-z0-9-]+)" has no deployed releases`)

// ParseHelmNoDeployedReleases extracts release names from "has no deployed
// releases" errors. Helm reports only the name (no namespace) for this error;
// callers map names back to namespaces via the managed-release markers.
//
// This error means the release is referenced in Pulumi state but Helm has no
// deployed revision for it — typically because an earlier failure-cleanup
// uninstalled the release while Pulumi state still tracked it. `helm upgrade`
// then fails on every subsequent run (a permanent wedge) until Pulumi state is
// refreshed so the stale resource is dropped and recreated fresh.
func ParseHelmNoDeployedReleases(errMsg string) []string {
	matches := helmNoDeployedReleasesRe.FindAllStringSubmatch(errMsg, -1)
	seen := make(map[string]bool, len(matches))
	var names []string
	for _, m := range matches {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		names = append(names, m[1])
	}
	return names
}

// ManagedReleaseByName returns the managed release pair matching the given
// release name, if bootstrap recorded one. Used to recover the namespace for
// Helm errors that report only the release name.
func ManagedReleaseByName(name string) (HelmReleasePair, bool) {
	for _, rel := range ManagedReleases() {
		if rel.Name == name {
			return rel, true
		}
	}
	return HelmReleasePair{}, false
}

// ResetDesyncedRelease prepares a release that Pulumi state references but Helm
// no longer has deployed for clean recreation: it clears the force-update marker
// (force does a `helm upgrade`, which cannot recover a non-deployed release) and
// purges any orphaned Helm storage so a post-refresh `pulumi up` recreates the
// release with a fresh install.
func ResetDesyncedRelease(executor *host.Executor, name, namespace string) {
	ClearForceUpdate(name, namespace)
	if executor != nil {
		_ = executor.Run("helm", "uninstall", name, "-n", namespace)
	}
}

// helmRemovedAPIRe confirms a Helm upgrade that failed because the DEPLOYED
// release manifest references an apiVersion the cluster no longer serves. This
// happens when a cluster is upgraded across a Kubernetes version that REMOVED an
// API the installed chart used — most commonly policy/v1beta1 PodDisruptionBudget
// (removed in 1.25). `helm upgrade` must build the existing manifest to diff it,
// can't map the removed kind, and aborts:
//
//	Helm Release kube-system/openstack-autoscaler: unable to build kubernetes
//	objects from current release manifest: resource mapping not found for name:
//	"openstack-autoscaler-manager" namespace: "" from "": no matches for kind
//	"PodDisruptionBudget" in version "policy/v1beta1"
//
// The fix is not a patch retry (the stored manifest itself is unbuildable) — the
// release must be uninstalled and recreated from the new chart, which renders the
// current apiVersion.
// helmRemovedAPIRe captures the release pair bound directly to the unbuildable-
// manifest phrase ("Helm Release ns/name: unable to build kubernetes objects from
// current release manifest"), so only the offending release is recreated — not
// every release that happens to be named elsewhere in the same error.
var helmRemovedAPIRe = regexp.MustCompile(`Helm [Rr]elease "?([a-z0-9-]+)/([a-z0-9-]+)"?:\s*unable to build kubernetes objects from current release manifest`)

// ParseHelmRemovedAPIFailures extracts releases that failed to upgrade because
// their deployed manifest references a removed/unserved apiVersion. Returns nil
// if the error is not of that shape.
func ParseHelmRemovedAPIFailures(errMsg string) []HelmReleasePair {
	// Guard: the unbuildable manifest must be due to an unmappable kind/version
	// (a removed API), not some other build failure.
	if !strings.Contains(errMsg, "resource mapping not found") && !strings.Contains(errMsg, "no matches for kind") {
		return nil
	}
	matches := helmRemovedAPIRe.FindAllStringSubmatch(errMsg, -1)
	seen := make(map[string]bool, len(matches))
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

// ResetRemovedAPIRelease drops a release whose stored manifest references a
// removed apiVersion so a post-refresh `pulumi up` reinstalls it fresh from the
// current chart. It clears the force marker (force is a `helm upgrade`, which
// hits the same unbuildable-manifest error) and uninstalls the release; if
// `helm uninstall` itself cannot build the manifest, it falls back to deleting
// the Helm release-storage Secrets directly so the next install starts clean.
func ResetRemovedAPIRelease(executor *host.Executor, name, namespace string) {
	ClearForceUpdate(name, namespace)
	if executor == nil {
		return
	}
	if err := executor.Run("helm", "uninstall", name, "-n", namespace, "--no-hooks"); err != nil {
		// Helm release storage is a Secret labelled owner=helm,name=<release>.
		_ = executor.Run("kubectl", "delete", "secret", "-n", namespace,
			"-l", "owner=helm,name="+name, "--ignore-not-found=true")
	}
}

// WarnIfClampedBelow logs when an addon's version map has no entry for this
// (older) Kubernetes version, so the lowest — newer-Kubernetes — entry gets
// deployed. The pairing usually boots (the e2e ladder creates at v1.20 with
// 1.24-era charts) but is upstream-untested; deploying it silently hides the
// risk from the operator.
func WarnIfClampedBelow(ctx *pulumi.Context, addon string, versionMap map[string]string, kubeVersion string) {
	if _, clamped := config.LookupByKubeVersionClamped(versionMap, kubeVersion); clamped {
		_ = ctx.Log.Warn(fmt.Sprintf(
			"%s: no chart/image entry for Kubernetes %s; deploying the lowest available entry, which targets a newer Kubernetes — untested pairing",
			addon, kubeVersion), nil)
	}
}

// helmPendingOperationPhrase is Helm's error when the release's newest
// storage record is stuck in a pending-* status — left behind when a previous
// helm install/upgrade/rollback was killed mid-flight (node reboot, Heat
// timeout, OOM). Every subsequent upgrade fails with this exact error until
// the pending record is cleared, so it is a permanent wedge without recovery.
const helmPendingOperationPhrase = "another operation (install/upgrade/rollback) is in progress"

var helmPendingOperationRe = regexp.MustCompile(`Helm [Rr]elease "?([a-z0-9-]+)/([a-z0-9-]+)"?:[^\n]*another operation \(install/upgrade/rollback\) is in progress`)

// ParseHelmPendingOperations reports whether errMsg is a pending-operation
// wedge and extracts the affected releases when the error names them. The
// bool is true whenever the phrase is present, even if no release pair could
// be parsed — callers then fall back to scanning managed releases.
func ParseHelmPendingOperations(errMsg string) ([]HelmReleasePair, bool) {
	if !strings.Contains(errMsg, helmPendingOperationPhrase) {
		return nil, false
	}
	matches := helmPendingOperationRe.FindAllStringSubmatch(errMsg, -1)
	seen := make(map[string]bool, len(matches))
	var releases []HelmReleasePair
	for _, m := range matches {
		key := m[1] + "/" + m[2]
		if seen[key] {
			continue
		}
		seen[key] = true
		releases = append(releases, HelmReleasePair{Namespace: m[1], Name: m[2]})
	}
	return releases, true
}

// PendingOperationManagedReleases scans the managed releases for any whose
// newest Helm history entry is stuck pending-*. Fallback for pending-operation
// errors that do not name the release.
func PendingOperationManagedReleases(executor *host.Executor) []HelmReleasePair {
	if executor == nil {
		return nil
	}
	var stuck []HelmReleasePair
	for _, rel := range ManagedReleases() {
		for _, e := range helmHistory(executor, rel.Name, rel.Namespace) {
			if strings.HasPrefix(e.Status, "pending-") {
				stuck = append(stuck, rel)
				break
			}
		}
	}
	return stuck
}

type helmHistoryEntry struct {
	Revision int    `json:"revision"`
	Status   string `json:"status"`
}

func helmHistory(executor *host.Executor, name, namespace string) []helmHistoryEntry {
	out, err := executor.RunCapture("helm", "history", name, "-n", namespace, "-o", "json")
	if err != nil {
		return nil
	}
	var entries []helmHistoryEntry
	if json.Unmarshal([]byte(out), &entries) != nil {
		return nil
	}
	return entries
}

// ResetPendingOperationRelease clears a release wedged in pending-* by
// deleting only the pending revisions' storage Secrets
// (sh.helm.release.v1.<name>.v<revision>). The last *deployed* revision (if
// any) becomes current again, so the live workload is untouched — unlike
// `helm uninstall`, which would take the addon (e.g. coredns) down. With no
// deployed revision left, the next pulumi up installs fresh. Returns true if
// at least one pending record was removed.
func ResetPendingOperationRelease(executor *host.Executor, name, namespace string) bool {
	if executor == nil {
		return false
	}
	cleared := false
	for _, e := range helmHistory(executor, name, namespace) {
		if !strings.HasPrefix(e.Status, "pending-") {
			continue
		}
		secret := fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, e.Revision)
		if executor.Run("kubectl", "delete", "secret", "-n", namespace, secret, "--ignore-not-found=true") == nil {
			cleared = true
		}
	}
	return cleared
}

// ParseHelmOwnershipConflicts extracts resources that block a Helm release
// update because they exist without the expected Helm ownership metadata.
func ParseHelmOwnershipConflicts(errMsg string) []HelmOwnershipConflict {
	if !strings.Contains(errMsg, "exists and cannot be imported into the current release") || !strings.Contains(errMsg, "invalid ownership metadata") {
		return nil
	}
	headers := helmOwnershipHeaderRe.FindAllStringSubmatchIndex(errMsg, -1)
	seen := make(map[string]bool, len(headers))
	conflicts := make([]HelmOwnershipConflict, 0, len(headers))
	for i, h := range headers {
		kind := errMsg[h[2]:h[3]]
		resName := errMsg[h[4]:h[5]]
		resNs := ""
		if h[6] >= 0 { // optional "in namespace" group matched
			resNs = errMsg[h[6]:h[7]]
		}
		// The block for this resource runs from the end of its header to the
		// start of the next header (or end of string). Only this block's
		// validation errors describe this resource's required ownership.
		blockEnd := len(errMsg)
		if i+1 < len(headers) {
			blockEnd = headers[i+1][0]
		}
		block := errMsg[h[1]:blockEnd]

		relName := firstSubmatch(helmReleaseNameWantRe, block)
		relNs := firstSubmatch(helmReleaseNamespaceWantRe, block)
		if relName == "" && relNs == "" {
			continue // nothing actionable for this resource
		}
		conflict := HelmOwnershipConflict{
			ResourceKind:      kind,
			ResourceName:      resName,
			ResourceNamespace: strings.TrimSpace(resNs),
			ReleaseName:       relName,
			ReleaseNamespace:  relNs,
		}
		key := strings.Join([]string{conflict.ReleaseNamespace, conflict.ReleaseName, conflict.ResourceKind, conflict.ResourceNamespace, conflict.ResourceName}, "/")
		if seen[key] {
			continue
		}
		seen[key] = true
		conflicts = append(conflicts, conflict)
	}
	return conflicts
}

func firstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// RepairHelmOwnershipConflicts patches the expected Helm labels/annotations
// onto the conflicting live resources so Helm can adopt them on retry.
func RepairHelmOwnershipConflicts(executor *host.Executor, conflicts []HelmOwnershipConflict) []HelmOwnershipConflict {
	if executor == nil {
		return nil
	}
	var repaired []HelmOwnershipConflict
	for _, conflict := range conflicts {
		resourceRef := strings.ToLower(conflict.ResourceKind) + "/" + conflict.ResourceName
		nsFlag := []string{}
		if conflict.ResourceNamespace != "" {
			nsFlag = []string{"-n", conflict.ResourceNamespace}
		}
		labelArgs := append([]string{"label"}, nsFlag...)
		labelArgs = append(labelArgs, resourceRef, "app.kubernetes.io/managed-by=Helm", "--overwrite")

		// Patch ONLY the annotations the conflict reported as wrong. In the
		// cross-release case Helm flags release-name only (the namespace already
		// matches); setting release-namespace="" here would clobber a correct
		// value and break adoption.
		annotateArgs := append([]string{"annotate"}, nsFlag...)
		annotateArgs = append(annotateArgs, resourceRef)
		annotated := false
		if conflict.ReleaseName != "" {
			annotateArgs = append(annotateArgs, "meta.helm.sh/release-name="+conflict.ReleaseName)
			annotated = true
		}
		if conflict.ReleaseNamespace != "" {
			annotateArgs = append(annotateArgs, "meta.helm.sh/release-namespace="+conflict.ReleaseNamespace)
			annotated = true
		}
		if !annotated {
			continue
		}
		annotateArgs = append(annotateArgs, "--overwrite")
		if executor.Logger != nil {
			executor.Logger.Warnf("helm ownership repair: patching %s %s for release %s/%s", conflict.ResourceKind, resourceRef, conflict.ReleaseNamespace, conflict.ReleaseName)
		}
		if err := executor.Run("kubectl", labelArgs...); err != nil {
			if executor.Logger != nil {
				executor.Logger.Warnf("helm ownership repair: failed labeling %s: %v", resourceRef, err)
			}
			continue
		}
		if err := executor.Run("kubectl", annotateArgs...); err != nil {
			if executor.Logger != nil {
				executor.Logger.Warnf("helm ownership repair: failed annotating %s: %v", resourceRef, err)
			}
			continue
		}
		repaired = append(repaired, conflict)
	}
	return repaired
}

// DeleteHelmOwnershipConflicts removes the exact live resources that still
// block a Helm release update after ownership repair was attempted.
func DeleteHelmOwnershipConflicts(executor *host.Executor, conflicts []HelmOwnershipConflict) []HelmOwnershipConflict {
	if executor == nil {
		return nil
	}
	var deleted []HelmOwnershipConflict
	for _, conflict := range conflicts {
		resourceRef := strings.ToLower(conflict.ResourceKind) + "/" + conflict.ResourceName
		deleteArgs := []string{"delete"}
		if conflict.ResourceNamespace != "" {
			deleteArgs = append(deleteArgs, "-n", conflict.ResourceNamespace)
		}
		deleteArgs = append(deleteArgs, resourceRef, "--ignore-not-found=true")
		if executor.Logger != nil {
			executor.Logger.Warnf("helm ownership fallback: deleting %s %s for release %s/%s", conflict.ResourceKind, resourceRef, conflict.ReleaseNamespace, conflict.ReleaseName)
		}
		if err := executor.Run("kubectl", deleteArgs...); err != nil {
			if executor.Logger != nil {
				executor.Logger.Warnf("helm ownership fallback: failed deleting %s: %v", resourceRef, err)
			}
			continue
		}
		deleted = append(deleted, conflict)
	}
	return deleted
}
