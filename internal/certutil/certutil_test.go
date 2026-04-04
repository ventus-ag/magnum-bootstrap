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
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
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
