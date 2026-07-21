package certutil

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"slices"
	"strings"
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

// LocalCAUsable reports whether the on-disk CA material can act as a local
// signing authority: the CA certificate parses and is inside its validity
// window, the CA private key exists, and the two are a matching pair. Used by
// the cert modules to decide whether a node whose Barbican CA is unusable,
// mismatched, or unreachable can keep converging on a manually-restored local
// CA instead of wedging (the operator later runs ca-rotate to reconverge on
// the canonical CA). The reason string explains a false result.
func LocalCAUsable(caCertPath, caKeyPath string) (bool, string) {
	if needs, why := CertFileNeedsRefresh(caCertPath); needs {
		return false, why
	}
	certPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return false, err.Error()
	}
	if !CertPEMMatchesKeyFile(certPEM, caKeyPath) {
		return false, fmt.Sprintf("CA certificate %s and key %s are not a matching pair (or the key is missing/unreadable)", caCertPath, caKeyPath)
	}
	return true, ""
}

// RenewExpiredCA mints a replacement CA certificate from the cluster's own CA
// key: same public key, same subject, fresh validity window. Because the key
// is unchanged, every leaf the old CA signed still chains to the renewed CA,
// and the renewed CA still validates against anything keyed on the public key
// — it is the cluster's "current CA" re-dated, not a new trust root. Used for
// last-resort recovery of a cluster whose CA expired before anyone rotated it
// (Barbican serves the same expired CA, so neither fetch nor local fallback
// can help). Errors unless the on-disk cert parses, is actually expired, and
// pairs with the key — so it can only ever re-date the cluster's own CA,
// never fabricate or adopt foreign material.
func RenewExpiredCA(certPath, keyPath string) ([]byte, error) {
	cert, err := loadCertificate(certPath)
	if err != nil {
		return nil, err
	}
	if time.Now().Before(cert.NotAfter) {
		return nil, fmt.Errorf("CA certificate %s is not expired; refusing to renew", certPath)
	}
	key, err := loadPrivateKey(keyPath)
	if err != nil {
		return nil, err
	}
	if !publicKeysMatch(cert.PublicKey, publicKey(key)) {
		return nil, fmt.Errorf("key %s is not the pair of CA certificate %s; refusing to renew", keyPath, certPath)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("key %s cannot sign (type %T)", keyPath, key)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               cert.Subject,
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              cert.KeyUsage | x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, cert.PublicKey, signer)
	if err != nil {
		return nil, fmt.Errorf("renew CA certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// LeafPEMSignedByCAFile reports whether the PEM-encoded leaf certificate was
// signed by any certificate in caFile. Used to vet freshly API-signed material
// before installing it over a cluster whose on-disk CA may differ from
// Barbican's (a hand-restored cluster whose CA mismatch was not otherwise
// observable this run). Returns true when caFile is missing or unparseable —
// there is nothing to verify against, so the signed material is canonical.
func LeafPEMSignedByCAFile(leafPEM []byte, caPath string) bool {
	leaf, err := parseCertPEM(leafPEM)
	if err != nil {
		return false
	}
	cas, err := loadCACerts(caPath)
	if err != nil || len(cas) == 0 {
		return true
	}
	for _, ca := range cas {
		if leafSignedBy(leaf, ca) {
			return true
		}
	}
	return false
}

// CAFromKubeconfig extracts the embedded cluster CA (certificate-authority-data)
// from a kubeconfig file. Used to discover a hand-rotated CA that an operator
// installed into the live kubeconfigs without updating the certs directory.
// Line-scan on our own generated kubeconfig shape — no YAML dependency.
func CAFromKubeconfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "certificate-authority-data:") {
			continue
		}
		encoded := strings.TrimSpace(strings.TrimPrefix(trimmed, "certificate-authority-data:"))
		if encoded == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode certificate-authority-data in %s: %w", path, err)
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("no certificate-authority-data in %s", path)
}

// SanitizeTrustBundle filters a PEM trust bundle down to certificates that
// Go >=1.23 x509 accepts. Old system stores (FCoS 34 era) carry roots with
// negative serial numbers (e.g. the EC-ACC root), which modern Go rejects
// with "x509: negative serial number" — cloud-provider-openstack, the
// cluster autoscaler, and magnum-auto-healer then hard-fail startup when
// such a cert is present in their ca-file. Unparseable and negative-serial
// certificates are dropped; surviving certs are re-encoded in order.
// Returns the sanitized bundle and the number of dropped certificates.
func SanitizeTrustBundle(bundle []byte) ([]byte, int) {
	var out bytes.Buffer
	dropped := 0
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || cert.SerialNumber == nil || cert.SerialNumber.Sign() < 0 {
			dropped++
			continue
		}
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes})
	}
	return out.Bytes(), dropped
}
