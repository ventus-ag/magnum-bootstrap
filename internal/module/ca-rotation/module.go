package carotation

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	coord "github.com/ventus-ag/magnum-bootstrap/internal/carotation"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	magnumapi "github.com/ventus-ag/magnum-bootstrap/internal/magnum"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

type Module struct{}

type Resource struct {
	pulumi.ResourceState
}

const (
	adminConf   = "/etc/kubernetes/admin.conf"
	rootKube    = "/root/.kube/config"
	etcdctlPath = "/usr/local/bin/etcdctl"

	// phaseOverrideEnv forces a single phase without coordination, for manual
	// recovery of a wedged rotation.
	phaseOverrideEnv = "MAGNUM_CA_ROTATION_PHASE"
)

// Cert directory roots. Vars so tests can redirect them away from /etc.
var (
	certDir     = "/etc/kubernetes/certs"
	etcdCertDir = "/etc/etcd/certs"
)

func (Module) PhaseID() string        { return "ca-rotation" }
func (Module) Dependencies() []string { return []string{"prereq-validation"} }

// RetryPolicy opts CA rotation out of the default per-module retry: a rotation
// failure is usually deterministic (cert/Barbican/keypair mismatch) rather than
// transient, and its slow path is a 7.5min API health wait — retrying would
// double that before failing without improving the odds of success.
func (Module) RetryPolicy() moduleapi.RetryPolicy { return moduleapi.RetryPolicy{MaxAttempts: 1} }

func (Module) Run(ctx context.Context, cfg config.Config, req moduleapi.Request) (moduleapi.Result, error) {
	rotationID, lastAppliedRotationID, err := resolveCARotationIDs(cfg, req, coord.MarkerPath)
	if err != nil {
		return moduleapi.Result{}, err
	}

	// Skip conditions: no rotation, TLS disabled, mixed operation, or already
	// finalized (marker matches).
	if rotationID == "" || cfg.Shared.TLSDisabled {
		return moduleapi.Result{}, nil
	}
	if !cfg.IsPureCARotation() {
		logf(req, "skipping ca rotation rotationId=%s operation=%s (not a pure CA rotation)", rotationID, cfg.Operation())
		return moduleapi.Result{}, nil
	}
	if lastAppliedRotationID == rotationID {
		logf(req, "skipping ca rotation rotationId=%s (already finalized)", rotationID)
		return moduleapi.Result{}, nil
	}

	// A brand-new node has nothing to rotate FROM. The ca-rotation phase runs
	// before the certificates phase, so on first boot there is no live ca.crt
	// yet. This is reached when a node is ADDED (resize, autoscale,
	// replacement) while the cluster stack still carries a current
	// ca_rotation_id: the new node inherits that id but must NOT run the
	// rotation protocol — it has no old material to stage, and no etcd
	// quorum / API access yet. It provisions fresh certs against the
	// already-current CA via the certificates phase. Record the rotation as
	// applied so it is not retried on later runs.
	if !nonEmpty(filepath.Join(certDir, "ca.crt")) {
		logf(req, "skipping ca rotation rotationId=%s: fresh node with no live CA (provisioning against current CA)", rotationID)
		if req.Apply {
			if err := coord.WriteMarker(rotationID); err != nil {
				return moduleapi.Result{}, fmt.Errorf("ca-rotation: mark rotation applied on fresh node: %w", err)
			}
		}
		return moduleapi.Result{}, nil
	}

	// Dry-run: report the rotation as a planned replace and stop.
	if !req.Apply {
		return moduleapi.Result{Changes: []host.Change{{
			Action:  host.ActionReplace,
			Path:    certDir,
			Summary: fmt.Sprintf("dual-CA rotate certificates (rotation_id=%s)", rotationID),
		}}}, nil
	}

	if cfg.Role() == config.RoleMaster &&
		(cfg.Shared.KubeServiceAccountKey == "" || cfg.Shared.KubeServiceAccountPrivateKey == "") {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: service account keys must be provided")
	}

	return runProtocol(ctx, cfg, req, rotationID)
}

