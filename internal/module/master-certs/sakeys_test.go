package mastercerts

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coord "github.com/ventus-ag/magnum-bootstrap/internal/carotation"
	"github.com/ventus-ag/magnum-bootstrap/internal/certutil"
	"github.com/ventus-ag/magnum-bootstrap/internal/host"
)

// testKey is a generated RSA keypair with the shapes the code under test uses.
type testKey struct {
	private *rsa.PrivateKey
	pubPEM  []byte
	privPEM []byte
	id      string
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubPEM, err := certutil.PublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatalf("encode public key: %v", err)
	}
	id, err := certutil.PublicKeyID(pubPEM)
	if err != nil {
		t.Fatalf("key id: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return testKey{private: key, pubPEM: pubPEM, privPEM: privPEM, id: id}
}

// jwksDoc renders the keys as an apiserver would serve them at
// /openid/v1/jwks.
func jwksDoc(t *testing.T, keys ...testKey) []byte {
	t.Helper()
	type jwk struct {
		Use string `json:"use"`
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	doc := struct {
		Keys []jwk `json:"keys"`
	}{}
	for _, key := range keys {
		doc.Keys = append(doc.Keys, jwk{
			Use: "sig", Kty: "RSA", Kid: key.id, Alg: "RS256",
			N: base64.RawURLEncoding.EncodeToString(key.private.PublicKey.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.private.PublicKey.E)).Bytes()),
		})
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return body
}

// certDirWith lays out a master's cert dir with the given verify bundle and
// signing key.
func certDirWith(t *testing.T, bundle []byte, signing testKey) string {
	t.Helper()
	dir := t.TempDir()
	if bundle != nil {
		if err := os.WriteFile(filepath.Join(dir, saVerifyKeyFile), bundle, 0o600); err != nil {
			t.Fatalf("write bundle: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, saPrivateKeyFile), signing.privPEM, 0o600); err != nil {
		t.Fatalf("write signing key: %v", err)
	}
	return dir
}

func envFor(dir string, peers map[string][]byte) saKeyEnv {
	ips := make([]string, 0, len(peers))
	for ip := range peers {
		ips = append(ips, ip)
	}
	return saKeyEnv{
		certDir:         dir,
		executor:        host.NewExecutor(true, nil),
		selfIP:          "10.0.0.1",
		numberOfMasters: 3,
		listPeerIPs: func(context.Context) ([]string, error) {
			return ips, nil
		},
		fetchJWKS: func(_ context.Context, host string) ([]byte, error) {
			doc, ok := peers[host]
			if !ok {
				return nil, fmt.Errorf("connection refused")
			}
			return doc, nil
		},
	}
}

func bundleIDs(t *testing.T, path string) []string {
	t.Helper()
	keys, err := certutil.PublicKeyPEMFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		id, err := certutil.PublicKeyID(key)
		if err != nil {
			t.Fatalf("key id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// The bug this whole file exists for: a master added later carries its own
// service-account keypair, so tokens minted by its peers come back 401. After
// convergence it verifies with both keys.
func TestConvergeAdoptsPeerKey(t *testing.T) {
	local := newTestKey(t)
	peer := newTestKey(t)

	dir := certDirWith(t, local.pubPEM, local)
	env := envFor(dir, map[string][]byte{"10.0.0.2": jwksDoc(t, peer)})

	changes, warnings, err := convergeSAVerifyKeys(context.Background(), env)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected the bundle to be rewritten")
	}

	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	if len(ids) != 2 {
		t.Fatalf("expected 2 keys in the bundle, got %d (%v)", len(ids), ids)
	}
	if ids[0] != local.id {
		t.Errorf("local key should stay first, got %s want %s", ids[0], local.id)
	}
	if ids[1] != peer.id {
		t.Errorf("peer key missing, got %s want %s", ids[1], peer.id)
	}

	var reported bool
	for _, warning := range warnings {
		if strings.Contains(warning, "adopted 1 service-account verification key") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("split should be reported as a warning, got %v", warnings)
	}

	// Second pass over a converged node must be a no-op, or every reconcile
	// would rewrite the file and bounce kube-apiserver.
	changes, _, err = convergeSAVerifyKeys(context.Background(), envFor(dir, map[string][]byte{"10.0.0.2": jwksDoc(t, peer)}))
	if err != nil {
		t.Fatalf("second converge: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("converged node should report no changes, got %v", changes)
	}
}

// The heat-param key is a source too: it is what a freshly built master would
// seed from, so an existing master should trust it pre-emptively.
func TestConvergeAdoptsHeatParamKey(t *testing.T) {
	local := newTestKey(t)
	fromHeat := newTestKey(t)

	dir := certDirWith(t, local.pubPEM, local)
	env := envFor(dir, nil)
	// heat-params carries the PEM with escaped newlines when the value could
	// not be unquoted.
	env.heatParamKey = strings.ReplaceAll(string(fromHeat.pubPEM), "\n", `\n`)

	if _, _, err := convergeSAVerifyKeys(context.Background(), env); err != nil {
		t.Fatalf("converge: %v", err)
	}

	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	if len(ids) != 2 || ids[1] != fromHeat.id {
		t.Fatalf("heat-param key not adopted: %v", ids)
	}
}

// Removing a key invalidates every token signed with it. Convergence is
// strictly additive, even when peers no longer advertise a key we hold.
func TestConvergeNeverShrinksBundle(t *testing.T) {
	local := newTestKey(t)
	retired := newTestKey(t)

	bundle := append(append([]byte{}, local.pubPEM...), retired.pubPEM...)
	dir := certDirWith(t, bundle, local)
	env := envFor(dir, map[string][]byte{"10.0.0.2": jwksDoc(t, local)})

	changes, _, err := convergeSAVerifyKeys(context.Background(), env)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("nothing new to add, expected no changes, got %v", changes)
	}

	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	if len(ids) != 2 {
		t.Fatalf("bundle shrank to %v", ids)
	}
}

// One bad peer must not cost us the good ones, and must not fail the
// certificate phase.
func TestConvergeToleratesBadPeers(t *testing.T) {
	local := newTestKey(t)
	good := newTestKey(t)

	dir := certDirWith(t, local.pubPEM, local)
	env := envFor(dir, map[string][]byte{
		"10.0.0.2": jwksDoc(t, good),
		"10.0.0.3": []byte("<html>not json</html>"),
	})

	_, warnings, err := convergeSAVerifyKeys(context.Background(), env)
	if err != nil {
		t.Fatalf("a malformed peer response must not fail the phase: %v", err)
	}

	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	if len(ids) != 2 || ids[1] != good.id {
		t.Fatalf("good peer key lost because another peer misbehaved: %v", ids)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning about the unparseable peer")
	}
}

func TestConvergeReportsUnreachablePeer(t *testing.T) {
	local := newTestKey(t)
	dir := certDirWith(t, local.pubPEM, local)

	env := envFor(dir, nil)
	env.listPeerIPs = func(context.Context) ([]string, error) { return []string{"10.0.0.9"}, nil }

	_, warnings, err := convergeSAVerifyKeys(context.Background(), env)
	if err != nil {
		t.Fatalf("an unreachable peer must not fail the phase: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "10.0.0.9") {
		t.Fatalf("expected one warning naming the unreachable peer, got %v", warnings)
	}
}

// A first boot has no bundle yet; seeding it is writeSAKeyIfAbsent's job.
// Building one out of peer keys alone would produce a bundle missing this
// node's own signing key.
func TestConvergeSkipsWhenNoLocalBundle(t *testing.T) {
	local := newTestKey(t)
	peer := newTestKey(t)

	dir := certDirWith(t, nil, local)
	env := envFor(dir, map[string][]byte{"10.0.0.2": jwksDoc(t, peer)})

	changes, warnings, err := convergeSAVerifyKeys(context.Background(), env)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(changes) != 0 || len(warnings) != 0 {
		t.Fatalf("expected a no-op without a local bundle, got %v / %v", changes, warnings)
	}
	if _, err := os.Stat(filepath.Join(dir, saVerifyKeyFile)); !os.IsNotExist(err) {
		t.Error("no bundle should have been created")
	}
}

// A single-master cluster has one key and nothing to reconcile with.
func TestConvergeSingleMasterIsNoOp(t *testing.T) {
	local := newTestKey(t)
	dir := certDirWith(t, local.pubPEM, local)

	env := envFor(dir, nil)
	env.numberOfMasters = 1
	env.listPeerIPs = func(context.Context) ([]string, error) { return []string{"10.0.0.1"}, nil }

	changes, warnings, err := convergeSAVerifyKeys(context.Background(), env)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no changes, got %v", changes)
	}
	if len(warnings) != 0 {
		t.Errorf("a single-key cluster is not a split, got %v", warnings)
	}
}

// A bundle whose signing key went missing from it self-repairs from the local
// private key, so this master keeps accepting the tokens it issues itself.
func TestConvergeReaddsOwnSigningKey(t *testing.T) {
	local := newTestKey(t)
	other := newTestKey(t)

	dir := certDirWith(t, other.pubPEM, local)
	env := envFor(dir, nil)

	if _, _, err := convergeSAVerifyKeys(context.Background(), env); err != nil {
		t.Fatalf("converge: %v", err)
	}

	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	var found bool
	for _, id := range ids {
		if id == local.id {
			found = true
		}
	}
	if !found {
		t.Fatalf("own signing key not restored into the bundle: %v", ids)
	}
}

// Garbage in the bundle is skipped, not fatal, and does not survive the
// rewrite.
func TestConvergeDropsUnparseableBundleEntries(t *testing.T) {
	local := newTestKey(t)
	peer := newTestKey(t)

	junk := []byte("-----BEGIN PUBLIC KEY-----\nbm90LWEta2V5\n-----END PUBLIC KEY-----\n")
	dir := certDirWith(t, append(append([]byte{}, junk...), local.pubPEM...), local)
	env := envFor(dir, map[string][]byte{"10.0.0.2": jwksDoc(t, peer)})

	if _, _, err := convergeSAVerifyKeys(context.Background(), env); err != nil {
		t.Fatalf("converge: %v", err)
	}

	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	if len(ids) != 2 {
		t.Fatalf("expected local+peer keys only, got %v", ids)
	}
}

func TestMergeSAVerifyKeysOrderingIsStable(t *testing.T) {
	first := newTestKey(t)
	second := newTestKey(t)
	third := newTestKey(t)

	existing := [][]byte{first.pubPEM}
	merged, added := mergeSAVerifyKeys(existing, [][]byte{third.pubPEM, second.pubPEM, first.pubPEM})
	if added != 2 {
		t.Fatalf("expected 2 additions, got %d", added)
	}

	// Existing keys keep file order; new keys are sorted by key ID so the
	// rendered file is byte-identical no matter what order peers answered in.
	reversed, addedAgain := mergeSAVerifyKeys(existing, [][]byte{second.pubPEM, third.pubPEM})
	if addedAgain != 2 {
		t.Fatalf("expected 2 additions, got %d", addedAgain)
	}
	for i := range merged {
		if string(merged[i]) != string(reversed[i]) {
			t.Fatalf("merge order depends on input order at index %d", i)
		}
	}
}

func TestNormalizeHeatPEM(t *testing.T) {
	real := "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
	if got := normalizeHeatPEM(real); got != real {
		t.Errorf("an already-expanded value must be left alone")
	}
	escaped := `-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n`
	if got := normalizeHeatPEM(escaped); got != real {
		t.Errorf("escaped newlines not expanded: %q", got)
	}
	if got := normalizeHeatPEM(""); got != "" {
		t.Errorf("empty value should stay empty, got %q", got)
	}
}

func TestParseEndpointIPs(t *testing.T) {
	body := []byte(`{"subsets":[{"addresses":[{"ip":"10.0.0.1"},{"ip":"10.0.0.2"}]},{"addresses":[{"ip":"10.0.0.3"}]}]}`)
	ips, err := parseEndpointIPs(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ips) != 3 || ips[2] != "10.0.0.3" {
		t.Fatalf("unexpected endpoints: %v", ips)
	}
	if _, err := parseEndpointIPs([]byte("nope")); err == nil {
		t.Error("expected an error on a non-JSON body")
	}
}

// A rotation's finalize barrier narrows the bundle to the new key on purpose.
// A peer that has not finalized yet still publishes the retired key, and
// adopting it back would silently undo the withdrawal of trust -- which for a
// compromise-driven rotation is the entire point of rotating.
func TestConvergeDoesNotResurrectRetiredKeys(t *testing.T) {
	defer coord.SetBaseDir(t.TempDir())()

	current := newTestKey(t)
	retired := newTestKey(t)
	if err := coord.RecordRetiredSAKeys([]string{retired.id}); err != nil {
		t.Fatalf("record retired: %v", err)
	}
	loaded, err := coord.LoadRetiredSAKeys()
	if err != nil {
		t.Fatalf("load retired: %v", err)
	}

	dir := certDirWith(t, current.pubPEM, current)
	env := envFor(dir, map[string][]byte{
		// A lagging peer still advertising both.
		"10.0.0.2": jwksDoc(t, current, retired),
	})
	env.retiredKeyIDs = loaded

	changes, _, err := convergeSAVerifyKeys(context.Background(), env)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("nothing adoptable, expected no rewrite, got %v", changes)
	}

	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	if len(ids) != 1 || ids[0] != current.id {
		t.Fatalf("retired key was resurrected: %v", ids)
	}
}

// A retired key is also not adopted from heat-params.
func TestConvergeIgnoresRetiredHeatParamKey(t *testing.T) {
	defer coord.SetBaseDir(t.TempDir())()

	current := newTestKey(t)
	retired := newTestKey(t)
	if err := coord.RecordRetiredSAKeys([]string{retired.id}); err != nil {
		t.Fatalf("record retired: %v", err)
	}
	loaded, _ := coord.LoadRetiredSAKeys()

	dir := certDirWith(t, current.pubPEM, current)
	env := envFor(dir, nil)
	env.heatParamKey = string(retired.pubPEM)
	env.retiredKeyIDs = loaded

	if _, _, err := convergeSAVerifyKeys(context.Background(), env); err != nil {
		t.Fatalf("converge: %v", err)
	}
	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	if len(ids) != 1 || ids[0] != current.id {
		t.Fatalf("retired heat-param key was adopted: %v", ids)
	}
}

// The node's own signing key is exempt: a key we are actively signing with
// cannot be one we have withdrawn trust from, and dropping it would make the
// master reject its own tokens.
func TestConvergeKeepsOwnSigningKeyEvenIfMarkedRetired(t *testing.T) {
	defer coord.SetBaseDir(t.TempDir())()

	own := newTestKey(t)
	if err := coord.RecordRetiredSAKeys([]string{own.id}); err != nil {
		t.Fatalf("record retired: %v", err)
	}
	loaded, _ := coord.LoadRetiredSAKeys()

	other := newTestKey(t)
	dir := certDirWith(t, other.pubPEM, own)
	env := envFor(dir, nil)
	env.retiredKeyIDs = loaded

	if _, _, err := convergeSAVerifyKeys(context.Background(), env); err != nil {
		t.Fatalf("converge: %v", err)
	}
	ids := bundleIDs(t, filepath.Join(dir, saVerifyKeyFile))
	found := false
	for _, id := range ids {
		if id == own.id {
			found = true
		}
	}
	if !found {
		t.Fatalf("own signing key must survive the retired filter: %v", ids)
	}
}

// Retirement is permanent and additive across rotations.
func TestRecordRetiredSAKeysAccumulates(t *testing.T) {
	defer coord.SetBaseDir(t.TempDir())()

	if err := coord.RecordRetiredSAKeys([]string{"aaa"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := coord.RecordRetiredSAKeys([]string{"bbb", "aaa"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	loaded, err := coord.LoadRetiredSAKeys()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 || !loaded["aaa"] || !loaded["bbb"] {
		t.Fatalf("unexpected registry contents: %v", loaded)
	}
}
