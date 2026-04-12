package magnum

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Client provides access to the Magnum certificate API via OpenStack Keystone
// authentication.
type Client struct {
	AuthURL     string
	MagnumURL   string
	TrusteeUID  string
	TrusteePwd  string
	TrustID     string
	ClusterUUID string
	VerifyCA    bool
	httpClient  *http.Client
}

// NewClient creates a Magnum API client. When verifyCA is false, TLS
// certificate verification is skipped (matching the -k curl flag).
func NewClient(authURL, magnumURL, trusteeUID, trusteePwd, trustID, clusterUUID string, verifyCA bool) *Client {
	transport := &http.Transport{}
	if !verifyCA {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &Client{
		AuthURL:     authURL,
		MagnumURL:   magnumURL,
		TrusteeUID:  trusteeUID,
		TrusteePwd:  trusteePwd,
		TrustID:     trustID,
		ClusterUUID: clusterUUID,
		VerifyCA:    verifyCA,
		httpClient:  &http.Client{Transport: transport},
	}
}

// GetToken authenticates to Keystone using trustee credentials and returns an
// X-Subject-Token.
func (c *Client) GetToken() (string, error) {
	body := fmt.Sprintf(`{
		"auth": {
			"identity": {
				"methods": ["password"],
				"password": {
					"user": {
						"id": %q,
						"password": %q
					}
				}
			},
			"scope": {
				"OS-TRUST:trust": {
					"id": %q
				}
			}
		}
	}`, c.TrusteeUID, c.TrusteePwd, c.TrustID)

	url := strings.TrimRight(c.AuthURL, "/") + "/auth/tokens"
	resp, err := c.httpClient.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("keystone auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("keystone auth: status %d: %s", resp.StatusCode, string(respBody))
	}
	token := resp.Header.Get("X-Subject-Token")
	if token == "" {
		return "", fmt.Errorf("keystone auth: no X-Subject-Token in response")
	}
	return token, nil
}

// FetchCACert retrieves the cluster CA certificate PEM from the Magnum API.
func (c *Client) FetchCACert(token string) (string, error) {
	url := strings.TrimRight(c.MagnumURL, "/") + "/certificates/" + c.ClusterUUID
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("OpenStack-API-Version", "container-infra latest")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch CA cert: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch CA cert: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		PEM string `json:"pem"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("fetch CA cert: decode: %w", err)
	}
	return result.PEM, nil
}

// SignCSR sends a CSR to Magnum for signing and returns the signed certificate PEM.
func (c *Client) SignCSR(token string, csrPEM string) (string, error) {
	payload := fmt.Sprintf(`{"cluster_uuid": %q, "csr": %q}`, c.ClusterUUID, csrPEM)
	url := strings.TrimRight(c.MagnumURL, "/") + "/certificates"
	req, err := http.NewRequest("POST", url, strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("OpenStack-API-Version", "container-infra latest")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sign CSR: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("sign CSR: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		PEM string `json:"pem"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("sign CSR: decode: %w", err)
	}
	return result.PEM, nil
}

// CertSpec describes a certificate to generate and sign.
type CertSpec struct {
	Name        string   // e.g. "server", "kubelet", "admin"
	CN          string   // Common Name
	O           string   // Organization (optional)
	SANIPs      []string // IP SANs
	SANDNSs     []string // DNS SANs
	KeyUsage    x509.KeyUsage
	ExtKeyUsage []x509.ExtKeyUsage
}

type SignedCert struct {
	Spec    CertSpec
	KeyPEM  string
	CertPEM string
}

// GenerateAndSignCerts generates keys/CSRs and signs all requested certs in
// parallel. Results preserve the input order so callers can write files and
// report changes deterministically.
func GenerateAndSignCerts(client *Client, token string, specs []CertSpec) ([]SignedCert, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if client == nil {
		return nil, fmt.Errorf("generate/sign certificates: nil Magnum client")
	}

	results := make([]SignedCert, len(specs))
	errCh := make(chan error, len(specs))
	var wg sync.WaitGroup

	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec CertSpec) {
			defer wg.Done()

			keyPEM, csrPEM, err := GenerateKeyAndCSR(spec)
			if err != nil {
				errCh <- fmt.Errorf("generate %s key/CSR: %w", spec.Name, err)
				return
			}

			certPEM, err := client.SignCSR(token, csrPEM)
			if err != nil {
				errCh <- fmt.Errorf("sign %s CSR: %w", spec.Name, err)
				return
			}

			results[i] = SignedCert{
				Spec:    spec,
				KeyPEM:  keyPEM,
				CertPEM: certPEM,
			}
		}(i, spec)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		return nil, err
	}

	return results, nil
}

