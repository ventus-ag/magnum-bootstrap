package certutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNeedsReconcileAcceptsMatchingPair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	spec := Spec{
		CommonName:    "system:node:worker-1",
		Organizations: []string{"system:nodes"},
		DNSNames:      []string{"worker-1"},
		IPAddresses:   []net.IP{net.ParseIP("10.0.0.10")},
		KeyUsage:      x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	if err := writeCertPair(certPath, keyPath, spec, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("writeCertPair: %v", err)
	}

	needs, reason := NeedsReconcile(certPath, keyPath, spec)
	if needs {
		t.Fatalf("expected cert to be reusable, got reason %q", reason)
	}
}

func TestNeedsReconcileDetectsExpiredCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	spec := Spec{CommonName: "admin"}
	if err := writeCertPair(certPath, keyPath, spec, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("writeCertPair: %v", err)
	}

	needs, reason := NeedsReconcile(certPath, keyPath, spec)
	if !needs || reason == "" {
		t.Fatalf("expected expired certificate to need reconcile")
	}
}

func TestNeedsReconcileDetectsKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	spec := Spec{CommonName: "system:kube-proxy", Organizations: []string{"system:node-proxier"}}
	if err := writeCertPair(certPath, keyPath, spec, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("writeCertPair: %v", err)
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(otherKey)}), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	needs, reason := NeedsReconcile(certPath, keyPath, spec)
	if !needs || reason == "" {
		t.Fatalf("expected key mismatch to need reconcile")
	}
}

func TestNeedsReconcileDetectsMissingSAN(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	actual := Spec{CommonName: "kubernetes", DNSNames: []string{"kubernetes"}}
	desired := Spec{CommonName: "kubernetes", DNSNames: []string{"kubernetes", "kubernetes.default"}}
	if err := writeCertPair(certPath, keyPath, actual, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("writeCertPair: %v", err)
	}

	needs, reason := NeedsReconcile(certPath, keyPath, desired)
	if !needs || reason == "" {
		t.Fatalf("expected SAN mismatch to need reconcile")
	}
}

func TestNeedsReconcileDetectsMissingExtendedKeyUsage(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	actual := Spec{
		CommonName:  "system:kube-scheduler",
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	desired := Spec{
		CommonName:  "system:kube-scheduler",
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	if err := writeCertPair(certPath, keyPath, actual, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("writeCertPair: %v", err)
	}

	needs, reason := NeedsReconcile(certPath, keyPath, desired)
	if !needs || reason == "" {
		t.Fatalf("expected EKU mismatch to need reconcile")
	}
}

func TestNeedsReconcileDetectsMissingKeyUsageBit(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	actual := Spec{
		CommonName: "system:kube-proxy",
		KeyUsage:   x509.KeyUsageDigitalSignature,
	}
	desired := Spec{
		CommonName: "system:kube-proxy",
		KeyUsage:   x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if err := writeCertPair(certPath, keyPath, actual, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("writeCertPair: %v", err)
	}

	needs, reason := NeedsReconcile(certPath, keyPath, desired)
	if !needs || reason == "" {
		t.Fatalf("expected key usage mismatch to need reconcile")
	}
}

func TestLeafChainBrokenFreshNodeReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	// No leaf, no CA on disk (fresh node): must not report a broken chain.
	if LeafChainBroken(filepath.Join(dir, "server.crt"), filepath.Join(dir, "ca.crt")) {
		t.Fatalf("missing leaf/CA must not report chain broken")
	}
}

func TestLeafChainBrokenMatchedPairReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	leafPath := filepath.Join(dir, "server.crt")
	caPath := filepath.Join(dir, "ca.crt")
	writeLeafSignedBy(t, ca, leafPath)
	writePEMCerts(t, caPath, ca.certDER)

	if LeafChainBroken(leafPath, caPath) {
		t.Fatalf("leaf signed by the on-disk CA must not report chain broken")
	}
}

func TestLeafChainBrokenForeignCAReturnsTrue(t *testing.T) {
	dir := t.TempDir()
	signer := newCA(t)
	other := newCA(t)
	leafPath := filepath.Join(dir, "server.crt")
	caPath := filepath.Join(dir, "ca.crt")
	writeLeafSignedBy(t, signer, leafPath)
	writePEMCerts(t, caPath, other.certDER) // CA swapped out from under the leaf

	if !LeafChainBroken(leafPath, caPath) {
		t.Fatalf("leaf not signed by the on-disk CA must report chain broken")
	}
}

func TestLeafChainBrokenBundleReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	newer := newCA(t)
	older := newCA(t)
	leafPath := filepath.Join(dir, "server.crt")
	caPath := filepath.Join(dir, "ca.crt")
	writeLeafSignedBy(t, newer, leafPath)
	// ca.crt is a new+old bundle (mid dual-CA rotation): leaf chains to one member.
	writePEMCerts(t, caPath, newer.certDER, older.certDER)

	if LeafChainBroken(leafPath, caPath) {
		t.Fatalf("leaf chaining to one CA in a bundle must not report chain broken")
	}
}

func TestLeafChainBrokenUnparseableCAReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	leafPath := filepath.Join(dir, "server.crt")
	caPath := filepath.Join(dir, "ca.crt")
	writeLeafSignedBy(t, ca, leafPath)
	if err := os.WriteFile(caPath, []byte("not a pem file"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// An unreadable CA is not "chain broken" here — the CA-expiry/missing checks
	// own that case, and forcing a refetch on garbage could clobber real material.
	if LeafChainBroken(leafPath, caPath) {
		t.Fatalf("unparseable CA must not report chain broken")
	}
}

func TestLeafChainBrokenLegacyNonCASignerReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	// A signer cert that does NOT advertise IsCA/BasicConstraints (legacy Magnum
	// material). CheckSignatureFrom rejects it as a parent, so LeafChainBroken
	// must fall back to a bare signature check and still see the leaf as chained.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "legacy-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		// deliberately no IsCA / BasicConstraintsValid
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate signer: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("ParseCertificate signer: %v", err)
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey leaf: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate leaf: %v", err)
	}

	caPath := filepath.Join(dir, "ca.crt")
	leafPath := filepath.Join(dir, "server.crt")
	writePEMCerts(t, caPath, caDER)
	writePEMCerts(t, leafPath, leafDER)

	if LeafChainBroken(leafPath, caPath) {
		t.Fatalf("leaf signed by a non-CA-flagged signer must not report chain broken (fallback path)")
	}
}

func TestCertPEMNeedsRefresh(t *testing.T) {
	ca := newCA(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certDER})
	if needs, why := CertPEMNeedsRefresh(caPEM); needs {
		t.Fatalf("valid CA PEM must not need refresh; got %q", why)
	}
	if needs, _ := CertPEMNeedsRefresh([]byte("garbage")); !needs {
		t.Fatalf("unparseable PEM must need refresh")
	}
}

func TestCertPEMMatchesKeyFile(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certDER})

	keyPath := filepath.Join(dir, "ca.key")
	writeKeyPEM(t, keyPath, ca.key)
	if !CertPEMMatchesKeyFile(caPEM, keyPath) {
		t.Fatalf("CA PEM must match its own key file")
	}

	otherKeyPath := filepath.Join(dir, "other.key")
	writeKeyPEM(t, otherKeyPath, newCA(t).key)
	if CertPEMMatchesKeyFile(caPEM, otherKeyPath) {
		t.Fatalf("CA PEM must not match a foreign key file")
	}
	if CertPEMMatchesKeyFile(caPEM, filepath.Join(dir, "missing.key")) {
		t.Fatalf("missing key file must not match")
	}
}

func writeKeyPEM(t *testing.T, path string, key *rsa.PrivateKey) {
	t.Helper()
	der := x509.MarshalPKCS1PrivateKey(key)
	buf := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, buf, 0o400); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

type testCA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *rsa.PrivateKey
}

func newCA(t *testing.T) *testCA {
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

func writeLeafSignedBy(t *testing.T, ca *testCA, leafPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey leaf: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1),
		Subject:      pkix.Name{CommonName: "kubernetes"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("CreateCertificate leaf: %v", err)
	}
	writePEMCerts(t, leafPath, der)
}

func writePEMCerts(t *testing.T, path string, ders ...[]byte) {
	t.Helper()
	var buf []byte
	for _, der := range ders {
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func writeCertPair(certPath, keyPath string, spec Spec, notBefore, notAfter time.Time) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   spec.CommonName,
			Organization: spec.Organizations,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              spec.KeyUsage,
		ExtKeyUsage:           spec.ExtKeyUsage,
		BasicConstraintsValid: true,
		DNSNames:              spec.DNSNames,
		IPAddresses:           spec.IPAddresses,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
}

func TestLeafNotSignedByCurrentCA(t *testing.T) {
	dir := t.TempDir()
	newer := newCA(t) // current CA: ca.key pairs with this
	older := newCA(t) // pre-rotation CA, still retained in the trust bundle
	caPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	// Post-prepare, pre-cutover state: bundle trusts both, ca.key is the new CA.
	writePEMCerts(t, caPath, newer.certDER, older.certDER)
	writeKeyPEM(t, keyPath, newer.key)

	// Leaf still signed by the OLD CA (cutover never ran) -> stale, must be flagged.
	oldLeaf := filepath.Join(dir, "old.crt")
	writeLeafSignedBy(t, older, oldLeaf)
	if !LeafNotSignedByCurrentCA(oldLeaf, caPath, keyPath) {
		t.Fatalf("old-CA-signed leaf must be flagged when ca.key is the new CA")
	}

	// Leaf signed by the current (new) CA -> healthy, must not be flagged.
	newLeaf := filepath.Join(dir, "new.crt")
	writeLeafSignedBy(t, newer, newLeaf)
	if LeafNotSignedByCurrentCA(newLeaf, caPath, keyPath) {
		t.Fatalf("current-CA-signed leaf must not be flagged")
	}

	// No ca.key (a worker) -> no-op (false), never churns worker leaves.
	if LeafNotSignedByCurrentCA(oldLeaf, caPath, filepath.Join(dir, "missing.key")) {
		t.Fatalf("missing ca.key must yield false")
	}

	// ca.key pairs with no CA in the bundle -> conservative false.
	foreignKey := filepath.Join(dir, "foreign.key")
	writeKeyPEM(t, foreignKey, newCA(t).key)
	if LeafNotSignedByCurrentCA(oldLeaf, caPath, foreignKey) {
		t.Fatalf("ca.key matching no bundle CA must yield false (cannot identify current CA)")
	}
}
