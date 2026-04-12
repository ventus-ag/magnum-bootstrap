package magnum

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerateKeyAndCSRIncludesRequestedExtensions(t *testing.T) {
	_, csrPEM, err := GenerateKeyAndCSR(CertSpec{
		Name:    "scheduler",
		CN:      "system:kube-scheduler",
		O:       "system:kube-scheduler",
		SANIPs:  []string{"127.0.0.1"},
		SANDNSs: []string{"kubernetes"},
		KeyUsage: x509.KeyUsageDigitalSignature |
			x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
	})
	if err != nil {
		t.Fatalf("GenerateKeyAndCSR returned error: %v", err)
	}

	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatalf("failed to decode CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificateRequest returned error: %v", err)
	}

	if len(csr.DNSNames) != 1 || csr.DNSNames[0] != "kubernetes" {
		t.Fatalf("unexpected DNS SANs: %#v", csr.DNSNames)
	}
	if len(csr.IPAddresses) != 1 || csr.IPAddresses[0].String() != "127.0.0.1" {
		t.Fatalf("unexpected IP SANs: %#v", csr.IPAddresses)
	}

	keyUsageExt := false
	ekuExt := false
	for _, ext := range csr.Extensions {
		if ext.Id.Equal(oidExtensionKeyUsage) {
			keyUsageExt = true
		}
		if ext.Id.Equal(oidExtensionExtendedKeyUsage) {
			ekuExt = true
		}
	}
	if !keyUsageExt {
		t.Fatalf("expected CSR to include keyUsage extension")
	}
	if !ekuExt {
		t.Fatalf("expected CSR to include extendedKeyUsage extension")
	}
}

func TestGenerateAndSignCertsRunsSignRequestsConcurrentlyAndPreservesOrder(t *testing.T) {
	specs := []CertSpec{
		{Name: "first", CN: "first"},
		{Name: "second", CN: "second"},
	}

	release := make(chan struct{})
	var inFlight int32
	var maxInFlight int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/certificates" {
			http.NotFound(w, r)
			return
		}

		current := atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)

		for {
			maxSeen := atomic.LoadInt32(&maxInFlight)
			if current <= maxSeen || atomic.CompareAndSwapInt32(&maxInFlight, maxSeen, current) {
				break
			}
		}

		if current == int32(len(specs)) {
			close(release)
		}

		select {
		case <-release:
		case <-time.After(10 * time.Second):
			http.Error(w, "sign requests did not overlap", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pem":"signed cert"}`))
	}))
	defer server.Close()

	client := &Client{
		MagnumURL:   server.URL,
		ClusterUUID: "cluster-id",
		httpClient:  server.Client(),
	}

	results, err := GenerateAndSignCerts(client, "token", specs)
	if err != nil {
		t.Fatalf("GenerateAndSignCerts returned error: %v", err)
	}

	if got := atomic.LoadInt32(&maxInFlight); got != int32(len(specs)) {
		t.Fatalf("expected %d concurrent sign requests, saw %d", len(specs), got)
	}
	if len(results) != len(specs) {
		t.Fatalf("expected %d signed certs, got %d", len(specs), len(results))
	}
	for i, result := range results {
		if result.Spec.Name != specs[i].Name {
			t.Fatalf("result %d spec order mismatch: got %s, want %s", i, result.Spec.Name, specs[i].Name)
		}
		if !strings.Contains(result.KeyPEM, "BEGIN RSA PRIVATE KEY") {
			t.Fatalf("result %d missing private key PEM", i)
		}
		if result.CertPEM != "signed cert" {
			t.Fatalf("result %d cert PEM mismatch: %q", i, result.CertPEM)
		}
	}
}
