package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clusters"
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/clustertemplates"
)

// runner holds the authenticated Magnum service client and run configuration.
type runner struct {
	cfg      config
	provider *gophercloud.ProviderClient
	magnum   *gophercloud.ServiceClient
	nova     *gophercloud.ServiceClient // compute client, built lazily
	swift    *gophercloud.ServiceClient // object-store client, built lazily
}

// computeClient returns a lazily-built, cached Nova (compute v2) client.
func (r *runner) computeClient() (*gophercloud.ServiceClient, error) {
	if r.nova != nil {
		return r.nova, nil
	}
	c, err := openstack.NewComputeV2(r.provider, gophercloud.EndpointOpts{Region: r.cfg.region})
	if err != nil {
		return nil, fmt.Errorf("locating Nova (compute) endpoint: %w", err)
	}
	r.nova = c
	return c, nil
}

// ephemeralKeypairName is the deterministic name of the keypair auto-created
// for a run when KEYPAIR is unset — deterministic so a separate -teardown can
// also clean it up.
func (r *runner) ephemeralKeypairName() string { return r.cfg.clusterName + "-kp" }

// ensureKeypair returns the keypair to attach to the cluster. If KEYPAIR was
// set it is used as-is; otherwise an ephemeral keypair is created (Nova
// generates it — we discard the returned private key since the e2e never SSHes
// to nodes, it talks to the apiserver) and is torn down with the cluster.
func (r *runner) ensureKeypair(ctx context.Context) (string, error) {
	if r.cfg.keypair != "" {
		return r.cfg.keypair, nil
	}
	nova, err := r.computeClient()
	if err != nil {
		return "", err
	}
	name := r.ephemeralKeypairName()
	r.log("no KEYPAIR set — creating ephemeral keypair %q", name)
	if _, err := keypairs.Create(ctx, nova, keypairs.CreateOpts{Name: name}).Extract(); err != nil {
		return "", fmt.Errorf("create ephemeral keypair: %w", err)
	}
	return name, nil
}

