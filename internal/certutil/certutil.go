package certutil

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"slices"
	"time"
)

type Spec struct {
	CommonName    string
	Organizations []string
	DNSNames      []string
	IPAddresses   []net.IP
	KeyUsage      x509.KeyUsage
	ExtKeyUsage   []x509.ExtKeyUsage
}

// NeedsReconcile returns true when the certificate/key pair is missing or no
// longer matches the desired certificate shape closely enough for safe reuse.
func NeedsReconcile(certPath, keyPath string, spec Spec) (bool, string) {
	cert, err := loadCertificate(certPath)
	if err != nil {
		return true, err.Error()
	}

	key, err := loadPrivateKey(keyPath)
	if err != nil {
		return true, err.Error()
	}

	if !publicKeysMatch(cert.PublicKey, publicKey(key)) {
		return true, "certificate and key do not match"
	}

	now := time.Now()
	if now.Before(cert.NotBefore) {
		return true, "certificate is not yet valid"
	}
	if now.After(cert.NotAfter) {
		return true, "certificate is expired"
	}

	if spec.CommonName != "" && cert.Subject.CommonName != spec.CommonName {
		return true, fmt.Sprintf("certificate CN mismatch: have %q want %q", cert.Subject.CommonName, spec.CommonName)
	}

	if len(spec.Organizations) > 0 {
		actual := slices.Clone(cert.Subject.Organization)
		expected := slices.Clone(spec.Organizations)
		slices.Sort(actual)
		slices.Sort(expected)
		if !slices.Equal(actual, expected) {
			return true, "certificate organization mismatch"
		}
	}

	for _, dnsName := range spec.DNSNames {
		if !slices.Contains(cert.DNSNames, dnsName) {
			return true, fmt.Sprintf("certificate missing DNS SAN %q", dnsName)
		}
	}

	for _, ip := range spec.IPAddresses {
		if !containsIP(cert.IPAddresses, ip) {
			return true, fmt.Sprintf("certificate missing IP SAN %q", ip.String())
		}
	}

	if spec.KeyUsage != 0 && cert.KeyUsage&spec.KeyUsage != spec.KeyUsage {
		return true, "certificate key usage mismatch"
	}

	for _, usage := range spec.ExtKeyUsage {
		if !slices.Contains(cert.ExtKeyUsage, usage) {
			return true, fmt.Sprintf("certificate missing extended key usage %d", usage)
		}
	}

	return false, ""
}

// CertFileNeedsRefresh returns true when the certificate file is missing,
// malformed, or expired.
func CertFileNeedsRefresh(path string) (bool, string) {
	cert, err := loadCertificate(path)
	if err != nil {
		return true, err.Error()
	}
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return true, "certificate is not yet valid"
	}
	if now.After(cert.NotAfter) {
		return true, "certificate is expired"
	}
	return false, ""
}

func loadCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read certificate %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("invalid certificate PEM %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate %s: %w", path, err)
	}
	return cert, nil
}

func loadPrivateKey(path string) (crypto.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid private key PEM %s", path)
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	return nil, fmt.Errorf("unsupported private key %s", path)
}

func publicKey(key crypto.PrivateKey) crypto.PublicKey {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	case ed25519.PrivateKey:
		return k.Public().(ed25519.PublicKey)
	default:
		return nil
	}
}

func publicKeysMatch(a, b crypto.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}
	aDER, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false
	}
	bDER, err := x509.MarshalPKIXPublicKey(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aDER, bDER)
}

func containsIP(values []net.IP, target net.IP) bool {
	for _, value := range values {
		if value.Equal(target) {
			return true
		}
	}
	return false
}
