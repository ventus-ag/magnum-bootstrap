// Command mock-magnum is a self-contained stand-in for the OpenStack Keystone
// and Magnum certificate APIs that the reconciler talks to during cert
// generation (see internal/magnum/client.go).
//
// It implements exactly three behaviours the reconciler depends on:
//
//   - POST .../auth/tokens          -> 201 with an X-Subject-Token header
//   - GET  .../certificates/{uuid}  -> {"pem": "<cluster CA cert>"}
//   - POST .../certificates         -> sign the posted CSR, {"pem": "<leaf cert>"}
//
// The same CA it serves and signs with is written to -ca-cert/-ca-key so the
// e2e harness can place the private key into heat-params as CA_KEY (which the
// reconciler writes to /etc/kubernetes/certs/ca.key for the controller-manager).
// Cert and key therefore come from one CA, so signed leaf certs verify against
// the CA the node trusts and a real single-node cluster actually comes up.
//
// Routing is by path suffix, not by a fixed /v1 or /v3 prefix, so it works
// regardless of how AUTH_URL / MAGNUM_URL are shaped in heat-params.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	var (
		listen   = flag.String("listen", ":9511", "address to listen on (host:port)")
		caCert   = flag.String("ca-cert", "ca.crt", "path to CA cert PEM (loaded if present, else generated)")
		caKey    = flag.String("ca-key", "ca.key", "path to CA key PEM (loaded if present, else generated; place its contents in heat-params CA_KEY)")
		certDays = flag.Int("cert-days", 3650, "validity in days for signed leaf certificates")
		genOnly  = flag.Bool("gen-ca", false, "generate the CA files and exit without serving")
		verbose  = flag.Bool("v", false, "log every request")
	)
	flag.Parse()

	srv, err := newServer(*caCert, *caKey, *certDays, *verbose)
	if err != nil {
		log.Fatalf("mock-magnum: %v", err)
	}
	if *genOnly {
		log.Printf("mock-magnum: wrote CA to %s / %s", *caCert, *caKey)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.route)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	log.Printf("mock-magnum: listening on %s (CA subject %q)", *listen, srv.ca.Subject.CommonName)
	if err := http.ListenAndServe(*listen, mux); err != nil { //nolint:gosec
		log.Fatalf("mock-magnum: serve: %v", err)
	}
}

type server struct {
	ca       *x509.Certificate
	caPEM    string
	caKey    *rsa.PrivateKey
	certDays int
	verbose  bool

	mu     sync.Mutex
	serial int64
}

func newServer(caCertPath, caKeyPath string, certDays int, verbose bool) (*server, error) {
	cert, key, certPEM, err := loadOrCreateCA(caCertPath, caKeyPath)
	if err != nil {
		return nil, err
	}
	return &server{ca: cert, caPEM: certPEM, caKey: key, certDays: certDays, verbose: verbose, serial: 1000}, nil
}

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	if s.verbose {
		log.Printf("mock-magnum: %s %s", r.Method, r.URL.Path)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/auth/tokens") && r.Method == http.MethodPost:
		s.handleToken(w, r)
	case strings.HasSuffix(r.URL.Path, "/certificates") && r.Method == http.MethodPost:
		s.handleSign(w, r)
	case strings.Contains(r.URL.Path, "/certificates/") && r.Method == http.MethodGet:
		s.handleCAFetch(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleToken mimics Keystone trust-scoped password auth. The reconciler only
// reads the X-Subject-Token header, so the body is cosmetic.
func (s *server) handleToken(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("X-Subject-Token", "mock-token-"+fmt.Sprint(time.Now().UnixNano()))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"token":{"methods":["password"],"roles":[{"name":"member"}]}}`)
}

// handleCAFetch returns the cluster CA certificate PEM.
func (s *server) handleCAFetch(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"pem": s.caPEM})
}

// handleSign signs the posted CSR with the CA. It preserves the CSR Subject and
// SANs and grants both server and client auth so every reconciler cert spec
// (server, kubelet, admin, etc.) verifies as up-to-date on the next run.
func (s *server) handleSign(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		ClusterUUID string `json:"cluster_uuid"`
		CSR         string `json:"csr"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}

	csr, err := parseCSR(req.CSR)
	if err != nil {
		http.Error(w, "parse csr: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := csr.CheckSignature(); err != nil {
		http.Error(w, "csr signature: "+err.Error(), http.StatusBadRequest)
		return
	}

	leafPEM, err := s.sign(csr)
	if err != nil {
		http.Error(w, "sign: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"pem": leafPEM})
}

func (s *server) sign(csr *x509.CertificateRequest) (string, error) {
	s.mu.Lock()
	s.serial++
	serial := s.serial
	s.mu.Unlock()

	now := time.Now().Add(-5 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               csr.Subject,
		NotBefore:             now,
		NotAfter:              now.Add(time.Duration(s.certDays) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageKeyAgreement | x509.KeyUsageDataEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              csr.DNSNames,
		IPAddresses:           csr.IPAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca, csr.PublicKey, s.caKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

func parseCSR(pemStr string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("invalid CSR PEM")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

func loadOrCreateCA(certPath, keyPath string) (*x509.Certificate, *rsa.PrivateKey, string, error) {
	certBytes, certErr := os.ReadFile(certPath)
	keyBytes, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, key, err := parseCA(certBytes, keyBytes)
		if err != nil {
			return nil, nil, "", err
		}
		return cert, key, string(certBytes), nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, nil, "", err
	}
	now := time.Now().Add(-5 * time.Minute)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kubernetes", Organization: []string{"OpenStack/Magnum (mock)"}},
		NotBefore:             now,
		NotAfter:              now.Add(20 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil { //nolint:gosec
		return nil, nil, "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, "", err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, "", err
	}
	return cert, key, string(certPEM), nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	cblock, _ := pem.Decode(certPEM)
	if cblock == nil {
		return nil, nil, fmt.Errorf("invalid CA cert PEM")
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, nil, fmt.Errorf("invalid CA key PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(kblock.Bytes); err == nil {
		return cert, key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(kblock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("CA key is not RSA")
	}
	return cert, key, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