// runProtocol drives the dual-CA prepare/cutover/finalize protocol with
// Kubernetes-API coordination, resuming from persisted state when present.
func runProtocol(ctx context.Context, cfg config.Config, req moduleapi.Request, rotationID string) (moduleapi.Result, error) {
	role := cfg.Role()
	isMaster := role == config.RoleMaster
	nodeName := cfg.Shared.InstanceName
	executor := host.NewExecutor(req.Apply, req.Logger)
	r := &runner{cfg: cfg, req: req, rotationID: rotationID, role: role, isMaster: isMaster, nodeName: nodeName, executor: executor}

	// Manual single-phase override (no coordination), for recovery.
	if override := strings.TrimSpace(os.Getenv(phaseOverrideEnv)); override != "" {
		phase := coord.Phase(override)
		if !phase.Valid() || phase == coord.PhaseDone {
			return moduleapi.Result{}, fmt.Errorf("ca-rotation: invalid %s=%q", phaseOverrideEnv, override)
		}
		logf(req, "ca-rotation: manual override running phase %s only (no coordination)", phase)
		if err := r.ensureStaged(); err != nil {
			return moduleapi.Result{}, err
		}
		if err := r.runPhase(ctx, phase, nil); err != nil {
			return moduleapi.Result{}, err
		}
		return moduleapi.Result{Changes: r.changes, Warnings: r.warnings, Outputs: r.outputs()}, nil
	}

	st, err := coord.LoadState(rotationID)
	if err != nil {
		return moduleapi.Result{}, err
	}
	completed := st.Phase

	// Build the coordinator client. It follows the live ca.crt so it always
	// trusts whatever CA material is currently active (old, then bundle).
	c, err := coord.NewCoordinator(adminConf, certDir+"/ca.crt")
	if err != nil {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: build coordinator: %w", err)
	}
	r.coord = c

	// Masters own coordination setup: fail fast (before mutating anything) if
	// the API is unreachable.
	if isMaster {
		// Pre-flight: refuse to begin a fresh rotation on a master whose etcd
		// is not in a healthy quorum. Starting a CA rotation on an already
		// degraded control plane risks a state from which it cannot converge.
		// Skipped on resume (completed != "") so an in-progress rotation, where
		// etcd may be momentarily re-forming, is not aborted.
		if completed == "" {
			if err := waitForEtcdQuorum(executor); err != nil {
				return moduleapi.Result{}, fmt.Errorf("ca-rotation: pre-flight etcd quorum check failed; refusing to start rotation: %w", err)
			}
		}
		if err := c.EnsureNodeReadRBAC(ctx); err != nil {
			return moduleapi.Result{}, fmt.Errorf("ca-rotation: ensure coordination RBAC: %w", err)
		}
		if err := c.EnsureRotation(ctx, rotationID); err != nil {
			return moduleapi.Result{}, fmt.Errorf("ca-rotation: ensure rotation state: %w", err)
		}
	}

	if err := r.ensureStaged(); err != nil {
		return moduleapi.Result{}, err
	}

	// Refresh our reported status so a resumed run re-asserts progress.
	if completed.Valid() && completed != "" {
		_ = c.ReportStatus(ctx, nodeName, completed, rotationID)
	}

	barrierOpts := coord.BarrierOptions{Logf: func(f string, a ...any) { logf(req, f, a...) }}

	// Stage 1: prepare → barrier → Stage 2: cutover → barrier → Stage 3: finalize.
	for _, step := range []coord.Phase{coord.PhasePrepare, coord.PhaseCutover, coord.PhaseFinalize} {
		if !completed.AtLeast(step) {
			if err := r.runPhase(ctx, step, c); err != nil {
				return moduleapi.Result{}, err
			}
			completed = step
		}
		if step != coord.PhaseFinalize {
			if err := c.Barrier(ctx, rotationID, step, isMaster, barrierOpts); err != nil {
				return moduleapi.Result{}, err
			}
		}
	}

	// Finalize bookkeeping (idempotent): workload rollout, completion marker.
	if isMaster {
		pc, pw := patchWorkloads(executor, rotationID)
		r.changes = append(r.changes, pc...)
		r.warnings = append(r.warnings, pw...)
	}
	if err := coord.WriteMarker(rotationID); err != nil {
		return moduleapi.Result{}, fmt.Errorf("ca-rotation: write completion marker: %w", err)
	}
	_ = coord.SaveState(coord.State{RotationID: rotationID, Role: role.String(), Instance: nodeName,
		Phase: coord.PhaseDone, CAMode: coord.CAModeNew, LeafMode: coord.LeafModeNew,
		SAVerifyMode: coord.CAModeNew, SASignMode: coord.LeafModeNew})
	_ = os.RemoveAll(coord.StagingDir(rotationID))

	return moduleapi.Result{Changes: r.changes, Warnings: r.warnings, Outputs: r.outputs()}, nil
}

