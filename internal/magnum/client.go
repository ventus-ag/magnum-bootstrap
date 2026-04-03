package magnum

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
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
	Name    string   // e.g. "server", "kubelet", "admin"
	CN      string   // Common Name
	O       string   // Organization (optional)
	SANIPs  []string // IP SANs
	SANDNSs []string // DNS SANs
	KeyUsage    x509.KeyUsage
	ExtKeyUsage []x509.ExtKeyUsage
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
			CommonName:   spec.CN,
			Organization: []string{},
			Country:      []string{"US"},
			Province:     []string{"TX"},
			Locality:     []string{"Austin"},
			OrganizationalUnit: []string{"OpenStack/Magnum"},
		},
		IPAddresses: ipSANs,
		DNSNames:    spec.SANDNSs,
	}
	if spec.O != "" {
		template.Subject.Organization = []string{spec.O}
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
