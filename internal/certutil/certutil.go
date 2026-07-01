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

// LeafChainBroken reports true ONLY when the leaf at leafPath and the CA file
// at caFile both parse, but NO certificate in caFile signed the leaf. caFile may
// hold a new+old bundle (multiple PEM blocks); the leaf need only chain to one
// of them, so this does not misfire mid dual-CA rotation. Missing or unparseable
// inputs return false — those cases are already handled by the missing/expired
// checks (a fresh node has no leaf yet, and reporting "chain broken" there would
// wrongly force a CA refetch). A true result means the on-disk leaf no longer
// chains to the on-disk CA: the CA or the leaf was replaced out from under us
// (e.g. changed by the cluster owner), and the leaf must be re-signed against the
// current CA.
func LeafChainBroken(leafPath, caFile string) bool {
	leaf, err := loadCertificate(leafPath)
	if err != nil {
		return false
	}
	cas, err := loadCACerts(caFile)
	if err != nil || len(cas) == 0 {
		return false
	}
	for _, ca := range cas {
		if leafSignedBy(leaf, ca) {
			return false
		}
	}
	return true
}

// leafSignedBy reports whether ca issued leaf. It prefers the standard
// CheckSignatureFrom (which also validates CA basic constraints) but falls back
// to a bare signature check against the CA public key when the CA certificate
// does not advertise IsCA/BasicConstraints — some legacy Magnum CA material does
// not, and we only care here whether the key pair matches, not whether the CA is
// policy-valid.
func leafSignedBy(leaf, ca *x509.Certificate) bool {
	if err := leaf.CheckSignatureFrom(ca); err == nil {
		return true
	}
	// Fallback: verify the leaf's signature directly with the CA public key,
	// ignoring CA basic-constraints/key-usage policy that CheckSignatureFrom
	// enforces (some legacy Magnum CA certs omit IsCA/KeyUsageCertSign).
	return ca.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
}

// loadCACerts decodes every CERTIFICATE block in a PEM file. It tolerates
// interleaved non-certificate blocks and returns an error only when the file
// cannot be read or contains no parseable certificate.
func loadCACerts(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA file %s: %w", path, err)
	}
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificate found in %s", path)
	}
	return certs, nil
}

// CertFileNeedsRefresh returns true when the certificate file is missing,
// malformed, or expired.
func CertFileNeedsRefresh(path string) (bool, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return true, fmt.Errorf("read certificate %s: %w", path, err).Error()
	}
	return CertPEMNeedsRefresh(data)
}

// CertPEMNeedsRefresh is the in-memory counterpart of CertFileNeedsRefresh: it
// reports whether a PEM-encoded certificate is unparseable or outside its
// validity window. Used to vet material freshly fetched from Barbican before
// installing it (e.g. detect that Barbican still serves an already-expired CA).
func CertPEMNeedsRefresh(pemData []byte) (bool, string) {
	cert, err := parseCertPEM(pemData)
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

// CertPEMMatchesKeyFile reports whether the PEM-encoded certificate's public
// half matches the private key at keyPath. It is the inverse of
// KeyPEMMatchesCertFile and returns false if either input cannot be read or
// parsed — callers treat false as "this fetched cert is not provably the partner
// of the on-disk key, so do not install it over a working pair".
func CertPEMMatchesKeyFile(certPEM []byte, keyPath string) bool {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return false
	}
	key, err := loadPrivateKey(keyPath)
	if err != nil {
		return false
	}
	return publicKeysMatch(cert.PublicKey, publicKey(key))
}

// KeyPEMMatchesCertPEM reports whether the PEM-encoded private key and
// PEM-encoded certificate are a matching pair. Both inputs are in-memory (no
// file I/O), for vetting freshly-fetched/parameter-supplied material before it
// is written to disk. Returns false if either input cannot be parsed.
func KeyPEMMatchesCertPEM(keyPEM, certPEM []byte) bool {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return false
	}
	key, err := loadPrivateKeyPEM(keyPEM)
	if err != nil {
		return false
	}
	return publicKeysMatch(cert.PublicKey, publicKey(key))
}

func loadCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read certificate %s: %w", path, err)
	}
	cert, err := parseCertPEM(data)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, path)
	}
	return cert, nil
}

func parseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("invalid certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	return cert, nil
}

func loadPrivateKey(path string) (crypto.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key %s: %w", path, err)
	}
	key, err := loadPrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, path)
	}
	return key, nil
}

func loadPrivateKeyPEM(data []byte) (crypto.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid private key PEM")
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

	return nil, fmt.Errorf("unsupported private key")
}

// KeyPEMMatchesCertFile reports whether the PEM-encoded private key's public
// half matches the public key in the certificate at certPath. Returns false if
// either input cannot be read or parsed — callers treat false as "this key is
// not provably the partner of that certificate, so do not install it".
func KeyPEMMatchesCertFile(keyPEM []byte, certPath string) bool {
	cert, err := loadCertificate(certPath)
	if err != nil {
		return false
	}
	key, err := loadPrivateKeyPEM(keyPEM)
	if err != nil {
		return false
	}
	return publicKeysMatch(cert.PublicKey, publicKey(key))
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
