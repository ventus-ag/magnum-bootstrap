package certutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalCAUsableMatchingPair(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	writePEMCerts(t, certPath, ca.certDER)
	writeKeyPEM(t, keyPath, ca.key)

	if ok, why := LocalCAUsable(certPath, keyPath); !ok {
		t.Fatalf("valid matching CA pair must be usable; got %q", why)
	}
}

func TestLocalCAUsableRejectsForeignKey(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	writePEMCerts(t, certPath, ca.certDER)
	writeKeyPEM(t, keyPath, newCA(t).key)

	if ok, _ := LocalCAUsable(certPath, keyPath); ok {
		t.Fatalf("CA cert with a foreign key must not be usable")
	}
}

func TestLocalCAUsableRejectsMissingKey(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath := filepath.Join(dir, "ca.crt")
	writePEMCerts(t, certPath, ca.certDER)

	if ok, _ := LocalCAUsable(certPath, filepath.Join(dir, "missing.key")); ok {
		t.Fatalf("missing CA key must not be usable")
	}
}

func TestLocalCAUsableRejectsMissingOrGarbageCert(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	keyPath := filepath.Join(dir, "ca.key")
	writeKeyPEM(t, keyPath, ca.key)

	if ok, _ := LocalCAUsable(filepath.Join(dir, "missing.crt"), keyPath); ok {
		t.Fatalf("missing CA cert must not be usable")
	}
	certPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(certPath, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if ok, _ := LocalCAUsable(certPath, keyPath); ok {
		t.Fatalf("garbage CA cert must not be usable")
	}
}

func newExpiredCA(t *testing.T) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kubernetes"},
		NotBefore:             time.Now().Add(-2 * 365 * 24 * time.Hour),
		NotAfter:              time.Now().Add(-time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate expired CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate expired CA: %v", err)
	}
	return &testCA{cert: cert, certDER: der, key: key}
}

func TestRenewExpiredCA(t *testing.T) {
	dir := t.TempDir()
	old := newExpiredCA(t)
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	writePEMCerts(t, certPath, old.certDER)
	writeKeyPEM(t, keyPath, old.key)

	// A leaf the expired CA signed earlier.
	leafPath := filepath.Join(dir, "leaf.crt")
	writeLeafSignedBy(t, old, leafPath)

	renewedPEM, err := RenewExpiredCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("RenewExpiredCA: %v", err)
	}
	block, _ := pem.Decode(renewedPEM)
	if block == nil {
		t.Fatalf("renewed CA is not PEM")
	}
	renewed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse renewed CA: %v", err)
	}

	now := time.Now()
	if now.Before(renewed.NotBefore) || now.After(renewed.NotAfter) {
		t.Fatalf("renewed CA must be currently valid")
	}
	if renewed.Subject.CommonName != old.cert.Subject.CommonName {
		t.Fatalf("renewed CA must keep the subject; got %q", renewed.Subject.CommonName)
	}
	if !renewed.PublicKey.(*rsa.PublicKey).Equal(&old.key.PublicKey) {
		t.Fatalf("renewed CA must keep the public key")
	}
	if !renewed.IsCA {
		t.Fatalf("renewed CA must be a CA")
	}
	// The renewed CA still pairs with the key and validates old leaves.
	if !CertPEMMatchesKeyFile(renewedPEM, keyPath) {
		t.Fatalf("renewed CA must pair with the original key")
	}
	if err := os.WriteFile(certPath, renewedPEM, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if LeafChainBroken(leafPath, certPath) {
		t.Fatalf("old leaves must still chain to the renewed CA")
	}
	if ok, why := LocalCAUsable(certPath, keyPath); !ok {
		t.Fatalf("renewed CA pair must be usable; got %q", why)
	}
}

func TestRenewExpiredCARefusesValidCA(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	writePEMCerts(t, certPath, ca.certDER)
	writeKeyPEM(t, keyPath, ca.key)

	if _, err := RenewExpiredCA(certPath, keyPath); err == nil {
		t.Fatalf("must refuse to renew a CA that is not expired")
	}
}

func TestRenewExpiredCARefusesForeignKey(t *testing.T) {
	dir := t.TempDir()
	old := newExpiredCA(t)
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	writePEMCerts(t, certPath, old.certDER)
	writeKeyPEM(t, keyPath, newCA(t).key)

	if _, err := RenewExpiredCA(certPath, keyPath); err == nil {
		t.Fatalf("must refuse to renew with a non-pair key")
	}
	if _, err := RenewExpiredCA(filepath.Join(dir, "missing.crt"), keyPath); err == nil {
		t.Fatalf("must refuse with a missing cert")
	}
}

func TestLeafPEMSignedByCAFile(t *testing.T) {
	dir := t.TempDir()
	signer := newCA(t)
	other := newCA(t)
	caPath := filepath.Join(dir, "ca.crt")
	writePEMCerts(t, caPath, signer.certDER)

	leafPath := filepath.Join(dir, "leaf.crt")
	writeLeafSignedBy(t, signer, leafPath)
	leafPEM, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !LeafPEMSignedByCAFile(leafPEM, caPath) {
		t.Fatalf("leaf signed by on-disk CA must chain")
	}

	otherCAPath := filepath.Join(dir, "other-ca.crt")
	writePEMCerts(t, otherCAPath, other.certDER)
	if LeafPEMSignedByCAFile(leafPEM, otherCAPath) {
		t.Fatalf("leaf signed by a foreign CA must not chain")
	}

	// Missing CA file: nothing to verify against, treat signed material as canonical.
	if !LeafPEMSignedByCAFile(leafPEM, filepath.Join(dir, "missing-ca.crt")) {
		t.Fatalf("missing CA file must not veto signed material")
	}
	if LeafPEMSignedByCAFile([]byte("garbage"), caPath) {
		t.Fatalf("unparseable leaf must not pass")
	}
}

func TestCAFromKubeconfig(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.certDER})

	kubeconfig := "apiVersion: v1\nclusters:\n- cluster:\n    certificate-authority-data: " +
		base64.StdEncoding.EncodeToString(caPEM) +
		"\n    server: https://10.0.0.1:6443\n  name: test\n"
	path := filepath.Join(dir, "admin.conf")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := CAFromKubeconfig(path)
	if err != nil {
		t.Fatalf("CAFromKubeconfig: %v", err)
	}
	if string(got) != string(caPEM) {
		t.Fatalf("extracted CA does not round-trip")
	}
	// The extracted CA must pair with the CA key (the adoption criterion).
	keyPath := filepath.Join(dir, "ca.key")
	writeKeyPEM(t, keyPath, ca.key)
	if !CertPEMMatchesKeyFile(got, keyPath) {
		t.Fatalf("extracted CA must match its key")
	}
}

func TestCAFromKubeconfigNoInlineCA(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kubelet.conf")
	// File-referenced CA (no inline data) must yield an error, not garbage.
	if err := os.WriteFile(path, []byte("clusters:\n- cluster:\n    certificate-authority: /etc/kubernetes/certs/ca.crt\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := CAFromKubeconfig(path); err == nil {
		t.Fatalf("kubeconfig without certificate-authority-data must error")
	}
	if _, err := CAFromKubeconfig(filepath.Join(dir, "missing.conf")); err == nil {
		t.Fatalf("missing kubeconfig must error")
	}
}
