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
	"github.com/gophercloud/gophercloud/v2/openstack/containerinfra/v1/nodegroups"
)

// runner holds the authenticated Magnum service client and run configuration.
type runner struct {
	cfg      config
	provider *gophercloud.ProviderClient
	magnum   *gophercloud.ServiceClient
	nova     *gophercloud.ServiceClient // compute client, built lazily
	swift    *gophercloud.ServiceClient // object-store client, built lazily
	heat     *gophercloud.ServiceClient // orchestration (Heat) client, built lazily
	sshKey   []byte                     // private key for node SSH (ephemeral keypair PEM, captured at create)

	stackNameCache string // resolved Heat stack name (truncated cluster name + stack short-id)

	// nodepoolActive tracks whether the extra worker nodepool currently exists
	// (toggled by add-nodepool / del-nodepool ops). When true, the verify bundle
	// also asserts the nodepool is schedulable.
	nodepoolActive bool

	// ladder is the ordered upgrade-template walk (version-ladder scenario); each
	// `upgrade` op advances ladderPos by one rung. Empty = a single fixed upgrade
	// template (cfg.upgradeTemplate).
	ladder    []string
	ladderPos int

	// Per-step result tracking for the end-of-run PASS/FAIL/SKIP summary (stdout +
	// GitHub run summary + JUnit). runFailed latches on the first failed step so
	// later steps record as SKIP instead of running.
	steps     []stepResult
	runFailed bool
	runErr    error
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
	kp, err := keypairs.Create(ctx, nova, keypairs.CreateOpts{Name: name}).Extract()
	if err != nil {
		return "", fmt.Errorf("create ephemeral keypair: %w", err)
	}
	// Retain the generated private key (Nova returns it only at creation) so the
	// diagnostics path can SSH to nodes for on-host logs when an op fails.
	r.sshKey = []byte(kp.PrivateKey)
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
	// Default Magnum microversion is 1.1, which predates the resize (1.7) and
	// upgrade (1.8) cluster actions — calling them on 1.1 returns 406 Not
	// Acceptable. Pin to the fork's CURRENT_MAX_VER (1.10) so the full
	// create→upgrade→resize→ca-rotate flow is accepted.
	magnum.Microversion = "1.10"
	return &runner{cfg: cfg, provider: provider, magnum: magnum, ladder: splitTrim(cfg.upgradeLadder)}, nil
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

// run executes the full lifecycle: create the cluster in its configured shape,
// run the baseline smoke + cloud integration, then drive the resolved op chain
// (scenario preset, explicit OPS, or legacy SKIP_* flags). Each op settles the
// cluster, triggers its Magnum action with busy/transient retry, waits for the
// update to actually start and complete, then runs the verify bundle. main()
// handles teardown.
func (r *runner) run(ctx context.Context) error {
	// Resolve (and validate) the op chain BEFORE creating any billed resource,
	// so a bad scenario/op name fails fast.
	ops, err := r.resolveOpList()
	if err != nil {
		return err
	}
	r.log("op chain (%d): %s", len(ops), formatOps(ops))

	// If the chain drives the cluster-autoscaler, the cluster must be CREATEd with
	// it enabled (master-0 deploys the autoscaler from AUTO_SCALING_ENABLED + the
	// worker nodegroup's min/max). Inject the labels before create.
	if opsContain(ops, "autoscale") {
		r.enableAutoscalerLabels()
	}

	if err := r.preflight(ctx); err != nil {
		return err
	}
	// Stage the freshly-built reconciler binary into Swift (if -bootstrap-binary
	// is set) so the nodes fetch this exact build; sets reconciler_binary_url.
	if err := r.stageBootstrap(ctx); err != nil {
		return err
	}
	// Each phase is a tracked step: on the first failure the rest record as SKIP
	// and `do` captures diagnostics at the failure point, so the end-of-run
	// summary shows exactly what passed, what failed, and what never ran.
	// printRunSummary always fires (even on failure / panic).
	opLabels := r.opStepLabels(ops)
	defer r.printRunSummary()

	r.do(ctx, "create", "create cluster from "+r.cfg.template, func() error {
		uuid, e := r.createCluster(ctx)
		if e == nil {
			r.log("cluster uuid=%s", uuid)
		}
		return e
	})
	r.do(ctx, "create-smoke", "nodes Ready + core pods up", func() error { return r.smokeCore(ctx) })
	r.do(ctx, "create-nodecount", "k8s nodes == nodegroup counts", func() error { return r.verifyNodeCount(ctx) })
	r.do(ctx, "create-cloud", opDescriptions["cloud-smoke"], func() error { return r.smokeCloudIntegration(ctx) })

	for i, o := range ops {
		if !r.runFailed {
			r.log("──────── op %d/%d: %s ────────", i+1, len(ops), formatOp(o))
		}
		o := o
		r.do(ctx, opLabels[i], opDescription(o), func() error { return r.execOp(ctx, o) })
	}
	return r.runErr
}

// execOp dispatches a single op to its Magnum trigger (via runMutation, which
// owns settle/retry/wait/verify) or to a read-only verification.
func (r *runner) execOp(ctx context.Context, o op) error {
	switch o.name {
	case "upgrade":
		// Resolve the target rung + advance the ladder ONCE per op (not per
		// trigger retry inside runMutation), so a retried trigger re-fires the
		// same rung instead of skipping ahead.
		target, err := r.upgradeTarget()
		if err != nil {
			return err
		}
		return r.runMutation(ctx, "upgrade->"+target, true, func() error { return r.triggerUpgrade(ctx, target) })

	case "ca-rotate":
		return r.runMutation(ctx, "ca-rotate", true, func() error { return r.triggerCARotate(ctx) })

	case "resize-workers":
		target := o.argOr(r.cfg.nodeCountResize)
		return r.runMutation(ctx, fmt.Sprintf("resize-workers=%d", target), true, func() error {
			return r.triggerWorkerResize(ctx, target)
		})

	case "resize-masters":
		target := o.argOr(r.cfg.masterCountResize)
		return r.runMutation(ctx, fmt.Sprintf("resize-masters=%d", target), true, func() error {
			ng, err := r.resolveNodeGroup(ctx, "master")
			if err != nil {
				return err
			}
			return r.triggerNodeGroupResize(ctx, ng, target)
		})

	case "resize-nodepool":
		target := o.argOr(1)
		return r.runMutation(ctx, fmt.Sprintf("resize-nodepool=%d", target), true, func() error {
			ng, err := r.resolveNodeGroupByName(ctx, r.nodepoolName())
			if err != nil {
				return err
			}
			return r.triggerNodeGroupResize(ctx, ng, target)
		})

	case "add-nodepool":
		count := o.argOr(1)
		if err := r.runMutation(ctx, fmt.Sprintf("add-nodepool=%d", count), true, func() error {
			return r.triggerNodepoolCreate(ctx, count)
		}); err != nil {
			return err
		}
		r.nodepoolActive = true
		return nil

	case "del-nodepool":
		if err := r.runMutation(ctx, "del-nodepool", true, func() error {
			return r.triggerNodepoolDelete(ctx)
		}); err != nil {
			return err
		}
		r.nodepoolActive = false
		return nil

	case "post-rotate":
		return r.postRotateScale(ctx)

	case "cloud-smoke":
		return r.smokeCloudIntegration(ctx)

	case "verify-sa":
		return r.verifySAConsistency(ctx)

	case "autoscale":
		return r.autoscaleCycle(ctx)
	}
	return fmt.Errorf("unhandled op %q", o.name)
}

// preflight verifies auth works and that the configured template + keypair are
// present before any billed resource is created.
func (r *runner) preflight(ctx context.Context) error {
	if r.cfg.template == "" {
		return fmt.Errorf("CLUSTER_TEMPLATE (-template) is required")
	}
	// Resolve + print the cluster shape and op chain so a dry -preflight run
	// shows exactly what a full run would do (and fails fast on a bad scenario).
	ops, err := r.resolveOpList()
	if err != nil {
		return err
	}
	r.log("scenario=%q shape: %d master(s) / %d worker(s), nodepool=%q", r.cfg.scenario, r.cfg.masterCount, r.cfg.nodeCount, r.nodepoolName())
	r.log("op chain (%d): %s", len(ops), formatOps(ops))
	// KEYPAIR is optional — an ephemeral keypair is auto-created when unset.
	// Resolve to a UUID first (GET-by-name 409s when several templates share a
	// name, e.g. a public + a private copy; resolveTemplateID disambiguates via
	// the project list), then GET by UUID for the unambiguous detail.
	tmplID, err := r.resolveTemplateID(ctx, r.cfg.template)
	if err != nil {
		return fmt.Errorf("cluster template %q: %w", r.cfg.template, err)
	}
	ct, err := clustertemplates.Get(ctx, r.magnum, tmplID).Extract()
	if err != nil {
		return fmt.Errorf("cluster template %q (uuid %s): %w", r.cfg.template, tmplID, err)
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
	if len(r.ladder) == 0 && r.cfg.upgradeTemplate != r.cfg.template {
		if _, err := r.resolveTemplateID(ctx, r.cfg.upgradeTemplate); err != nil {
			return fmt.Errorf("upgrade template %q: %w", r.cfg.upgradeTemplate, err)
		}
	}
	// Resolve every upgrade-ladder rung up front so a typo / missing version
	// template fails the dry preflight, before any real resource is created.
	for i, rung := range r.ladder {
		if _, err := r.resolveTemplateID(ctx, rung); err != nil {
			return fmt.Errorf("upgrade ladder rung %d %q: %w", i+1, rung, err)
		}
	}
	if len(r.ladder) > 0 {
		r.log("upgrade ladder OK (%d rungs): %s", len(r.ladder), strings.Join(r.ladder, " → "))
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

// upgradeTarget picks the next upgrade template and advances the ladder cursor
// exactly once per upgrade op. Without a ladder (the common single-hop case) it
// returns the fixed -upgrade-template; with a ladder it returns the next rung and
// errors if the op chain asked for more upgrades than there are rungs.
func (r *runner) upgradeTarget() (string, error) {
	if len(r.ladder) == 0 {
		return r.cfg.upgradeTemplate, nil
	}
	t, next, err := nextLadderTarget(r.ladder, r.ladderPos)
	if err != nil {
		return "", err
	}
	r.log("upgrade ladder rung %d/%d → %s", r.ladderPos+1, len(r.ladder), t)
	r.ladderPos = next
	return t, nil
}

// triggerUpgrade fires the Magnum upgrade action against the given target
// template (no wait — runMutation owns settle/wait/verify). Magnum upgrade
// targets a template; many deployments parameterise a single template by the
// kube_tag label, in which case -upgrade-template == -template and the version
// bump rides on the cluster's kube_tag; if you keep a distinct template per
// version (or a version-ladder), each rung is its own template.
func (r *runner) triggerUpgrade(ctx context.Context, target string) error {
	tmplID, err := r.resolveTemplateID(ctx, target)
	if err != nil {
		return fmt.Errorf("resolve upgrade template %q: %w", target, err)
	}
	if _, err := clusters.Upgrade(ctx, r.magnum, r.cfg.clusterName, clusters.UpgradeOpts{ClusterTemplate: tmplID}).Extract(); err != nil {
		return fmt.Errorf("upgrade -> %s: %w", target, err)
	}
	return nil
}

// triggerWorkerResize fires a resize of the default worker count (no wait).
func (r *runner) triggerWorkerResize(ctx context.Context, target int) error {
	n := target
	if _, err := clusters.Resize(ctx, r.magnum, r.cfg.clusterName, clusters.ResizeOpts{NodeCount: &n}).Extract(); err != nil {
		return fmt.Errorf("resize workers -> %d: %w", target, err)
	}
	return nil
}

// resolveNodeGroup returns the default nodegroup with the given role
// ("master" or "worker"), falling back to the first non-default one.
func (r *runner) resolveNodeGroup(ctx context.Context, role string) (*nodegroups.NodeGroup, error) {
	pages, err := nodegroups.List(r.magnum, r.cfg.clusterName, nil).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodegroups: %w", err)
	}
	ngs, err := nodegroups.ExtractNodeGroups(pages)
	if err != nil {
		return nil, fmt.Errorf("extract nodegroups: %w", err)
	}
	var fallback *nodegroups.NodeGroup
	for i := range ngs {
		if ngs[i].Role != role {
			continue
		}
		if ngs[i].IsDefault {
			ng := ngs[i]
			return &ng, nil
		}
		if fallback == nil {
			ng := ngs[i]
			fallback = &ng
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("no %s nodegroup found for cluster %s", role, r.cfg.clusterName)
}

// resolveNodeGroupByName returns the nodegroup with the given name (used for the
// extra nodepool, which is non-default).
func (r *runner) resolveNodeGroupByName(ctx context.Context, name string) (*nodegroups.NodeGroup, error) {
	pages, err := nodegroups.List(r.magnum, r.cfg.clusterName, nil).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodegroups: %w", err)
	}
	ngs, err := nodegroups.ExtractNodeGroups(pages)
	if err != nil {
		return nil, fmt.Errorf("extract nodegroups: %w", err)
	}
	for i := range ngs {
		if ngs[i].Name == name {
			ng := ngs[i]
			return &ng, nil
		}
	}
	return nil, fmt.Errorf("nodegroup %q not found on cluster %s", name, r.cfg.clusterName)
}

// triggerNodeGroupResize resizes a specific nodegroup (worker/master/nodepool)
// to target (no wait — runMutation owns settle/wait/verify). The Magnum resize
// action is pinned to the nodegroup by UUID.
func (r *runner) triggerNodeGroupResize(ctx context.Context, ng *nodegroups.NodeGroup, target int) error {
	n := target
	opts := clusters.ResizeOpts{NodeCount: &n, NodeGroup: ng.UUID}
	if _, err := clusters.Resize(ctx, r.magnum, r.cfg.clusterName, opts).Extract(); err != nil {
		return fmt.Errorf("resize nodegroup %s (%s) -> %d: %w", ng.Name, ng.Role, target, err)
	}
	return nil
}

// nodepoolName returns the configured extra-nodepool name (default "e2e-np").
func (r *runner) nodepoolName() string {
	if r.cfg.nodepoolName != "" {
		return r.cfg.nodepoolName
	}
	return "e2e-np"
}

// triggerNodepoolCreate creates the extra worker nodegroup (no wait). It runs
// with a distinct flavor when NODEPOOL_FLAVOR is set, so the cluster carries
// heterogeneous node sizes through the op chain. MergeLabels keeps the
// template's CNI/runtime/reconciler labels (see merge_labels gotcha).
func (r *runner) triggerNodepoolCreate(ctx context.Context, count int) error {
	n := count
	merge := true
	opts := nodegroups.CreateOpts{
		Name:        r.nodepoolName(),
		Role:        "worker",
		NodeCount:   &n,
		MergeLabels: &merge,
	}
	if r.cfg.nodepoolFlavor != "" {
		opts.FlavorID = r.cfg.nodepoolFlavor
	}
	ng, err := nodegroups.Create(ctx, r.magnum, r.cfg.clusterName, opts).Extract()
	if err != nil {
		return fmt.Errorf("create nodepool %q: %w", r.nodepoolName(), err)
	}
	r.log("nodepool %q (uuid=%s, flavor=%q) creating with %d node(s)", ng.Name, ng.UUID, r.cfg.nodepoolFlavor, count)
	return nil
}

// triggerNodepoolDelete deletes the extra worker nodegroup (no wait).
func (r *runner) triggerNodepoolDelete(ctx context.Context) error {
	ng, err := r.resolveNodeGroupByName(ctx, r.nodepoolName())
	if err != nil {
		return err
	}
	if err := nodegroups.Delete(ctx, r.magnum, r.cfg.clusterName, ng.UUID).ExtractErr(); err != nil {
		return fmt.Errorf("delete nodepool %q: %w", ng.Name, err)
	}
	return nil
}

// postRotateScale reproduces the exact incident: a node ADDED after a CA
// rotation. The fork rotates the service-account keys into the child node
// stacks only; without the cluster-stack sync, a node created here would
// render the PRE-rotation SA keys and split the cluster's SA-token trust. This
// adds a node post-rotation; the verify bundle (run by runMutation) asserts it
// joins Ready, the node count matches, and SA tokens validate across masters.
//
// It prefers adding a MASTER — the new apiserver is the SA-key surface — when
// MASTER_COUNT_RESIZE > master count; otherwise it adds a worker.
func (r *runner) postRotateScale(ctx context.Context) error {
	role := "worker"
	if r.cfg.masterCountResize > r.cfg.masterCount {
		role = "master"
	}
	ng, err := r.resolveNodeGroup(ctx, role)
	if err != nil {
		return err
	}
	target := ng.NodeCount + 1
	if role == "master" {
		target = r.cfg.masterCountResize
	}
	name := fmt.Sprintf("post-rotate add %s (%s %d->%d)", role, ng.Name, ng.NodeCount, target)
	return r.runMutation(ctx, name, true, func() error {
		return r.triggerNodeGroupResize(ctx, ng, target)
	})
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

// getCluster fetches the current cluster object (status + updated_at snapshot).
func (r *runner) getCluster(ctx context.Context) (*clusters.Cluster, error) {
	return clusters.Get(ctx, r.magnum, r.cfg.clusterName).Extract()
}

// ensureSettled blocks until the cluster is in a terminal state (*_COMPLETE),
// failing fast on *_FAILED. It is the pre-op gate: a mutating op must never be
// triggered while a previous update is still in flight (Magnum would reject it
// with 400 "Updating a cluster when status is UPDATE_IN_PROGRESS is not
// supported").
func (r *runner) ensureSettled(ctx context.Context) error {
	deadline := time.Now().Add(time.Duration(r.cfg.timeoutMin) * time.Minute)
	for {
		c, err := r.getCluster(ctx)
		if err == nil {
			switch {
			case strings.HasSuffix(c.Status, "_FAILED"):
				return fmt.Errorf("cluster in %s: %s %v", c.Status, c.StatusReason, c.Faults)
			case strings.HasSuffix(c.Status, "_COMPLETE"):
				return nil
			default:
				r.log("waiting for cluster to settle (status %s)", c.Status)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for cluster to settle")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
		}
	}
}

// transitionWindow bounds how long waitTransition watches for an update to
// visibly start before assuming the op was a no-op (or completed instantly) and
// proceeding to waitStatus. Kept short so a genuine no-op doesn't stall.
const transitionWindow = 3 * time.Minute

// waitTransition confirms the just-triggered op actually started — i.e. the
// status moved to *_IN_PROGRESS or updated_at advanced past the pre-trigger
// snapshot. This defeats the stale-*_COMPLETE race: Magnum status lags a trigger
// by a few seconds, so polling for UPDATE_COMPLETE immediately can match the
// PREVIOUS completion and return before the op even began.
func (r *runner) waitTransition(ctx context.Context, before *clusters.Cluster) error {
	if before == nil {
		return nil // no snapshot — skip straight to completion wait
	}
	deadline := time.Now().Add(transitionWindow)
	for {
		c, err := r.getCluster(ctx)
		if err == nil {
			switch {
			case strings.HasSuffix(c.Status, "_FAILED"):
				return fmt.Errorf("cluster entered %s: %s %v", c.Status, c.StatusReason, c.Faults)
			case strings.HasSuffix(c.Status, "_IN_PROGRESS"):
				return nil
			case c.UpdatedAt.After(before.UpdatedAt):
				return nil // already advanced (fast op)
			}
		}
		if time.Now().After(deadline) {
			r.log("update did not visibly transition within %s — proceeding to completion wait", transitionWindow)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// runMutation is the robust wrapper EVERY mutating op goes through:
//
//	ensureSettled → snapshot → trigger (retry on busy/transient) →
//	waitTransition → waitStatus(UPDATE_COMPLETE) → verifyBundle
//
// disruptive controls whether the verify bundle also runs the SA-consistency
// probe. The retry loop is what lets a chained op (e.g. ca-rotate right after an
// upgrade) survive Magnum's lagging status instead of racing it into a 400.
func (r *runner) runMutation(ctx context.Context, name string, disruptive bool, trigger func() error) error {
	r.log("=== op: %s ===", name)
	if err := r.ensureSettled(ctx); err != nil {
		return fmt.Errorf("pre-op settle: %w", err)
	}
	before, _ := r.getCluster(ctx)

	const maxAttempts = 5
	triggered := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := trigger()
		if err == nil {
			triggered = true
			break
		}
		if !retryableMutationErr(err) {
			return fmt.Errorf("trigger: %w", err)
		}
		r.log("trigger attempt %d/%d not accepted (%v) — settling + retrying", attempt, maxAttempts, err)
		if serr := r.ensureSettled(ctx); serr != nil {
			return fmt.Errorf("settle before retry: %w", serr)
		}
		backoff := time.Duration(attempt) * 15 * time.Second
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	if !triggered {
		return fmt.Errorf("trigger not accepted after %d attempts", maxAttempts)
	}

	if err := r.waitTransition(ctx, before); err != nil {
		return fmt.Errorf("wait for update to start: %w", err)
	}
	if err := r.waitStatus(ctx, "UPDATE_COMPLETE"); err != nil {
		return err
	}
	return r.verifyBundle(ctx, name, disruptive)
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

	// Persist the failure context to the diagnostics dir so the CI artifact upload
	// always has something — even when the Kubernetes API is unreachable. The Heat
	// + node-log collectors add the deep detail (reconciler run-once output and the
	// on-host service journals).
	var b strings.Builder
	fmt.Fprintf(&b, "cluster:  %s\nstatus:   %s\nreason:   %s\ntime:     %s\n\nfaults:\n",
		c.Name, c.Status, c.StatusReason, time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	for k, v := range c.Faults {
		fmt.Fprintf(&b, "  %s: %s\n", k, v)
	}
	r.writeDiagFile("cluster-state", b.String())
	// Per-step Heat + node-log diagnostics are captured by `do` at the exact
	// failing step; this stays a lightweight always-on status/faults snapshot.
}
