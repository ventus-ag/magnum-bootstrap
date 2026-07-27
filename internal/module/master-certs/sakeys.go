package mastercerts

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	coord "github.com/ventus-ag/magnum-bootstrap/internal/carotation"
	"github.com/ventus-ag/magnum-bootstrap/internal/certutil"
	"github.com/ventus-ag/magnum-bootstrap/internal/config"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
	"github.com/ventus-ag/magnum-bootstrap/internal/hostresource"
	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
)

// Service-account verify-key convergence.
//
// Every master signs the ServiceAccount tokens it issues with its own
// service_account_private.key and verifies incoming tokens against the public
// keys in service_account.key (kube-apiserver's --service-account-key-file).
// Those two files are established at cluster creation and are meant to be
// identical on every master.
//
// A master built later can end up with a DIFFERENT pair: Magnum regenerates the
// keypair whenever it cannot read the existing one back out of the Heat stack,
// and a fresh node has an empty cert dir so it seeds from that new heat-param,
// while the pre-existing masters keep their own (writeSAKeyIfAbsent refuses to
// overwrite live SA material — correctly, since overwriting invalidates every
// token in the cluster).
//
// The result is a split control plane: a token minted by one apiserver is
// rejected with 401 by the others. Because client-go pins a long-lived HTTP/2
// connection to a single backend, an in-cluster workload is then either fine or
// permanently broken depending on which master it happened to land on.
//
// The fix needs no private keys. --service-account-key-file accepts a BUNDLE of
// PEM public keys, so every master can verify tokens issued by every other
// master while continuing to sign with its own key. CA rotation already relies
// on this (it writes a new+old two-key file). Convergence is purely additive:
// no token is invalidated, no workload needs restarting, and masters can
// converge in any order.
const (
	saVerifyKeyFile  = "service_account.key"
	saPrivateKeyFile = "service_account_private.key"

	saPeerFetchTimeout = 5 * time.Second
	saTotalTimeout     = 45 * time.Second
)

// saKeyEnv carries everything convergeSAVerifyKeys needs. The two function
// fields are seams: production wires them to real HTTPS calls, tests supply
// canned documents.
type saKeyEnv struct {
	certDir  string
	executor *host.Executor
	logger   *logging.Logger

	// heatParamKey is KUBE_SERVICE_ACCOUNT_KEY as parsed from heat-params.
	heatParamKey string
	// selfIP is this node's address, excluded from the peer sweep.
	selfIP string
	// apiEndpoint is the address used to ask the API for the master list,
	// and the fallback sweep target when that read fails.
	apiEndpoint string
	// numberOfMasters sizes the fallback sweep.
	numberOfMasters int

	// retiredKeyIDs are key IDs a finalized rotation withdrew trust from. They
	// are never adopted back from a peer or from heat-params.
	retiredKeyIDs map[string]bool

	listPeerIPs func(ctx context.Context) ([]string, error)
	fetchJWKS   func(ctx context.Context, host string) ([]byte, error)
}

// dropRetiredKeys filters out keys a previous rotation deliberately retired,
// returning the survivors and how many were dropped.
func dropRetiredKeys(keys [][]byte, retired map[string]bool) ([][]byte, int) {
	if len(retired) == 0 {
		return keys, 0
	}
	kept := make([][]byte, 0, len(keys))
	skipped := 0
	for _, key := range keys {
		id, err := certutil.PublicKeyID(key)
		if err == nil && retired[id] {
			skipped++
			continue
		}
		kept = append(kept, key)
	}
	return kept, skipped
}