// runner carries shared state across the protocol phases.
type runner struct {
	cfg        config.Config
	req        moduleapi.Request
	rotationID string
	role       config.Role
	isMaster   bool
	nodeName   string
	executor   *host.Executor
	coord      *coord.Coordinator

	changes  []host.Change
	warnings []string
}

func (r *runner) outputs() map[string]string {
	return map[string]string{"caRotationId": r.rotationID, "role": r.role.String()}
}

// ensureStaged makes sure the old snapshot, new material and CA bundle exist in
// the staging directory. It is idempotent and safe to call on every run.
func (r *runner) ensureStaged() error {
	if err := snapshotOldMaterial(r.rotationID, r.role); err != nil {
		return err
	}
	if err := generateNewMaterial(r.cfg, r.rotationID, r.role); err != nil {
		return err
	}
	return buildBundle(r.rotationID)
}

// runPhase converges the node to `phase` (file writes + restart + health),
// reports status and persists state. When c is nil (manual override) status is
// not reported.
func (r *runner) runPhase(ctx context.Context, phase coord.Phase, c *coord.Coordinator) error {
	logf(r.req, "ca-rotation: entering phase %s rotationId=%s role=%s", phase, r.rotationID, r.role)

	switch phase {
	case coord.PhasePrepare:
		if err := r.writePrepareFiles(); err != nil {
			return err
		}
	case coord.PhaseCutover:
		if err := r.writeCutoverFiles(); err != nil {
			return err
		}
	case coord.PhaseFinalize:
		if err := r.writeFinalizeFiles(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("ca-rotation: unknown phase %q", phase)
	}

	if r.isMaster {
		cs, err := updateAdminKubeconfig(r.cfg, r.executor)
		if err != nil {
			return err
		}
		r.changes = append(r.changes, cs...)
		if err := r.chownCerts(); err != nil {
			return err
		}
	}

	// Restart services to load the new material, serialized cluster-wide via the
	// restart Lease so quorum and API availability are preserved. The Lease is
	// taken for ANY master count, not just >1: trusting NUMBER_OF_MASTERS to
	// decide whether to serialize is fragile (a stale/zero/incorrect value on a
	// real multi-master cluster would let every apiserver restart at once and
	// break quorum). On a genuine single-master cluster the Lease is
	// uncontended — acquire and release are effectively instant — so it is a
	// no-op cost there while making 2/3/4+ master clusters safe by construction.
	restart := func() error { return r.restartAndWait() }
	if c != nil && r.isMaster {
		release, err := c.AcquireRestartLock(ctx, r.nodeName, coord.LockOptions{
			Logf: func(f string, a ...any) { logf(r.req, f, a...) },
		})
		if err != nil {
			return err
		}
		err = restart()
		release()
		if err != nil {
			return err
		}
	} else if err := restart(); err != nil {
		return err
	}

	// Rebuild the coordinator client so it reflects the post-restart trust.
	if c != nil {
		if err := c.Reload(); err != nil {
			return err
		}
		if err := r.reportWithRetry(ctx, c, phase); err != nil {
			return err
		}
	}

	return coord.SaveState(r.stateFor(phase))
}

// reportWithRetry reports phase status, tolerating transient API errors so a
// momentary blip after a successful restart does not fail (and re-run) the
// whole phase.
func (r *runner) reportWithRetry(ctx context.Context, c *coord.Coordinator, phase coord.Phase) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = c.ReportStatus(ctx, r.nodeName, phase, r.rotationID); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("ca-rotation: report %s status: %w", phase, err)
}

func (r *runner) stateFor(phase coord.Phase) coord.State {
	s := coord.State{RotationID: r.rotationID, Role: r.role.String(), Instance: r.nodeName, Phase: phase}
	switch phase {
	case coord.PhasePrepare:
		s.CAMode, s.LeafMode, s.SAVerifyMode, s.SASignMode = coord.CAModeBundle, coord.LeafModeOld, coord.CAModeBundle, coord.LeafModeOld
	case coord.PhaseCutover:
		s.CAMode, s.LeafMode, s.SAVerifyMode, s.SASignMode = coord.CAModeBundle, coord.LeafModeNew, coord.CAModeBundle, coord.LeafModeNew
	case coord.PhaseFinalize:
		s.CAMode, s.LeafMode, s.SAVerifyMode, s.SASignMode = coord.CAModeNew, coord.LeafModeNew, coord.CAModeNew, coord.LeafModeNew
	}
	return s
}

// --- phase file writes -----------------------------------------------------

