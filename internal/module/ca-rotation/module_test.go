package carotation

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	coord "github.com/ventus-ag/magnum-bootstrap/internal/carotation"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
	"github.com/ventus-ag/magnum-bootstrap/internal/moduleapi"
)

func workerCfg() config.Config {
	return config.Config{Shared: config.SharedConfig{NodegroupRole: "worker"}}
}

func TestHardRotateRequestedForcedByEnv(t *testing.T) {
	t.Setenv(hardRotateEnv, "true")
	hard, why := hardRotateRequested(workerCfg())
	if !hard {
		t.Fatalf("env force must request hard rotate")
	}
	if why == "" {
		t.Fatalf("expected a reason")
	}
}

func TestHardRotateRequestedHealthyCertsReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	restore := certDir
	certDir = dir
	defer func() { certDir = restore }()

	ca := newTestCA(t)
	writeCertPEM(t, filepath.Join(dir, "ca.crt"), ca.certDER)
	for _, name := range leafNames(config.RoleWorker) { // kubelet, proxy
		signLeaf(t, ca, filepath.Join(dir, name+".crt"))
	}

	if hard, why := hardRotateRequested(workerCfg()); hard {
		t.Fatalf("healthy certs must not request hard rotate; got %q", why)
	}
}

func TestHardRotateRequestedBrokenChainReturnsTrue(t *testing.T) {
	dir := t.TempDir()
	restore := certDir
	certDir = dir
	defer func() { certDir = restore }()

	ca := newTestCA(t)
	other := newTestCA(t)
	writeCertPEM(t, filepath.Join(dir, "ca.crt"), ca.certDER)
	signLeaf(t, ca, filepath.Join(dir, "kubelet.crt"))
	signLeaf(t, other, filepath.Join(dir, "proxy.crt")) // signed by a foreign CA

	hard, why := hardRotateRequested(workerCfg())
	if !hard {
		t.Fatalf("a leaf not chaining to the live CA must request hard rotate")
	}
	if why == "" {
		t.Fatalf("expected a reason")
	}
}

func TestHardRotateRequestedExpiredLeafReturnsTrue(t *testing.T) {
	dir := t.TempDir()
	restore := certDir
	certDir = dir
	defer func() { certDir = restore }()

	ca := newTestCA(t)
	writeCertPEM(t, filepath.Join(dir, "ca.crt"), ca.certDER)
	signLeaf(t, ca, filepath.Join(dir, "kubelet.crt"))
	signLeafExpired(t, ca, filepath.Join(dir, "proxy.crt"))

	if hard, _ := hardRotateRequested(workerCfg()); !hard {
		t.Fatalf("an expired leaf must request hard rotate")
	}
}

func TestWriteHardFilesMasterEndState(t *testing.T) {
	dir := t.TempDir()
	restoreCert, restoreEtcd := certDir, etcdCertDir
	certDir = filepath.Join(dir, "kube")
	etcdCertDir = filepath.Join(dir, "etcd")
	defer func() { certDir, etcdCertDir = restoreCert, restoreEtcd }()
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(etcdCertDir, 0o755); err != nil {
		t.Fatal(err)
	}
	restoreBase := coord.SetBaseDir(filepath.Join(dir, "staging"))
	defer restoreBase()

	const rid = "rot-hard-1"
	ca := newTestCA(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certDER})
	seedHardStaging(t, rid, caPEM)

	cfg := config.Config{Shared: config.SharedConfig{
		NodegroupRole: "master",
		InstanceName:  "test-master-0",
		CAKey:         string(keyToPEM(ca.key)), // pairs with the new CA
	}}
	r := &runner{cfg: cfg, rotationID: rid, role: config.RoleMaster, isMaster: true,
		nodeName: cfg.Shared.InstanceName, executor: newTestExecutor(t)}

	if err := r.writeHardFiles(); err != nil {
		t.Fatalf("writeHardFiles: %v", err)
	}

	assertFile(t, filepath.Join(certDir, "ca.crt"), string(caPEM))
	assertFile(t, filepath.Join(etcdCertDir, "ca.crt"), string(caPEM))
	assertFile(t, filepath.Join(certDir, "server.crt"), "SERVER-CRT")
	assertFile(t, filepath.Join(etcdCertDir, "server.crt"), "SERVER-CRT") // mirrored to etcd
	assertFile(t, filepath.Join(certDir, "service_account_private.key"), "NEW-SA-PRIV")
	// SA verify = new+old bundle so running pods' tokens keep validating.
	assertFile(t, filepath.Join(certDir, "service_account.key"), "NEW-SA\nOLD-SA\n")
	// ca.key installed from CA_KEY (it pairs with the new CA).
	assertFile(t, filepath.Join(certDir, "ca.key"), string(keyToPEM(ca.key))+"\n")
}

