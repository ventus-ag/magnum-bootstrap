package magnum

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestCA(t *testing.T, dir string) (certPath, keyPath string, caCert *x509.Certificate, caKey *rsa.PrivateKey) {
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
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	certPath = filepath.Join(dir, "ca.crt")
	keyPath = filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("WriteFile ca.crt: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatalf("WriteFile ca.key: %v", err)
	}
	return certPath, keyPath, cert, key
}

func parseTestCertPEM(t *testing.T, pemStr string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		t.Fatalf("no PEM block in signed cert")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

func TestGenerateLocalSignedCerts(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caCert, _ := writeTestCA(t, dir)

	specs := []CertSpec{
		{
			Name:        "kubelet",
			CN:          "system:node:test-node-0",
			O:           "system:nodes",
			SANIPs:      []string{"10.0.0.7", "not-an-ip"},
			SANDNSs:     []string{"test-node-0"},
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		},
		{
			Name:        "proxy",
			CN:          "system:kube-proxy",
			O:           "system:node-proxier",
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
	}

	signed, err := GenerateLocalSignedCerts(certPath, keyPath, specs)
	if err != nil {
		t.Fatalf("GenerateLocalSignedCerts: %v", err)
	}
	if len(signed) != len(specs) {
		t.Fatalf("want %d signed certs, got %d", len(specs), len(signed))
	}

	for i, s := range signed {
		if s.Spec.Name != specs[i].Name {
			t.Fatalf("result order not preserved: want %s got %s", specs[i].Name, s.Spec.Name)
		}
		cert := parseTestCertPEM(t, s.CertPEM)
		if err := cert.CheckSignatureFrom(caCert); err != nil {
			t.Fatalf("%s: not signed by local CA: %v", s.Spec.Name, err)
		}
		if cert.Subject.CommonName != specs[i].CN {
			t.Fatalf("%s: CN mismatch: %q", s.Spec.Name, cert.Subject.CommonName)
		}
		if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != specs[i].O {
			t.Fatalf("%s: O mismatch: %v", s.Spec.Name, cert.Subject.Organization)
		}
		for _, eku := range specs[i].ExtKeyUsage {
			found := false
			for _, have := range cert.ExtKeyUsage {
				if have == eku {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: missing EKU %d", s.Spec.Name, eku)
			}
		}
		if cert.IsCA {
			t.Fatalf("%s: leaf must not be a CA", s.Spec.Name)
		}
		now := time.Now()
		if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			t.Fatalf("%s: outside validity window", s.Spec.Name)
		}

		// Key pairs with cert.
		block, _ := pem.Decode([]byte(s.KeyPEM))
		if block == nil {
			t.Fatalf("%s: no key PEM", s.Spec.Name)
		}
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("%s: parse key: %v", s.Spec.Name, err)
		}
		if !key.PublicKey.Equal(cert.PublicKey) {
			t.Fatalf("%s: key does not pair with cert", s.Spec.Name)
		}
	}

	// SANs on the kubelet cert (invalid IP silently dropped).
	kubelet := parseTestCertPEM(t, signed[0].CertPEM)
	if len(kubelet.IPAddresses) != 1 || kubelet.IPAddresses[0].String() != "10.0.0.7" {
		t.Fatalf("kubelet IP SANs wrong: %v", kubelet.IPAddresses)
	}
	if len(kubelet.DNSNames) != 1 || kubelet.DNSNames[0] != "test-node-0" {
		t.Fatalf("kubelet DNS SANs wrong: %v", kubelet.DNSNames)
	}
}

func TestGenerateLocalSignedCertsMissingCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := GenerateLocalSignedCerts(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), []CertSpec{{Name: "kubelet", CN: "x"}}); err == nil {
		t.Fatalf("missing CA material must error")
	}
	if out, err := GenerateLocalSignedCerts(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), nil); err != nil || out != nil {
		t.Fatalf("empty spec list must be a no-op")
	}
}