func (r *runner) writePrepareFiles() error {
	bundle, err := os.ReadFile(filepath.Join(coord.BundleDir(r.rotationID), "ca.crt"))
	if err != nil {
		return fmt.Errorf("ca-rotation: read bundle: %w", err)
	}
	if err := r.writeLiveCA(bundle); err != nil {
		return err
	}
	if r.isMaster {
		// SA verify set = new + old; signing key stays old until cutover.
		verify, err := concatPEM(
			filepath.Join(coord.NewDir(r.rotationID), "service_account.key"),
			filepath.Join(coord.OldDir(r.rotationID), "service_account.key"),
		)
		if err != nil {
			return err
		}
		if err := r.writeLive(certDir+"/service_account.key", verify, 0o440); err != nil {
			return err
		}
		// New CA signing key for the controller-manager (cert_manager_api).
		if r.cfg.Shared.CAKey != "" {
			if err := r.writeLive(certDir+"/ca.key", []byte(r.cfg.Shared.CAKey+"\n"), 0o400); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) writeCutoverFiles() error {
	leaves := leafNames(r.role)
	for _, name := range leaves {
		for _, ext := range []string{".crt", ".key"} {
			data, err := os.ReadFile(filepath.Join(coord.NewDir(r.rotationID), name+ext))
			if err != nil {
				return fmt.Errorf("ca-rotation: read new %s%s: %w", name, ext, err)
			}
			mode := os.FileMode(0o444)
			if ext == ".key" {
				mode = 0o440
			}
			if err := r.writeLeaf(name+ext, data, mode); err != nil {
				return err
			}
		}
	}
	if r.isMaster {
		// Switch SA signing key to new now that everyone verifies new+old.
		sign, err := os.ReadFile(filepath.Join(coord.NewDir(r.rotationID), "service_account_private.key"))
		if err != nil {
			return fmt.Errorf("ca-rotation: read new SA signing key: %w", err)
		}
		if err := r.writeLive(certDir+"/service_account_private.key", sign, 0o440); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) writeFinalizeFiles() error {
	newCA, err := os.ReadFile(filepath.Join(coord.NewDir(r.rotationID), "ca.crt"))
	if err != nil {
		return fmt.Errorf("ca-rotation: read new CA: %w", err)
	}
	if err := r.writeLiveCA(newCA); err != nil {
		return err
	}
	if r.isMaster {
		newVerify, err := os.ReadFile(filepath.Join(coord.NewDir(r.rotationID), "service_account.key"))
		if err != nil {
			return fmt.Errorf("ca-rotation: read new SA verify key: %w", err)
		}
		if err := r.writeLive(certDir+"/service_account.key", newVerify, 0o440); err != nil {
			return err
		}
	}
	return nil
}

// writeLiveCA writes ca.crt to the kube cert dir and, for masters, the etcd
// cert dir (etcd trusts the same CA material).
func (r *runner) writeLiveCA(content []byte) error {
	if err := r.writeLive(certDir+"/ca.crt", content, 0o444); err != nil {
		return err
	}
	if r.isMaster {
		if err := r.writeLive(etcdCertDir+"/ca.crt", content, 0o444); err != nil {
			return err
		}
	}
	return nil
}

// writeLeaf writes a leaf cert/key to the kube cert dir and mirrors it into the
// etcd cert dir for masters (matches the legacy etcd cert handoff).
func (r *runner) writeLeaf(name string, content []byte, mode os.FileMode) error {
	if err := r.writeLive(certDir+"/"+name, content, mode); err != nil {
		return err
	}
	if r.isMaster {
		if err := r.writeLive(etcdCertDir+"/"+name, content, mode); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) writeLive(path string, content []byte, mode os.FileMode) error {
	change, err := atomicWrite(path, content, mode)
	if err != nil {
		return fmt.Errorf("ca-rotation: write %s: %w", path, err)
	}
	if change != nil {
		r.changes = append(r.changes, *change)
	}
	return nil
}

func (r *runner) chownCerts() error {
	if err := r.executor.Run("chown", "-R", "kube:kube_etcd", certDir); err != nil {
		return fmt.Errorf("ca-rotation: chown %s: %w", certDir, err)
	}
	if err := r.executor.Run("chown", "-R", "etcd:kube_etcd", etcdCertDir); err != nil {
		return fmt.Errorf("ca-rotation: chown %s: %w", etcdCertDir, err)
	}
	return nil
}

// --- service restart + health ----------------------------------------------

func (r *runner) restartAndWait() error {
	if err := r.executor.Run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("ca-rotation: systemctl daemon-reload: %w", err)
	}
	for _, svc := range rotationServices(r.role) {
		if err := r.executor.Run("systemctl", "restart", svc); err != nil {
			return fmt.Errorf("ca-rotation: restart %s: %w", svc, err)
		}
		r.changes = append(r.changes, host.Change{Action: host.ActionRestart, Summary: fmt.Sprintf("restart %s (CA rotation)", svc)})
		if !r.executor.WaitForSystemctlActive(svc, serviceStartTimeout(svc), 2*time.Second) {
			return fmt.Errorf("ca-rotation: service %s did not become active after restart", svc)
		}
	}
	return waitForHealthy(r.cfg, r.executor)
}

func rotationServices(role config.Role) []string {
	if role == config.RoleMaster {
		return []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet", "kube-proxy"}
	}
	return []string{"kubelet", "kube-proxy"}
}

func serviceStartTimeout(svc string) time.Duration {
	switch svc {
	case "etcd":
		return 120 * time.Second
	case "kube-apiserver":
		return 90 * time.Second
	default:
		return 60 * time.Second
	}
}

func waitForHealthy(cfg config.Config, executor *host.Executor) error {
	if cfg.Role() != config.RoleMaster {
		for i := 0; i < 30; i++ {
			if executor.SystemctlIsActive("kubelet") {
				return nil
			}
			time.Sleep(2 * time.Second)
		}
		return fmt.Errorf("ca-rotation: kubelet did not become active")
	}
	for _, svc := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kubelet"} {
		healthy := false
		for i := 0; i < 30; i++ {
			if executor.SystemctlIsActive(svc) {
				healthy = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !healthy {
			return fmt.Errorf("ca-rotation: service %s did not become active", svc)
		}
		// As soon as etcd is active, confirm it has rejoined a healthy quorum
		// before declaring the node healthy. This is what makes serialized
		// restarts safe even for a 2-master cluster: the cluster-wide lease is
		// not released (and so the next master does not restart its etcd) until
		// this etcd is back in quorum.
		if svc == "etcd" {
			if err := waitForEtcdQuorum(executor); err != nil {
				return err
			}
		}
	}
	for i := 0; i < 90; i++ { // up to 7.5 minutes for API readiness
		if err := executor.Run("kubectl", "--kubeconfig="+adminConf, "get", "--raw=/healthz"); err == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("ca-rotation: API server health check failed")
}

// waitForEtcdQuorum blocks until the local etcd endpoint reports healthy, which
// requires a working quorum (etcdctl endpoint health performs a linearizable
// read). It uses etcd's always-present local plaintext listener so it works
// regardless of TLS state mid-rotation. If etcdctl is not installed it skips the
// check (best-effort) rather than failing.
func waitForEtcdQuorum(executor *host.Executor) error {
	if _, err := os.Stat(etcdctlPath); err != nil {
		return nil
	}
	for i := 0; i < 90; i++ { // up to ~3 minutes for quorum to re-form
		if etcdLocalHealthy(executor) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("ca-rotation: etcd did not reach quorum health after restart")
}

func etcdLocalHealthy(executor *host.Executor) bool {
	return executor.Run(etcdctlPath,
		"--endpoints=http://127.0.0.1:2379", "--command-timeout=5s",
		"endpoint", "health") == nil
}

// --- staging ---------------------------------------------------------------

// snapshotOldMaterial copies the currently live CA/leaf/SA material into the
// staging old/ directory exactly once. The snapshot is atomic (temp dir +
// rename) so a crash mid-copy never leaves a partial, mistaken "old" set.
func snapshotOldMaterial(rotationID string, role config.Role) error {
	oldDir := coord.OldDir(rotationID)
	if _, err := os.Stat(filepath.Join(oldDir, "ca.crt")); err == nil {
		return nil // already snapshotted
	}
	tmp := oldDir + ".partial"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("ca-rotation: create old staging: %w", err)
	}
	for _, name := range append([]string{"ca.crt"}, leafFiles(role)...) {
		if err := copyIfPresent(filepath.Join(certDir, name), filepath.Join(tmp, name)); err != nil {
			return err
		}
	}
	if role == config.RoleMaster {
		for _, name := range []string{"service_account.key", "service_account_private.key"} {
			if err := copyIfPresent(filepath.Join(certDir, name), filepath.Join(tmp, name)); err != nil {
				return err
			}
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "ca.crt")); err != nil {
		return fmt.Errorf("ca-rotation: live ca.crt missing, cannot snapshot old material")
	}
	return os.Rename(tmp, oldDir)
}

// generateNewMaterial fetches the new CA and signs new leaf certificates into
// the staging new/ directory. It skips work already present so resumed runs do
// not re-sign.
func generateNewMaterial(cfg config.Config, rotationID string, role config.Role) error {
	newDir := coord.NewDir(rotationID)
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return err
	}

	specs := masterCertSpecs(cfg)
	if role != config.RoleMaster {
		specs = workerCertSpecs(cfg)
	}

	if newMaterialComplete(newDir, role, specs, cfg) {
		return nil
	}

	client := magnumapi.NewClient(cfg.Shared.AuthURL, cfg.Shared.MagnumURL,
		cfg.Shared.TrusteeUserID, cfg.Shared.TrusteePassword,
		cfg.Shared.TrustID, cfg.Shared.ClusterUUID, cfg.Shared.VerifyCA)
	token, err := client.GetToken()
	if err != nil {
		return fmt.Errorf("ca-rotation: keystone auth: %w", err)
	}

	caPEM, err := client.FetchCACert(token)
	if err != nil {
		return fmt.Errorf("ca-rotation: fetch CA: %w", err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "ca.crt"), []byte(caPEM), 0o444); err != nil {
		return err
	}

	signed, err := magnumapi.GenerateAndSignCerts(client, token, specs)
	if err != nil {
		return fmt.Errorf("ca-rotation: sign certs: %w", err)
	}
	for _, s := range signed {
		if err := os.WriteFile(filepath.Join(newDir, s.Spec.Name+".key"), []byte(s.KeyPEM), 0o440); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(newDir, s.Spec.Name+".crt"), []byte(s.CertPEM), 0o444); err != nil {
			return err
		}
	}

	if role == config.RoleMaster {
		if err := os.WriteFile(filepath.Join(newDir, "service_account.key"), []byte(cfg.Shared.KubeServiceAccountKey+"\n"), 0o440); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(newDir, "service_account_private.key"), []byte(cfg.Shared.KubeServiceAccountPrivateKey+"\n"), 0o440); err != nil {
			return err
		}
	}
	return nil
}

func newMaterialComplete(newDir string, role config.Role, specs []magnumapi.CertSpec, cfg config.Config) bool {
	if !nonEmpty(filepath.Join(newDir, "ca.crt")) {
		return false
	}
	for _, s := range specs {
		if !nonEmpty(filepath.Join(newDir, s.Name+".crt")) || !nonEmpty(filepath.Join(newDir, s.Name+".key")) {
			return false
		}
	}
	if role == config.RoleMaster {
		if !nonEmpty(filepath.Join(newDir, "service_account.key")) || !nonEmpty(filepath.Join(newDir, "service_account_private.key")) {
			return false
		}
	}
	return true
}

// buildBundle writes bundle/ca.crt = new CA followed by old CA. New is first so
// single-cert readers (cluster-signing-cert-file, certutil) pick the new CA.
func buildBundle(rotationID string) error {
	bundleDir := coord.BundleDir(rotationID)
	if err := os.MkdirAll(bundleDir, 0o700); err != nil {
		return err
	}
	bundle, err := concatPEM(
		filepath.Join(coord.NewDir(rotationID), "ca.crt"),
		filepath.Join(coord.OldDir(rotationID), "ca.crt"),
	)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bundleDir, "ca.crt"), bundle, 0o444)
}