// convergeSAVerifyKeys makes this master's service_account.key the union of
// every service-account verification key known in the cluster. It never removes
// a key (pruning belongs to ca-rotate, which alone knows a key is retired) and
// never touches the private signing key.
func convergeSAVerifyKeys(ctx context.Context, env saKeyEnv) ([]host.Change, []string, error) {
	bundlePath := env.certDir + "/" + saVerifyKeyFile

	existing, err := certutil.PublicKeyPEMFile(bundlePath)
	if err != nil {
		return nil, nil, fmt.Errorf("master-certificates: read service-account verify keys: %w", err)
	}
	if len(existing) == 0 {
		// No local material yet — a first boot that has not written the key,
		// or a file we cannot make sense of. Seeding is writeSAKeyIfAbsent's
		// job; adding peer keys to nothing would produce a bundle with no
		// local signing key in it.
		return nil, nil, nil
	}

	candidates := [][]byte{}
	candidates = append(candidates, existing...)

	// The public half of our own signing key. Normally already in the bundle;
	// worth adding explicitly so a node that somehow lost it self-repairs.
	// Exempt from the retired filter below: a key we are actively signing with
	// cannot be one we have withdrawn trust from.
	if own, err := certutil.PublicKeyPEMFromPrivateKeyFile(env.certDir + "/" + saPrivateKeyFile); err == nil {
		candidates = append(candidates, own)
	} else if env.logger != nil {
		env.logger.Infof("master-certificates: local service-account signing key unreadable (%v); relying on the existing bundle", err)
	}

	// Keys we may adopt from outside this node. These are filtered against the
	// retired registry: a rotation's finalize barrier narrows the bundle on
	// purpose, and a peer that has not finalized yet still publishes the key it
	// dropped. Re-adopting it would quietly undo the withdrawal of trust --
	// which for a compromise-driven rotation is the whole point of rotating.
	var adoptable [][]byte
	adoptable = append(adoptable, certutil.ParsePublicKeyPEMs([]byte(normalizeHeatPEM(env.heatParamKey)))...)

	peerKeys, peerWarnings := collectPeerSAKeys(ctx, env)
	adoptable = append(adoptable, peerKeys...)

	warnings := peerWarnings
	adoptable, skipped := dropRetiredKeys(adoptable, env.retiredKeyIDs)
	if skipped > 0 && env.logger != nil {
		env.logger.Infof("master-certificates: ignored %d service-account key(s) a previous rotation retired; peers still publishing them have not finalized yet", skipped)
	}
	candidates = append(candidates, adoptable...)

	merged, added := mergeSAVerifyKeys(existing, candidates)

	if added == 0 {
		// Nothing was missing. A multi-key bundle on its own is not drift --
		// a dual-CA rotation leaves new+old on purpose -- so this is silent.
		return nil, warnings, nil
	}

	if split := describeSAKeySplit(added, merged); split != "" {
		warnings = append(warnings, split)
		if env.logger != nil {
			env.logger.Warnf("master-certificates: %s", split)
		}
	}

	content := make([]byte, 0, len(merged)*512)
	for _, key := range merged {
		content = append(content, key...)
	}

	// An atomic rewrite lands as root:root; the caller runs this before the
	// recursive chown of certDir, which restores kube:kube_etcd so
	// kube-apiserver can still read the 0440 bundle.
	fileResult, err := (hostresource.FileSpec{Path: bundlePath, Content: content, Mode: 0o440}).Apply(env.executor)
	if err != nil {
		return nil, warnings, fmt.Errorf("master-certificates: write service-account verify keys: %w", err)
	}
	changes := fileResult.Changes

	if env.logger != nil {
		env.logger.Infof("master-certificates: added %d service-account verification key(s) to %s (%d total) — this master now accepts tokens issued by every other master",
			added, bundlePath, len(merged))
	}

	return changes, warnings, nil
}

// mergeSAVerifyKeys returns the deduplicated union of existing and candidate
// keys, and how many were added. Existing keys keep their file order and stay
// first; new keys follow, sorted by key ID. A converged bundle therefore has a
// stable byte representation and does not churn between runs.
func mergeSAVerifyKeys(existing, candidates [][]byte) ([][]byte, int) {
	seen := make(map[string]bool, len(candidates))
	result := make([][]byte, 0, len(candidates))

	for _, key := range existing {
		id, err := certutil.PublicKeyID(key)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, key)
	}

	var fresh []struct {
		id  string
		pem []byte
	}
	for _, key := range candidates {
		id, err := certutil.PublicKeyID(key)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		fresh = append(fresh, struct {
			id  string
			pem []byte
		}{id, key})
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].id < fresh[j].id })
	for _, key := range fresh {
		result = append(result, key.pem)
	}

	return result, len(fresh)
}

// describeSAKeySplit reports that this master was missing service-account keys
// the rest of the cluster already used -- i.e. it was rejecting tokens issued
// by its peers until now. Only a genuine adoption is reported: a bundle that
// merely holds several keys is normal after a dual-CA rotation and says
// nothing about drift.
func describeSAKeySplit(added int, keys [][]byte) string {
	if added == 0 {
		return ""
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		if id, err := certutil.PublicKeyID(key); err == nil {
			ids = append(ids, id)
		}
	}
	return fmt.Sprintf("adopted %d service-account verification key(s) this master was missing; "+
		"until now it rejected tokens issued by the masters holding them (401 Unauthorized). "+
		"The bundle is now %d key(s): %s. Usual cause is Magnum regenerating the keypair on a "+
		"full-template update and a master seeding from the new value",
		added, len(keys), strings.Join(ids, ", "))
}

// collectPeerSAKeys asks every other apiserver which service-account keys it
// verifies with. Failures are warnings, never errors: an unreachable peer must
// not fail the certificate phase, and the next reconcile will try again.
func collectPeerSAKeys(ctx context.Context, env saKeyEnv) ([][]byte, []string) {
	if env.fetchJWKS == nil || env.listPeerIPs == nil {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, saTotalTimeout)
	defer cancel()

	var warnings []string
	var keys [][]byte

	targets, err := env.listPeerIPs(ctx)
	if err != nil || len(targets) == 0 {
		// Without the endpoint list, sweep the API load balancer instead: it
		// round-robins across masters, so repeated probes on fresh
		// connections reach all of them with high probability.
		if env.apiEndpoint == "" {
			return nil, warnings
		}
		if err != nil && env.logger != nil {
			env.logger.Infof("master-certificates: cannot list apiserver endpoints (%v); sweeping the API load balancer instead", err)
		}
		attempts := max(4*env.numberOfMasters, 4)
		for range attempts {
			doc, err := env.fetchJWKS(ctx, env.apiEndpoint)
			if err != nil {
				continue
			}
			parsed, err := certutil.JWKSPublicKeyPEMs(doc)
			if err != nil {
				continue
			}
			keys = append(keys, parsed...)
		}
		return keys, warnings
	}

	for _, ip := range targets {
		if ip == "" || ip == env.selfIP {
			continue
		}
		doc, err := env.fetchJWKS(ctx, ip)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"could not read service-account keys from master %s (%v); its keys are not in this node's verify bundle yet", ip, err))
			continue
		}
		parsed, err := certutil.JWKSPublicKeyPEMs(doc)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("master %s served an unparseable JWKS document (%v)", ip, err))
			continue
		}
		keys = append(keys, parsed...)
	}

	return keys, warnings
}

