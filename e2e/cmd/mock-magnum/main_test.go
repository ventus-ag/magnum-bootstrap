package main

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ventus-ag/magnum-bootstrap/internal/magnum"
)

// TestMockServesRealMagnumClient drives the mock through the production
// magnum.Client (the same code the reconciler runs) and verifies the full
// token -> CA-fetch -> sign flow, ending with a real chain verification of a
// signed leaf against the served CA. If this passes, the mock is a faithful
// stand-in for Keystone+Magnum for cert generation.
func TestMockServesRealMagnumClient(t *testing.T) {
	dir := t.TempDir()
	srv, err := newServer(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"), 3650, false)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(http.HandlerFunc(srv.route))
	defer ts.Close()

	client := magnum.NewClient(
		ts.URL+"/v3", ts.URL+"/v1",
		"trustee-uid", "trustee-pwd", "trust-id",
		"cluster-uuid-1234", false,
	)

	token, err := client.GetToken()
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if token == "" {
		t.Fatal("GetToken returned empty token")
	}

	caPEM, err := client.FetchCACert(token)
	if err != nil {
		t.Fatalf("FetchCACert: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM([]byte(caPEM)) {
		t.Fatal("FetchCACert returned a PEM that is not a valid CA")
	}

	specs := []magnum.CertSpec{
		{
			Name:        "server",
			CN:          "kube-apiserver",
			SANIPs:      []string{"10.0.0.10", "10.254.0.1"},
			SANDNSs:     []string{"kubernetes", "kubernetes.default"},
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		},
		{
			Name:        "admin",
			CN:          "admin",
			O:           "system:masters",
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
	}

	signed, err := magnum.GenerateAndSignCerts(client, token, specs)
	if err != nil {
		t.Fatalf("GenerateAndSignCerts: %v", err)
	}
	if len(signed) != len(specs) {
		t.Fatalf("expected %d signed certs, got %d", len(specs), len(signed))
	}

	for _, sc := range signed {
		block, _ := pem.Decode([]byte(sc.CertPEM))
		if block == nil {
			t.Fatalf("%s: signed cert is not valid PEM", sc.Spec.Name)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("%s: parse signed cert: %v", sc.Spec.Name, err)
		}
		if cert.Subject.CommonName != sc.Spec.CN {
			t.Errorf("%s: CN = %q, want %q", sc.Spec.Name, cert.Subject.CommonName, sc.Spec.CN)
		}
		// The leaf must chain to the served CA — proves shared-CA wiring.
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots:     caPool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}); err != nil {
			t.Errorf("%s: chain verify against served CA failed: %v", sc.Spec.Name, err)
		}
	}
}