// --- cert specs ------------------------------------------------------------

func masterCertSpecs(cfg config.Config) []magnumapi.CertSpec {
	var sanIPs []string
	addIP := func(ip string) {
		if ip == "" {
			return
		}
		for _, e := range sanIPs {
			if e == ip {
				return
			}
		}
		sanIPs = append(sanIPs, ip)
	}
	addIP(cfg.ResolveNodeIP())
	addIP(cfg.Shared.KubeNodePublicIP)
	if cfg.Master != nil {
		addIP(cfg.Master.KubeAPIPublicAddress)
		addIP(cfg.Master.KubeAPIPrivateAddress)
		addIP(cfg.Master.EtcdLBVIP)
	}
	addIP("127.0.0.1")
	if cfg.Shared.PortalNetworkCIDR != "" {
		if serviceIP := computeServiceIP(cfg.Shared.PortalNetworkCIDR); serviceIP != "" {
			addIP(serviceIP)
		}
	}

	sanDNSs := []string{"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local"}
	if cfg.Master != nil && cfg.Master.MasterHostname != "" {
		sanDNSs = append(sanDNSs, cfg.Master.MasterHostname)
	}

	return []magnumapi.CertSpec{
		{Name: "server", CN: "kubernetes", SANIPs: sanIPs, SANDNSs: sanDNSs,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}},
		{Name: "kubelet", CN: fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), O: "system:nodes", SANIPs: sanIPs, SANDNSs: sanDNSs,
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}},
		{Name: "admin", CN: "admin", O: "system:masters", ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		{Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier",
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		{Name: "controller", CN: "system:kube-controller-manager", O: "system:kube-controller-manager",
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}},
		{Name: "scheduler", CN: "system:kube-scheduler", O: "system:kube-scheduler",
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}},
	}
}