func (r *runner) log(format string, a ...any) {
	fmt.Printf("\033[1;34m[magnum %s]\033[0m %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func (r *runner) err(format string, a ...any) {
	fmt.Printf("\033[1;31m[magnum %s] ERROR:\033[0m %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildAuthOptions reads the full standard OpenStack RC environment the way the
// `openstack` CLI / clouds.yaml do — honouring OS_USER_DOMAIN_NAME/ID and
// OS_PROJECT_DOMAIN_NAME/ID separately. gophercloud's own AuthOptionsFromEnv
// only reads OS_DOMAIN_NAME/ID (no user/project split), so it rejects most real
// RC files; this mirrors clientconfig semantics instead.
func buildAuthOptions() (gophercloud.AuthOptions, error) {
	ao := gophercloud.AuthOptions{
		IdentityEndpoint: os.Getenv("OS_AUTH_URL"),
		AllowReauth:      true,
	}
	if ao.IdentityEndpoint == "" {
		return ao, fmt.Errorf("OS_AUTH_URL is required")
	}

	// Application credential — self-contained (carries its own user + scope).
	if id := os.Getenv("OS_APPLICATION_CREDENTIAL_ID"); id != "" {
		ao.ApplicationCredentialID = id
		ao.ApplicationCredentialSecret = os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET")
		if ao.ApplicationCredentialSecret == "" {
			return ao, fmt.Errorf("OS_APPLICATION_CREDENTIAL_SECRET is required with OS_APPLICATION_CREDENTIAL_ID")
		}
		return ao, nil
	}

	// Username/password (or TOTP passcode).
	ao.Username = os.Getenv("OS_USERNAME")
	ao.UserID = os.Getenv("OS_USERID")
	ao.Password = os.Getenv("OS_PASSWORD")
	ao.Passcode = os.Getenv("OS_PASSCODE")
	// The domain the USER belongs to (required when authenticating by name).
	ao.DomainName = os.Getenv("OS_USER_DOMAIN_NAME")
	ao.DomainID = os.Getenv("OS_USER_DOMAIN_ID")

	// Project scope. gophercloud requires ProjectID to be supplied ALONE (a
	// project UUID is globally unique, so a project-domain alongside it is both
	// redundant and rejected); a ProjectName, by contrast, needs its domain to
	// disambiguate. So prefer ID-only, and fall back to name+domain.
	if projectID := firstNonEmpty(os.Getenv("OS_PROJECT_ID"), os.Getenv("OS_TENANT_ID")); projectID != "" {
		ao.Scope = &gophercloud.AuthScope{ProjectID: projectID}
	} else if projectName := firstNonEmpty(os.Getenv("OS_PROJECT_NAME"), os.Getenv("OS_TENANT_NAME")); projectName != "" {
		ao.Scope = &gophercloud.AuthScope{
			ProjectName: projectName,
			DomainID:    os.Getenv("OS_PROJECT_DOMAIN_ID"),
			DomainName:  os.Getenv("OS_PROJECT_DOMAIN_NAME"),
		}
	}

	if ao.Username == "" && ao.UserID == "" {
		return ao, fmt.Errorf("set OS_USERNAME (or OS_USERID), or an application credential (OS_APPLICATION_CREDENTIAL_ID/SECRET)")
	}
	if ao.Password == "" && ao.Passcode == "" {
		return ao, fmt.Errorf("OS_PASSWORD is required for username auth")
	}
	if ao.Username != "" && ao.DomainName == "" && ao.DomainID == "" {
		return ao, fmt.Errorf("OS_USER_DOMAIN_NAME (or OS_USER_DOMAIN_ID) is required when authenticating by OS_USERNAME")
	}
	return ao, nil
}

// newRunner authenticates against OpenStack using standard OS_* environment
// variables (application credential or user/password) and resolves the Magnum
// (Container Infra v1) service endpoint.
func newRunner(ctx context.Context, cfg config) (*runner, error) {
	ao, err := buildAuthOptions()
	if err != nil {
		return nil, fmt.Errorf("reading OS_* auth env: %w", err)
	}
	provider, err := openstack.AuthenticatedClient(ctx, ao)
	if err != nil {
		return nil, fmt.Errorf("keystone authenticate: %w", err)
	}
	eo := gophercloud.EndpointOpts{Region: cfg.region}
	magnum, err := openstack.NewContainerInfraV1(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("locating Magnum (container-infra) endpoint: %w", err)
	}
	return &runner{cfg: cfg, provider: provider, magnum: magnum}, nil
}

// listResources prints the cluster templates and nova keypairs visible to the
// authenticated project — used to discover the exact CLUSTER_TEMPLATE/KEYPAIR
// values for CI config. It also proves auth works against both Magnum and Nova.
func (r *runner) listResources(ctx context.Context) error {
	pages, err := clustertemplates.List(r.magnum, nil).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list cluster templates: %w", err)
	}
	tmpls, err := clustertemplates.ExtractClusterTemplates(pages)
	if err != nil {
		return fmt.Errorf("extract cluster templates: %w", err)
	}
	r.log("cluster templates (%d):", len(tmpls))
	for _, t := range tmpls {
		recon := "reconciler:-"
		if t.Labels["reconciler_version"] != "" || t.Labels["reconciler_binary_url"] != "" {
			recon = fmt.Sprintf("reconciler:ver=%q url=%q", t.Labels["reconciler_version"], t.Labels["reconciler_binary_url"])
		}
		r.log("  %-32s coe=%-10s kube_tag=%-10s %s", t.Name, t.COE, t.Labels["kube_tag"], recon)
		r.log("      uuid=%s", t.UUID)
	}

	compute, err := r.computeClient()
	if err != nil {
		return err
	}
	kpPages, err := keypairs.List(compute, nil).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list keypairs: %w", err)
	}
	kps, err := keypairs.ExtractKeyPairs(kpPages)
	if err != nil {
		return fmt.Errorf("extract keypairs: %w", err)
	}
	r.log("keypairs (%d):", len(kps))
	for _, k := range kps {
		r.log("  %-32s %s", k.Name, k.Fingerprint)
	}

	// Probe the service catalog for an object store (Swift) — tells us whether
	// the in-cloud binary-staging path is available on this cloud.
	if ep, err := r.provider.EndpointLocator(gophercloud.EndpointOpts{
		Type:         "object-store",
		Region:       r.cfg.region,
		Availability: gophercloud.AvailabilityPublic,
	}); err == nil {
		r.log("object-store (Swift): present @ %s", ep)
	} else {
		r.log("object-store (Swift): NOT in catalog (%v)", err)
	}
	return nil
}

// run executes the full lifecycle. Each step returns an error that aborts the
// run; main() handles teardown.
func (r *runner) run(ctx context.Context) error {
	if err := r.preflight(ctx); err != nil {
		return err
	}
	// Stage the freshly-built reconciler binary into Swift (if -bootstrap-binary
	// is set) so the nodes fetch this exact build; sets reconciler_binary_url.
	if err := r.stageBootstrap(ctx); err != nil {
		return err
	}
	uuid, err := r.createCluster(ctx)
	if err != nil {
		return err
	}
	r.log("cluster uuid=%s", uuid)

	if err := r.smokeCore(ctx); err != nil {
		return err
	}
	if err := r.smokeCloudIntegration(ctx); err != nil {
		return err
	}
	if r.cfg.skipUpgrade {
		r.log("SKIP_UPGRADE set — skipping upgrade step")
	} else if err := r.upgradeCluster(ctx); err != nil {
		return err
	}
	if r.cfg.skipResize {
		r.log("SKIP_RESIZE set — skipping resize step")
	} else if err := r.resizeCluster(ctx); err != nil {
		return err
	}
	if r.cfg.skipCARotate {
		r.log("SKIP_CA_ROTATE set — skipping ca-rotate step")
	} else if err := r.caRotateCluster(ctx); err != nil {
		return err
	}
	return nil
}

// preflight verifies auth works and that the configured template + keypair are
// present before any billed resource is created.
func (r *runner) preflight(ctx context.Context) error {
	if r.cfg.template == "" {
		return fmt.Errorf("CLUSTER_TEMPLATE (-template) is required")
	}
	// KEYPAIR is optional — an ephemeral keypair is auto-created when unset.
	ct, err := clustertemplates.Get(ctx, r.magnum, r.cfg.template).Extract()
	if err != nil {
		return fmt.Errorf("cluster template %q: %w", r.cfg.template, err)
	}
	r.log("auth OK; template %q resolved to %s (coe=%s, network_driver=%s)", r.cfg.template, ct.UUID, ct.COE, ct.NetworkDriver)
	// Show the template's default labels — these are inherited at create, and
	// our overrides (reconciler_*, kube_tag) are merged on top (merge_labels).
	r.log("template default labels (%d) — inherited, then merged with overrides:", len(ct.Labels))
	for k, v := range ct.Labels {
		r.log("  %-28s = %s", k, v)
	}
	if ct.Labels["reconciler_version"] == "" && ct.Labels["reconciler_binary_url"] == "" {
		r.log("note: template carries no reconciler_* labels — they must come from staging (-bootstrap-binary) or RECONCILER_VERSION/URL")
	}
	if r.cfg.upgradeTemplate != r.cfg.template {
		if _, err := r.resolveTemplateID(ctx, r.cfg.upgradeTemplate); err != nil {
			return fmt.Errorf("upgrade template %q: %w", r.cfg.upgradeTemplate, err)
		}
	}
	return r.preflightKeypair(ctx)
}

// preflightKeypair proves the keypair side works before a long run: when
// KEYPAIR is supplied, confirm it exists; otherwise do a real create→delete
// round-trip of a throwaway key, which also verifies we hold the Nova write
// permission the ephemeral-keypair path needs.
func (r *runner) preflightKeypair(ctx context.Context) error {
	nova, err := r.computeClient()
	if err != nil {
		return err
	}
	if r.cfg.keypair != "" {
		if _, err := keypairs.Get(ctx, nova, r.cfg.keypair, nil).Extract(); err != nil {
			return fmt.Errorf("keypair %q not found: %w", r.cfg.keypair, err)
		}
		r.log("keypair %q present", r.cfg.keypair)
		return nil
	}
	name := r.cfg.clusterName + "-preflight-kp"
	r.log("keypair round-trip: creating throwaway %q", name)
	if _, err := keypairs.Create(ctx, nova, keypairs.CreateOpts{Name: name}).Extract(); err != nil {
		return fmt.Errorf("create test keypair (Nova write perm?): %w", err)
	}
	if err := keypairs.Delete(ctx, nova, name, nil).ExtractErr(); err != nil {
		return fmt.Errorf("delete test keypair %q (leaked!): %w", name, err)
	}
	r.log("keypair round-trip OK — created + deleted %q (ephemeral-keypair path works)", name)
	return nil
}

// resolveTemplateID accepts a template name or UUID and returns its UUID.
// Magnum's GET /clustertemplates/{ident} resolves either, so a single Get
// suffices; List is the fallback for ambiguous names.
func (r *runner) resolveTemplateID(ctx context.Context, ident string) (string, error) {
	ct, err := clustertemplates.Get(ctx, r.magnum, ident).Extract()
	if err == nil {
		return ct.UUID, nil
	}
	// Fallback: list and match by exact name.
	pages, lerr := clustertemplates.List(r.magnum, nil).AllPages(ctx)
	if lerr != nil {
		return "", err // surface the original Get error
	}
	all, eerr := clustertemplates.ExtractClusterTemplates(pages)
	if eerr != nil {
		return "", err
	}
	for _, t := range all {
		if t.Name == ident || t.UUID == ident {
			return t.UUID, nil
		}
	}
	return "", fmt.Errorf("not found")
}

// buildLabels assembles the cluster labels: kube_tag plus any reconciler_*
// overrides. NOTE: the reconciler launcher skips entirely unless BOTH
// reconciler_version and reconciler_binary_url resolve (from these labels or
// the template defaults). If you pass a URL, pass a version too.
func (r *runner) buildLabels() map[string]string {
	labels := map[string]string{}
	// Only override kube_tag when explicitly set. On clouds that keep one
	// template per version (template name == version, kube_tag baked in), leave
	// KUBE_TAG empty so the template's own kube_tag stands.
	if r.cfg.kubeTag != "" {
		labels["kube_tag"] = r.cfg.kubeTag
	}
	if r.cfg.reconcilerVersion != "" {
		labels["reconciler_version"] = r.cfg.reconcilerVersion
	}
	if r.cfg.reconcilerURL != "" {
		labels["reconciler_binary_url"] = r.cfg.reconcilerURL
	}
	for kv := range strings.SplitSeq(r.cfg.extraLabels, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		if k, v, ok := strings.Cut(kv, "="); ok {
			labels[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return labels
}

func (r *runner) createCluster(ctx context.Context) (string, error) {
	tmplID, err := r.resolveTemplateID(ctx, r.cfg.template)
	if err != nil {
		return "", fmt.Errorf("resolve template: %w", err)
	}
	keypair, err := r.ensureKeypair(ctx)
	if err != nil {
		return "", err
	}
	labels := r.buildLabels()
	r.log("=== create cluster %s (template=%s, keypair=%s, masters=%d, workers=%d) ===", r.cfg.clusterName, r.cfg.template, keypair, r.cfg.masterCount, r.cfg.nodeCount)
	r.log("label overrides (merged onto template): %v", labels)
	mc, nc := r.cfg.masterCount, r.cfg.nodeCount
	// merge_labels=true is REQUIRED: Magnum otherwise REPLACES the template's
	// whole label set with whatever we pass (cluster.py: `else` branch with
	// merge_labels=False), which would drop the template's CNI/runtime/etc. and
	// break the cluster. With merge_labels=true our reconciler_*/kube_tag
	// overrides are layered on top of the template defaults (ours win on tie).
	mergeLabels := true
	uuid, err := clusters.Create(ctx, r.magnum, clusters.CreateOpts{
		Name:              r.cfg.clusterName,
		ClusterTemplateID: tmplID,
		Keypair:           keypair,
		MasterCount:       &mc,
		NodeCount:         &nc,
		Labels:            labels,
		MergeLabels:       &mergeLabels,
	}).Extract()
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	if err := r.waitStatus(ctx, "CREATE_COMPLETE"); err != nil {
		return uuid, err
	}
	return uuid, nil
}

func (r *runner) upgradeCluster(ctx context.Context) error {
	r.log("=== upgrade cluster -> %s ===", r.cfg.kubeTagUpgrade)
	// Magnum upgrade targets a template. Many deployments parameterise a single
	// template by the kube_tag label, in which case -upgrade-template == -template
	// and the version bump rides on the cluster's kube_tag; if you keep a distinct
	// template per version, point -upgrade-template at it.
	tmplID, err := r.resolveTemplateID(ctx, r.cfg.upgradeTemplate)
	if err != nil {
		return fmt.Errorf("resolve upgrade template: %w", err)
	}
	if _, err := clusters.Upgrade(ctx, r.magnum, r.cfg.clusterName, clusters.UpgradeOpts{ClusterTemplate: tmplID}).Extract(); err != nil {
		return fmt.Errorf("upgrade: %w", err)
	}
	if err := r.waitStatus(ctx, "UPDATE_COMPLETE"); err != nil {
		return err
	}
	return r.smokeCore(ctx)
}

func (r *runner) resizeCluster(ctx context.Context) error {
	r.log("=== resize cluster workers -> %d ===", r.cfg.nodeCountResize)
	n := r.cfg.nodeCountResize
	if _, err := clusters.Resize(ctx, r.magnum, r.cfg.clusterName, clusters.ResizeOpts{NodeCount: &n}).Extract(); err != nil {
		return fmt.Errorf("resize: %w", err)
	}
	if err := r.waitStatus(ctx, "UPDATE_COMPLETE"); err != nil {
		return err
	}
	return r.smokeCore(ctx)
}

func (r *runner) deleteCluster(ctx context.Context) error {
	if _, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract(); err != nil {
		r.log("cluster %s not present — nothing to delete", r.cfg.clusterName)
		return nil
	}
	r.log("=== delete cluster %s ===", r.cfg.clusterName)
	if err := clusters.Delete(ctx, r.magnum, r.cfg.clusterName).ExtractErr(); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	// Best-effort wait; a delete-complete cluster vanishes (Get 404), which
	// waitStatus treats as success for the DELETE_COMPLETE target.
	if err := r.waitStatus(ctx, "DELETE_COMPLETE"); err != nil {
		r.err("waiting for delete: %v (continuing)", err)
	}
	r.cleanupEphemeralKeypair(ctx)
	r.unstageBootstrap(ctx)
	return nil
}

// cleanupEphemeralKeypair best-effort deletes the auto-created keypair. The
// name is deterministic, so this also cleans up after a separate -teardown
// invocation. No-op when KEYPAIR was supplied (we never created one).
func (r *runner) cleanupEphemeralKeypair(ctx context.Context) {
	if r.cfg.keypair != "" {
		return
	}
	nova, err := r.computeClient()
	if err != nil {
		return
	}
	name := r.ephemeralKeypairName()
	if err := keypairs.Delete(ctx, nova, name, nil).ExtractErr(); err == nil {
		r.log("deleted ephemeral keypair %q", name)
	}
}

// waitStatus polls the cluster status until it reaches want, fails fast on any
// *_FAILED status, and (for DELETE_COMPLETE) treats a vanished cluster as done.
func (r *runner) waitStatus(ctx context.Context, want string) error {
	deadline := time.Now().Add(time.Duration(r.cfg.timeoutMin) * time.Minute)
	r.log("waiting for status %s (timeout %dm)", want, r.cfg.timeoutMin)
	for {
		c, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
		if err != nil {
			if want == "DELETE_COMPLETE" {
				r.log("cluster gone — reached %s", want)
				return nil
			}
			// transient API error — retry until deadline
		} else {
			switch {
			case c.Status == want:
				r.log("reached %s", c.Status)
				return nil
			case strings.HasSuffix(c.Status, "_FAILED"):
				return fmt.Errorf("cluster entered %s: %s %v", c.Status, c.StatusReason, c.Faults)
			}
		}
		if time.Now().After(deadline) {
			last := "UNKNOWN"
			if c != nil {
				last = c.Status
			}
			return fmt.Errorf("timed out waiting for %s (last: %s)", want, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
		}
	}
}

// listClusters prints every Magnum cluster in the project with its status —
// a diagnostic for spotting leftovers stuck in *_IN_PROGRESS / *_FAILED.
func (r *runner) listClusters(ctx context.Context) error {
	pages, err := clusters.ListDetail(r.magnum, nil).AllPages(ctx)
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	cs, err := clusters.ExtractClusters(pages)
	if err != nil {
		return fmt.Errorf("extract clusters: %w", err)
	}
	if len(cs) == 0 {
		r.log("no clusters in project")
		return nil
	}
	r.log("clusters (%d):", len(cs))
	for _, c := range cs {
		r.log("  %-28s %-18s masters=%d nodes=%d uuid=%s", c.Name, c.Status, c.MasterCount, c.NodeCount, c.UUID)
		if c.StatusReason != "" {
			r.log("      reason: %s", c.StatusReason)
		}
		if len(c.Faults) > 0 {
			r.log("      faults: %v", c.Faults)
		}
	}
	return nil
}

func (r *runner) dumpClusterState(ctx context.Context) {
	c, err := clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
	if err != nil {
		r.err("could not fetch cluster state: %v", err)
		return
	}
	r.err("cluster %s status=%s reason=%q faults=%v", c.Name, c.Status, c.StatusReason, c.Faults)
}