func TestWriteHardFilesRejectsMismatchedCAKey(t *testing.T) {
	dir := t.TempDir()
	restoreCert, restoreEtcd := certDir, etcdCertDir
	certDir = filepath.Join(dir, "kube")
	etcdCertDir = filepath.Join(dir, "etcd")
	defer func() { certDir, etcdCertDir = restoreCert, restoreEtcd }()
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatal(err)
	}
	restoreBase := coord.SetBaseDir(filepath.Join(dir, "staging"))
	defer restoreBase()

	const rid = "rot-hard-mismatch"
	ca := newTestCA(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certDER})
	seedHardStaging(t, rid, caPEM)

	cfg := config.Config{Shared: config.SharedConfig{
		NodegroupRole: "master", InstanceName: "test-master-0",
		CAKey: string(keyToPEM(newTestCA(t).key)), // a DIFFERENT key — does not pair
	}}
	r := &runner{cfg: cfg, rotationID: rid, role: config.RoleMaster, isMaster: true,
		nodeName: cfg.Shared.InstanceName, executor: newTestExecutor(t)}

	err := r.writeHardFiles()
	if err == nil {
		t.Fatalf("expected error when CA_KEY does not pair with the new CA")
	}
	// Must fail BEFORE mutating live material (no split pair on disk).
	if _, statErr := os.Stat(filepath.Join(certDir, "ca.crt")); statErr == nil {
		t.Fatalf("ca.crt must not have been written on a mismatched-key hard rotate")
	}
}

func TestHardRotateInProgress(t *testing.T) {
	dir := t.TempDir()
	restoreBase := coord.SetBaseDir(dir)
	defer restoreBase()

	const rid = "rot-bc"
	if in, err := hardRotateInProgress(rid); err != nil || in {
		t.Fatalf("no state → not in progress; got in=%v err=%v", in, err)
	}
	// Breadcrumb before cutover: Hard=true, Phase="" → in progress.
	if err := coord.SaveState(coord.State{RotationID: rid, Hard: true}); err != nil {
		t.Fatal(err)
	}
	if in, err := hardRotateInProgress(rid); err != nil || !in {
		t.Fatalf("hard breadcrumb → in progress; got in=%v err=%v", in, err)
	}
	// After completion (PhaseDone) → not in progress.
	if err := coord.SaveState(coord.State{RotationID: rid, Hard: true, Phase: coord.PhaseDone}); err != nil {
		t.Fatal(err)
	}
	if in, err := hardRotateInProgress(rid); err != nil || in {
		t.Fatalf("done → not in progress; got in=%v err=%v", in, err)
	}
}

// seedHardStaging pre-populates the NewDir/OldDir staging that writeHardFiles
// reads, so the test does not need a live Barbican.
func seedHardStaging(t *testing.T, rid string, caPEM []byte) {
	t.Helper()
	newDir, oldDir := coord.NewDir(rid), coord.OldDir(rid)
	for _, d := range []string{newDir, oldDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o440); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	write(filepath.Join(newDir, "ca.crt"), string(caPEM))
	for _, name := range leafNames(config.RoleMaster) {
		write(filepath.Join(newDir, name+".crt"), "SERVER-CRT")
		write(filepath.Join(newDir, name+".key"), "SERVER-KEY")
	}
	write(filepath.Join(newDir, "service_account.key"), "NEW-SA")
	write(filepath.Join(newDir, "service_account_private.key"), "NEW-SA-PRIV")
	write(filepath.Join(oldDir, "service_account.key"), "OLD-SA")
}

func keyToPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func newTestExecutor(t *testing.T) *host.Executor {
	t.Helper()
	logger, err := logging.New(filepath.Join(t.TempDir(), "log"), io.Discard, false)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	return host.NewExecutor(true, logger)
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

// --- test cert helpers -----------------------------------------------------

type testCA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *rsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kubernetes"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate CA: %v", err)
	}
	return &testCA{cert: cert, certDER: der, key: key}
}

