package magnum

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
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