// normalizeHeatPEM tolerates a heat-param value whose "\n" escapes were not
// expanded (decodeValue only unescapes double-quoted values, and a value that
// fails strconv.Unquote is passed through verbatim).
func normalizeHeatPEM(value string) string {
	if value == "" || strings.Contains(value, "\n") {
		return value
	}
	return strings.ReplaceAll(value, `\n`, "\n")
}

// newSAKeyEnv wires the production seams: mutual-TLS calls straight to the
// apiservers using the certificates this module has just written. Going direct
// rather than through kubectl keeps the phase independent of client-tools
// ordering and of a kubeconfig that may not exist yet.
func newSAKeyEnv(cfg config.Config, certDir string, executor *host.Executor, logger *logging.Logger) saKeyEnv {
	env := saKeyEnv{
		certDir:      certDir,
		executor:     executor,
		logger:       logger,
		heatParamKey: cfg.Shared.KubeServiceAccountKey,
		selfIP:       cfg.ResolveNodeIP(),
	}
	if cfg.Master != nil {
		env.numberOfMasters = cfg.Master.NumberOfMasters
		env.apiEndpoint = cfg.Master.KubeAPIPrivateAddress
	}

	retired, err := coord.LoadRetiredSAKeys()
	if err != nil {
		// Fail closed on the adoption side: without the registry we cannot
		// tell a retired key from a legitimate peer key, and re-adopting a
		// retired one would undo a rotation's withdrawal of trust.
		if logger != nil {
			logger.Warnf("master-certificates: cannot read the retired service-account key registry (%v); skipping peer key adoption this run", err)
		}
		return env
	}
	env.retiredKeyIDs = retired

	port := cfg.Shared.KubeAPIPort
	if port == 0 {
		port = 6443
	}

	client, err := saAPIClient(certDir)
	if err != nil {
		if logger != nil {
			logger.Infof("master-certificates: no client for apiserver probes (%v); skipping peer service-account key collection", err)
		}
		return env
	}

	get := func(ctx context.Context, host, path string) ([]byte, error) {
		url := fmt.Sprintf("https://%s%s", net.JoinHostPort(host, fmt.Sprintf("%d", port)), path)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return body, nil
	}

	env.fetchJWKS = func(ctx context.Context, host string) ([]byte, error) {
		return get(ctx, host, "/openid/v1/jwks")
	}

	env.listPeerIPs = func(ctx context.Context) ([]string, error) {
		// Ask the local apiserver first — it is the authority on the current
		// membership and needs no load balancer. Fall back to the VIP so a
		// master whose own apiserver is down still learns its peers.
		sources := []string{"127.0.0.1"}
		if env.apiEndpoint != "" {
			sources = append(sources, env.apiEndpoint)
		}
		var lastErr error
		for _, source := range sources {
			body, err := get(ctx, source, "/api/v1/namespaces/default/endpoints/kubernetes")
			if err != nil {
				lastErr = err
				continue
			}
			ips, err := parseEndpointIPs(body)
			if err != nil {
				lastErr = err
				continue
			}
			return ips, nil
		}
		return nil, lastErr
	}

	return env
}

func parseEndpointIPs(body []byte) ([]string, error) {
	var parsed struct {
		Subsets []struct {
			Addresses []struct {
				IP string `json:"ip"`
			} `json:"addresses"`
		} `json:"subsets"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse kubernetes endpoints: %w", err)
	}
	var ips []string
	for _, subset := range parsed.Subsets {
		for _, address := range subset.Addresses {
			if address.IP != "" {
				ips = append(ips, address.IP)
			}
		}
	}
	return ips, nil
}

// saAPIClient builds an mTLS client from the master's own admin credentials.
// Each apiserver's serving certificate carries its private IP and 127.0.0.1 in
// its SANs, so peers validate against the cluster CA without name juggling.
func saAPIClient(certDir string) (*http.Client, error) {
	caPEM, err := os.ReadFile(certDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no usable CA in %s/ca.crt", certDir)
	}
	cert, err := tls.LoadX509KeyPair(certDir+"/admin.crt", certDir+"/admin.key")
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
		// A fresh connection per probe: pooling would pin every request to
		// one backend, which is exactly what hides a split.
		DisableKeepAlives: true,
	}
	return &http.Client{Transport: transport, Timeout: saPeerFetchTimeout}, nil
}