// GenerateKeyAndCSR creates a 4096-bit RSA key and a PEM-encoded CSR.
func GenerateKeyAndCSR(spec CertSpec) (keyPEM, csrPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}

	keyBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	var ipSANs []net.IP
	for _, ip := range spec.SANIPs {
		if parsed := net.ParseIP(ip); parsed != nil {
			ipSANs = append(ipSANs, parsed)
		}
	}

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:         spec.CN,
			Organization:       []string{},
			Country:            []string{"US"},
			Province:           []string{"TX"},
			Locality:           []string{"Austin"},
			OrganizationalUnit: []string{"OpenStack/Magnum"},
		},
		IPAddresses: ipSANs,
		DNSNames:    spec.SANDNSs,
	}
	if spec.O != "" {
		template.Subject.Organization = []string{spec.O}
	}
	if len(spec.ExtKeyUsage) > 0 {
		extension, err := marshalExtendedKeyUsage(spec.ExtKeyUsage)
		if err != nil {
			return "", "", fmt.Errorf("marshal extended key usage: %w", err)
		}
		template.ExtraExtensions = append(template.ExtraExtensions, extension)
	}
	if spec.KeyUsage != 0 {
		extension, err := marshalKeyUsage(spec.KeyUsage)
		if err != nil {
			return "", "", fmt.Errorf("marshal key usage: %w", err)
		}
		template.ExtraExtensions = append(template.ExtraExtensions, extension)
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return "", "", fmt.Errorf("create CSR: %w", err)
	}

	csrBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csrDER,
	})

	return string(keyBytes), string(csrBytes), nil
}

var (
	oidExtensionKeyUsage         = asn1.ObjectIdentifier{2, 5, 29, 15}
	oidExtensionExtendedKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 37}
)

func marshalExtendedKeyUsage(usages []x509.ExtKeyUsage) (pkix.Extension, error) {
	encoded := make([]asn1.ObjectIdentifier, 0, len(usages))
	for _, usage := range usages {
		oid, ok := extKeyUsageOID(usage)
		if !ok {
			return pkix.Extension{}, fmt.Errorf("unsupported extended key usage %d", usage)
		}
		encoded = append(encoded, oid)
	}
	value, err := asn1.Marshal(encoded)
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{Id: oidExtensionExtendedKeyUsage, Value: value}, nil
}

func marshalKeyUsage(usage x509.KeyUsage) (pkix.Extension, error) {
	var bits asn1.BitString
	for bit := 0; bit < 9; bit++ {
		if usage&(1<<bit) != 0 {
			bits.BitLength = bit + 1
		}
	}
	if bits.BitLength == 0 {
		return pkix.Extension{Id: oidExtensionKeyUsage, Critical: true, Value: []byte{0x03, 0x01, 0x00}}, nil
	}
	byteLen := (bits.BitLength + 7) / 8
	bits.Bytes = make([]byte, byteLen)
	for bit := 0; bit < bits.BitLength; bit++ {
		if usage&(1<<bit) == 0 {
			continue
		}
		byteIndex := bit / 8
		bitIndex := uint(7 - (bit % 8))
		bits.Bytes[byteIndex] |= 1 << bitIndex
	}
	value, err := asn1.Marshal(bits)
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{Id: oidExtensionKeyUsage, Critical: true, Value: value}, nil
}

func extKeyUsageOID(usage x509.ExtKeyUsage) (asn1.ObjectIdentifier, bool) {
	switch usage {
	case x509.ExtKeyUsageAny:
		return asn1.ObjectIdentifier{2, 5, 29, 37, 0}, true
	case x509.ExtKeyUsageServerAuth:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}, true
	case x509.ExtKeyUsageClientAuth:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}, true
	case x509.ExtKeyUsageCodeSigning:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 3}, true
	case x509.ExtKeyUsageEmailProtection:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 4}, true
	case x509.ExtKeyUsageIPSECEndSystem:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 5}, true
	case x509.ExtKeyUsageIPSECTunnel:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 6}, true
	case x509.ExtKeyUsageIPSECUser:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 7}, true
	case x509.ExtKeyUsageTimeStamping:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}, true
	case x509.ExtKeyUsageOCSPSigning:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 9}, true
	case x509.ExtKeyUsageMicrosoftServerGatedCrypto:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 10, 3, 3}, true
	case x509.ExtKeyUsageNetscapeServerGatedCrypto:
		return asn1.ObjectIdentifier{2, 16, 840, 1, 113730, 4, 1}, true
	case x509.ExtKeyUsageMicrosoftCommercialCodeSigning:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 22}, true
	case x509.ExtKeyUsageMicrosoftKernelCodeSigning:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 61, 1, 1}, true
	default:
		return nil, false
	}
}
