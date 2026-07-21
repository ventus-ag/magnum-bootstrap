package magnum

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// localLeafValidity matches the validity Magnum's cert manager applies to
// cluster leaf certificates, so locally-signed material ages the same way as
// API-signed material.
const localLeafValidity = 5 * 365 * 24 * time.Hour

// GenerateLocalSignedCerts generates keys and signs all requested certs with
// the on-disk CA pair instead of the Magnum CSR-signing API. It is the
// fallback path for a manually-restored cluster whose local CA no longer
// matches (or cannot reach) Barbican: the node keeps converging on the local
// CA until an operator-triggered ca-rotate reconverges it on the canonical CA.
// Output mirrors GenerateAndSignCerts — same subject style, same key size,
// input order preserved.
func GenerateLocalSignedCerts(caCertPath, caKeyPath string, specs []CertSpec) ([]SignedCert, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	caCert, err := loadPEMCertificate(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("local sign: load CA certificate %s: %w", caCertPath, err)
	}
	caKey, err := loadPEMSigner(caKeyPath)
	if err != nil {
		return nil, fmt.Errorf("local sign: load CA key %s: %w", caKeyPath, err)
	}

	results := make([]SignedCert, len(specs))
	errCh := make(chan error, len(specs))
	var wg sync.WaitGroup

	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec CertSpec) {
			defer wg.Done()

			signed, err := signSpecLocally(caCert, caKey, spec)
			if err != nil {
				errCh <- fmt.Errorf("local sign %s: %w", spec.Name, err)
				return
			}
			results[i] = signed
		}(i, spec)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		return nil, err
	}

	return results, nil
}

func signSpecLocally(caCert *x509.Certificate, caKey crypto.Signer, spec CertSpec) (SignedCert, error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return SignedCert{}, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return SignedCert{}, fmt.Errorf("generate serial: %w", err)
	}

	var ipSANs []net.IP
	for _, ip := range spec.SANIPs {
		if parsed := net.ParseIP(ip); parsed != nil {
			ipSANs = append(ipSANs, parsed)
		}
	}

	keyUsage := spec.KeyUsage
	if keyUsage == 0 {
		keyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         spec.CN,
			Country:            []string{"US"},
			Province:           []string{"TX"},
			Locality:           []string{"Austin"},
			OrganizationalUnit: []string{"OpenStack/Magnum"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(localLeafValidity),
		KeyUsage:              keyUsage,
		ExtKeyUsage:           spec.ExtKeyUsage,
		BasicConstraintsValid: true,
		IPAddresses:           ipSANs,
		DNSNames:              spec.SANDNSs,
	}
	if spec.O != "" {
		template.Subject.Organization = []string{spec.O}
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return SignedCert{}, fmt.Errorf("sign certificate: %w", err)
	}

	return SignedCert{
		Spec: spec,
		KeyPEM: string(pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})),
		CertPEM: string(pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: der,
		})),
	}, nil
}

func loadPEMCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func loadPEMSigner(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unsupported private key format: %w", err)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("private key type %T cannot sign", parsed)
	}
	return signer, nil
}
