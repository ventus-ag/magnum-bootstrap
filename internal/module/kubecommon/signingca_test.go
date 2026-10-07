package kubecommon

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

func caPair(t *testing.T) (certPEM, keyPEM []byte) {
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
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func countCerts(pemData []byte) int {
	n := 0
	rest := pemData
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return n
		}
		if block.Type == "CERTIFICATE" {
			n++
		}
	}
}

func withSigningPath(t *testing.T, dir string) {
	t.Helper()
	restore := signingCACertPath
	signingCACertPath = filepath.Join(dir, "ca-signing.crt")
	t.Cleanup(func() { signingCACertPath = restore })
}

func TestEnsureSigningCAPicksTheKeysCertificateFromABundle(t *testing.T) {
	dir := t.TempDir()
	withSigningPath(t, dir)

	newCert, newKey := caPair(t)
	oldCert, _ := caPair(t)
	bundlePath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(bundlePath, append(append([]byte{}, newCert...), oldCert...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, newKey, 0o600); err != nil {
		t.Fatal(err)
	}

	executor := host.NewExecutor(true, nil)
	changes, warning, err := EnsureSigningCA(executor, bundlePath, keyPath)
	if err != nil || warning != "" {
		t.Fatalf("EnsureSigningCA: %v / %q", err, warning)
	}
	if len(changes) == 0 {
		t.Fatal("expected the signing certificate to be written")
	}
	written, err := os.ReadFile(signingCACertPath)
	if err != nil {
		t.Fatal(err)
	}
	// One certificate only: kube-controller-manager refuses a bundle outright.
	if n := countCerts(written); n != 1 {
		t.Fatalf("signing file holds %d certificates; want exactly 1", n)
	}
	if string(written) != string(newCert) {
		t.Fatal("signing file must hold the certificate that pairs with ca.key")
	}

	// Idempotent: a second pass changes nothing.
	changes, _, err = EnsureSigningCA(executor, bundlePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("second pass reported %d change(s); want none", len(changes))
	}
}

func TestEnsureSigningCAWarnsOnUnpairedBundle(t *testing.T) {
	dir := t.TempDir()
	withSigningPath(t, dir)

	cert, _ := caPair(t)
	_, strayKey := caPair(t)
	bundlePath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(bundlePath, cert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, strayKey, 0o600); err != nil {
		t.Fatal(err)
	}

	_, warning, err := EnsureSigningCA(host.NewExecutor(true, nil), bundlePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warning, "pairs with") {
		t.Fatalf("an unpaired CA must be reported, got %q", warning)
	}
	if n := countCerts(mustRead(t, signingCACertPath)); n != 1 {
		t.Fatal("the first certificate must still be installed, preserving the previous behaviour")
	}
}

func TestEnsureSigningCANoopWithoutKey(t *testing.T) {
	dir := t.TempDir()
	withSigningPath(t, dir)
	cert, _ := caPair(t)
	bundlePath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(bundlePath, cert, 0o644); err != nil {
		t.Fatal(err)
	}

	changes, warning, err := EnsureSigningCA(host.NewExecutor(true, nil), bundlePath, filepath.Join(dir, "absent.key"))
	if err != nil || warning != "" || len(changes) != 0 {
		t.Fatalf("a node without ca.key must be untouched: %v / %q / %v", err, warning, changes)
	}
	if _, err := os.Stat(signingCACertPath); !os.IsNotExist(err) {
		t.Fatal("no signing file should have been created")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