func workerCertSpecs(cfg config.Config) []magnumapi.CertSpec {
	var sanIPs []string
	if ip := cfg.ResolveNodeIP(); ip != "" {
		sanIPs = append(sanIPs, ip)
	}
	sanDNSs := []string{cfg.Shared.InstanceName}
	return []magnumapi.CertSpec{
		{Name: "kubelet", CN: fmt.Sprintf("system:node:%s", cfg.Shared.InstanceName), O: "system:nodes", SANIPs: sanIPs, SANDNSs: sanDNSs,
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}},
		{Name: "proxy", CN: "system:kube-proxy", O: "system:node-proxier",
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
	}
}

// leafNames returns the leaf cert base names (no extension) for a role.
func leafNames(role config.Role) []string {
	if role == config.RoleMaster {
		return []string{"server", "kubelet", "admin", "proxy", "controller", "scheduler"}
	}
	return []string{"kubelet", "proxy"}
}

// leafFiles returns the leaf cert/key file names for a role.
func leafFiles(role config.Role) []string {
	var files []string
	for _, n := range leafNames(role) {
		files = append(files, n+".crt", n+".key")
	}
	return files
}

// --- admin kubeconfig ------------------------------------------------------

func updateAdminKubeconfig(cfg config.Config, executor *host.Executor) ([]host.Change, error) {
	apiPort := cfg.Shared.KubeAPIPort
	if apiPort == 0 {
		apiPort = 6443
	}
	readB64 := func(path string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("ca-rotation: read %s for admin kubeconfig: %w", path, err)
		}
		return base64.StdEncoding.EncodeToString(data), nil
	}
	caB64, err := readB64(certDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	adminCertB64, err := readB64(certDir + "/admin.crt")
	if err != nil {
		return nil, err
	}
	adminKeyB64, err := readB64(certDir + "/admin.key")
	if err != nil {
		return nil, err
	}

	content := fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://127.0.0.1:%d
  name: %s
contexts:
- context:
    cluster: %s
    user: admin
  name: default
current-context: default
kind: Config
preferences: {}
users:
- name: admin
  user:
    client-certificate-data: %s
    client-key-data: %s
`, caB64, apiPort, cfg.Shared.ClusterUUID, cfg.Shared.ClusterUUID, adminCertB64, adminKeyB64)

	var changes []host.Change
	change, err := executor.EnsureFile(adminConf, []byte(content), 0o600)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}
	change, err = executor.EnsureCopy(adminConf, rootKube, 0o600)
	if err != nil {
		return nil, err
	}
	if change != nil {
		changes = append(changes, *change)
	}
	return changes, nil
}

// --- workload rollout ------------------------------------------------------

func patchWorkloads(executor *host.Executor, rotationID string) ([]host.Change, []string) {
	var changes []host.Change
	var warnings []string
	// Key the rollout annotation on the new CA's content fingerprint, NOT the
	// rotation id. The roll exists so pods re-read the new trust anchor; if a
	// rotation re-runs (resume, periodic, or a repeat that mints the same CA)
	// the fingerprint is unchanged, the `kubectl patch` is a true no-op, and no
	// rollout is triggered. Only a genuine CA change rolls workloads, exactly
	// once. This stops every CA-rotate operation from churning the whole
	// cluster, which is what dragged control-plane health waits toward their
	// ceilings across repeated operations.
	stamp := caRolloutStamp(rotationID)
	annotation := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"magnum.openstack.org/ca-rotation":"%s"}}}}}`, stamp)

	for _, kind := range []string{"deployment", "daemonset"} {
		var out string
		var err error
		for attempt := 0; attempt < 6; attempt++ {
			out, err = executor.RunCapture("kubectl", "--kubeconfig="+adminConf,
				"get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
			if err == nil {
				break
			}
			time.Sleep(10 * time.Second)
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("failed to list namespaces for %s patching: %v", kind, err))
			continue
		}
		var patchFailures int
		for _, ns := range splitFields(out) {
			resources, err := executor.RunCapture("kubectl", "--kubeconfig="+adminConf,
				"get", kind, "-n", ns, "-o", "jsonpath={.items[*].metadata.name}")
			if err != nil || resources == "" {
				continue
			}
			for _, name := range splitFields(resources) {
				if err := executor.Run("kubectl", "--kubeconfig="+adminConf,
					"patch", kind, name, "-n", ns, "-p", annotation); err != nil {
					patchFailures++
				}
			}
		}
		if patchFailures > 0 {
			warnings = append(warnings, fmt.Sprintf("failed to patch %d %s(s) during CA rotation rollout", patchFailures, kind))
		}
		changes = append(changes, host.Change{Action: host.ActionUpdate, Summary: fmt.Sprintf("patch %ss with ca-rotation annotation", kind)})
	}
	return changes, warnings
}