func signLeaf(t *testing.T, ca *testCA, path string) {
	t.Helper()
	signLeafWindow(t, ca, path, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

func signLeafExpired(t *testing.T, ca *testCA, path string) {
	t.Helper()
	signLeafWindow(t, ca, path, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
}

func signLeafWindow(t *testing.T, ca *testCA, path string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey leaf: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("CreateCertificate leaf: %v", err)
	}
	writeCertPEM(t, path, der)
}

func writeCertPEM(t *testing.T, path string, ders ...[]byte) {
	t.Helper()
	var buf []byte
	for _, der := range ders {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(path, buf, 0o444); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestLatestAppliedCARotationIDPrefersMarkerFile(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "last_ca_rotation_id")
	if err := os.WriteFile(marker, []byte("marker-id\n"), 0o644); err != nil {
		t.Fatalf("failed to write marker file: %v", err)
	}

	got, err := latestAppliedCARotationID(marker, "state-id")
	if err != nil {
		t.Fatalf("latestAppliedCARotationID returned error: %v", err)
	}
	if got != "marker-id" {
		t.Fatalf("expected marker id, got %q", got)
	}
}

func TestLatestAppliedCARotationIDFallsBackToState(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "missing")

	got, err := latestAppliedCARotationID(marker, "state-id")
	if err != nil {
		t.Fatalf("latestAppliedCARotationID returned error: %v", err)
	}
	if got != "state-id" {
		t.Fatalf("expected state id fallback, got %q", got)
	}
}

func TestModuleRunSkipsWhenNoRotationActive(t *testing.T) {
	// With no CA_ROTATION_ID set, there is no active rotation, so the module is a
	// no-op. (Resize/upgrade no longer suppress rotation — the IS_RESIZE/IS_UPGRADE
	// flags were removed; rotation is gated purely on the rotation token.)
	cfg := config.Config{
		Trigger: config.TriggerConfig{
			CARotationID: "",
		},
	}

	res, err := (Module{}).Run(context.Background(), cfg, moduleapi.Request{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no changes when no rotation is active, got %d", len(res.Changes))
	}
}

func TestModuleRunSkipsWhenRotationAlreadyAppliedFromState(t *testing.T) {
	cfg := config.Config{
		Trigger: config.TriggerConfig{
			CARotationID: "rotate-123",
		},
	}

	res, err := (Module{}).Run(context.Background(), cfg, moduleapi.Request{
		PreviousCARotationID: "rotate-123",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("expected no changes for already applied ca rotation, got %d", len(res.Changes))
	}
}

// stageRotation records rotationID at phase with newCA staged as its new CA.
func stageRotation(t *testing.T, rotationID string, phase coord.Phase, newCA *testCA) {
	t.Helper()
	if err := coord.SaveState(coord.State{RotationID: rotationID, Phase: phase}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(coord.NewDir(rotationID), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCertPEM(t, filepath.Join(coord.NewDir(rotationID), "ca.crt"), newCA.certDER)
}

func writeLiveCA(t *testing.T, dir string, cas ...*testCA) {
	t.Helper()
	ders := make([][]byte, 0, len(cas))
	for _, ca := range cas {
		ders = append(ders, ca.certDER)
	}
	writeCertPEM(t, filepath.Join(dir, "ca.crt"), ders...)
}

func TestResumableRotationIDFromState(t *testing.T) {
	defer coord.SetBaseDir(t.TempDir())()
	dir := t.TempDir()
	restore := certDir
	certDir = dir
	defer func() { certDir = restore }()

	old, next := newTestCA(t), newTestCA(t)
	stageRotation(t, "rot-1", coord.PhasePrepare, next)
	writeLiveCA(t, dir, next, old)

	id, why := resumableRotationID("")
	if id != "rot-1" || why == "" {
		t.Fatalf("resumableRotationID = %q (%q); want rot-1 with a reason", id, why)
	}
	if id, _ := resumableRotationID("rot-1"); id != "" {
		t.Fatalf("an applied rotation must not resume, got %q", id)
	}
}

func TestResumableRotationIDIgnoresSupersededRotation(t *testing.T) {
	// rot-1 was abandoned at prepare, then rot-2 completed. rot-1's state still
	// reads unfinished, but the live CA no longer trusts its new CA: reviving it
	// would reset the cluster's coordination and re-install a stale bundle.
	defer coord.SetBaseDir(t.TempDir())()
	dir := t.TempDir()
	restore := certDir
	certDir = dir
	defer func() { certDir = restore }()

	abandoned, current := newTestCA(t), newTestCA(t)
	stageRotation(t, "rot-1", coord.PhasePrepare, abandoned)
	writeLiveCA(t, dir, current)

	if id, why := resumableRotationID("rot-2"); id != "" {
		t.Fatalf("a superseded rotation must not resume, got %q (%q)", id, why)
	}
}

func TestResumableRotationIDFromStagedBundle(t *testing.T) {
	// The node that was DRIVING the rotation is the one an interrupting stack
	// update kills first, often before any phase is recorded. Its only trace is
	// that the live CA is the staged dual-CA bundle — which must still resume.
	defer coord.SetBaseDir(t.TempDir())()
	dir := t.TempDir()
	restore := certDir
	certDir = dir
	defer func() { certDir = restore }()

	ca := newTestCA(t)
	next := newTestCA(t)
	bundle := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: next.certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certDER})...)

	if err := os.MkdirAll(coord.BundleDir("rot-9"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coord.BundleDir("rot-9"), "ca.crt"), bundle, 0o444); err != nil {
		t.Fatal(err)
	}

	// Live CA is still the old single cert: prepare never landed here.
	writeCertPEM(t, filepath.Join(dir, "ca.crt"), ca.certDER)
	if id, _ := resumableRotationID(""); id != "" {
		t.Fatalf("a staged-but-not-installed rotation must not resume, got %q", id)
	}

	// Live CA is the staged bundle: prepare landed, the run died after it.
	_ = os.Remove(filepath.Join(dir, "ca.crt"))
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), bundle, 0o444); err != nil {
		t.Fatal(err)
	}
	if id, why := resumableRotationID(""); id != "rot-9" || why == "" {
		t.Fatalf("resumableRotationID = %q (%q); want rot-9", id, why)
	}
}