// caRolloutStamp returns a stable identifier for the trust anchor installed by
// this rotation: the SHA-256 fingerprint of the new CA certificate. Workloads
// are re-rolled only when this value changes, so re-running the same rotation
// (or one that produced an identical CA) does not churn the cluster. If the new
// CA cannot be read it falls back to the rotation id, preserving the previous
// always-roll behaviour rather than silently skipping a needed rollout.
func caRolloutStamp(rotationID string) string {
	data, err := os.ReadFile(filepath.Join(coord.NewDir(rotationID), "ca.crt"))
	if err != nil || len(data) == 0 {
		return rotationID
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:16])
}

// --- small helpers ---------------------------------------------------------

func resolveCARotationIDs(cfg config.Config, req moduleapi.Request, markerPath string) (string, string, error) {
	rotationID := strings.TrimSpace(cfg.Trigger.CARotationID)
	latest, err := latestAppliedCARotationID(markerPath, req.PreviousCARotationID)
	if err != nil {
		return "", "", fmt.Errorf("ca-rotation: load latest applied rotation id: %w", err)
	}
	return rotationID, latest, nil
}

func latestAppliedCARotationID(markerPath, previousRotationID string) (string, error) {
	previousRotationID = strings.TrimSpace(previousRotationID)
	if markerPath == "" {
		return previousRotationID, nil
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return previousRotationID, nil
		}
		return "", err
	}
	if marker := strings.TrimSpace(string(data)); marker != "" {
		return marker, nil
	}
	return previousRotationID, nil
}

func logf(req moduleapi.Request, format string, args ...any) {
	if req.Logger != nil {
		req.Logger.Infof(format, args...)
	}
}

// atomicWrite writes content to path via a temp file + rename, returning a
// Change when the content differs from what is already on disk.
func atomicWrite(path string, content []byte, mode os.FileMode) (*host.Change, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(content) {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp := path + ".carot.tmp"
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return &host.Change{Action: host.ActionReplace, Path: path, Summary: "rotate " + filepath.Base(path)}, nil
}

func copyIfPresent(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("ca-rotation: snapshot %s: %w", src, err)
	}
	return os.WriteFile(dst, data, 0o440)
}

// concatPEM joins two PEM files, ensuring a single newline separates the
// blocks. Missing files are treated as empty.
func concatPEM(first, second string) ([]byte, error) {
	a, err := os.ReadFile(first)
	if err != nil {
		return nil, fmt.Errorf("ca-rotation: read %s: %w", first, err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("ca-rotation: read %s: %w", second, err)
		}
		b = nil
	}
	out := strings.TrimRight(string(a), "\n") + "\n"
	if len(b) > 0 {
		out += strings.TrimRight(string(b), "\n") + "\n"
	}
	return []byte(out), nil
}

func nonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

func computeServiceIP(cidr string) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip = ip.To4()
	if ip == nil {
		return ""
	}
	ip[3]++
	return ip.String()
}

func splitFields(s string) []string {
	var result []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' || s[i] == '\n' || s[i] == '\t' {
			if i > start {
				result = append(result, s[start:i])
			}
			start = i + 1
		}
	}
	return result
}

func (Module) Register(ctx *pulumi.Context, name string, heat *moduleapi.HeatParamsComponent, opts ...pulumi.ResourceOption) (pulumi.Resource, error) {
	cfg := heat.Cfg
	res := &Resource{}
	if err := ctx.RegisterComponentResource("magnum:module:CARotation", name, res, opts...); err != nil {
		return nil, err
	}
	if err := ctx.RegisterResourceOutputs(res, pulumi.Map{
		"caRotationId": pulumi.String(cfg.Trigger.CARotationID),
		"role":         pulumi.String(cfg.Role().String()),
	}); err != nil {
		return nil, err
	}
	return res, nil
}
