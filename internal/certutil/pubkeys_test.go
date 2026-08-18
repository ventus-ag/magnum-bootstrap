package certutil

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PublicKeyID must reproduce the `kid` kube-apiserver publishes at
// /openid/v1/jwks — that is what makes a logged warning directly comparable
// with what an operator reads out of the API. The derivation (unpadded
// base64url of SHA-256 over the PKIX DER) was checked against both keys of a
// real split cluster and matched exactly; here we pin the shape and stability,
// since a JWKS round-trip is covered by TestJWKSPublicKeyPEMsRSAAndEC.
func TestPublicKeyIDShapeAndStability(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pubPEM, err := PublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	id, err := PublicKeyID(pubPEM)
	if err != nil {
		t.Fatalf("key id: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		t.Fatalf("key id is not unpadded base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("key id should be a SHA-256 digest, got %d bytes", len(raw))
	}
	again, err := PublicKeyID(pubPEM)
	if err != nil || again != id {
		t.Fatalf("key id is not stable: %v %v", again, err)
	}
	if _, err := PublicKeyID([]byte("not a pem")); err == nil {
		t.Error("expected an error on garbage input")
	}
}

func TestJWKSPublicKeyPEMsRSAAndEC(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}
	ecKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ec: %v", err)
	}
	// Uncompressed SEC 1 point: 0x04 || X || Y.
	ecPoint := ecKey.PublicKey().Bytes()

	doc := fmt.Sprintf(`{"keys":[
      {"kty":"RSA","n":%q,"e":%q},
      {"kty":"EC","crv":"P-256","x":%q,"y":%q},
      {"kty":"OKP","crv":"Ed25519","x":"AAAA"},
      {"kty":"RSA","n":"!!!not-base64!!!","e":"AQAB"}
    ]}`,
		base64.RawURLEncoding.EncodeToString(rsaKey.PublicKey.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.PublicKey.E)).Bytes()),
		base64.RawURLEncoding.EncodeToString(ecPoint[1:33]),
		base64.RawURLEncoding.EncodeToString(ecPoint[33:]),
	)

	keys, err := JWKSPublicKeyPEMs([]byte(doc))
	if err != nil {
		t.Fatalf("convert jwks: %v", err)
	}
	// The unsupported and malformed entries are skipped, not fatal — one bad
	// key from a peer must never cost us the good ones.
	if len(keys) != 2 {
		t.Fatalf("expected 2 usable keys, got %d", len(keys))
	}

	wantRSA, err := PublicKeyPEM(&rsaKey.PublicKey)
	if err != nil {
		t.Fatalf("encode rsa: %v", err)
	}
	if string(keys[0]) != string(wantRSA) {
		t.Error("RSA key did not round-trip through JWK")
	}
	wantEC, err := PublicKeyPEM(ecKey.PublicKey())
	if err != nil {
		t.Fatalf("encode ec: %v", err)
	}
	if string(keys[1]) != string(wantEC) {
		t.Error("EC key did not round-trip through JWK")
	}

	if _, err := JWKSPublicKeyPEMs([]byte("<html>")); err == nil {
		t.Error("expected an error on a non-JSON document")
	}
}

func TestParsePublicKeyPEMsNormalisesBlockTypes(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pkix, err := PublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PUBLIC KEY",
		Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey),
	})

	bundle := append(append([]byte("noise\n"), pkcs1...), pkix...)
	keys := ParsePublicKeyPEMs(bundle)
	if len(keys) != 2 {
		t.Fatalf("expected both blocks, got %d", len(keys))
	}
	// Both encodings of the same key normalise to identical PKIX bytes, which
	// is what lets the bundle dedupe across formats.
	if string(keys[0]) != string(keys[1]) {
		t.Error("PKCS#1 and PKIX encodings of one key did not normalise equal")
	}
}

func TestPublicKeyPEMFileMissingIsNotAnError(t *testing.T) {
	keys, err := PublicKeyPEMFile(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("a missing bundle is a legitimate starting state: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected no keys, got %d", len(keys))
	}
}

func TestPublicKeyPEMFromPrivateKeyFile(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "service_account_private.key")
	body := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := PublicKeyPEMFromPrivateKeyFile(path)
	if err != nil {
		t.Fatalf("derive public half: %v", err)
	}
	want, err := PublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(got) != string(want) {
		t.Error("public half does not match the private key")
	}

	if _, err := PublicKeyPEMFromPrivateKeyFile(filepath.Join(dir, "absent")); err == nil {
		t.Error("expected an error for a missing private key")
	}
}

func testPubPEM(t *testing.T) ([]byte, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	pemBytes, err := PublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	id, err := PublicKeyID(pemBytes)
	if err != nil {
		t.Fatalf("key id: %v", err)
	}
	return pemBytes, id
}

func TestCapPublicKeyBundleKeepsNewestFirst(t *testing.T) {
	// Each rotation prepends its new key, so bundle order IS age order.
	k1, id1 := testPubPEM(t)
	k2, id2 := testPubPEM(t)
	k3, id3 := testPubPEM(t)
	k4, id4 := testPubPEM(t)

	var bundle []byte
	for _, k := range [][]byte{k1, k2, k3, k4} {
		bundle = append(bundle, k...)
	}

	capped, dropped := CapPublicKeyBundle(bundle, 3)
	if len(dropped) != 1 || dropped[0] != id4 {
		t.Fatalf("expected the oldest key evicted, got %v", dropped)
	}
	got := ParsePublicKeyPEMs(capped)
	if len(got) != 3 {
		t.Fatalf("expected 3 keys kept, got %d", len(got))
	}
	for i, want := range []string{id1, id2, id3} {
		id, err := PublicKeyID(got[i])
		if err != nil || id != want {
			t.Errorf("position %d: got %v want %s", i, id, want)
		}
	}
}

func TestCapPublicKeyBundleDedupesAndIsIdempotent(t *testing.T) {
	k1, _ := testPubPEM(t)
	k2, _ := testPubPEM(t)

	// The same key appearing twice must not consume two generations.
	bundle := append(append(append([]byte{}, k1...), k2...), k1...)
	capped, dropped := CapPublicKeyBundle(bundle, 3)
	if len(dropped) != 0 {
		t.Fatalf("duplicates are not evictions, got %v", dropped)
	}
	if n := len(ParsePublicKeyPEMs(capped)); n != 2 {
		t.Fatalf("expected 2 distinct keys, got %d", n)
	}

	again, dropped := CapPublicKeyBundle(capped, 3)
	if len(dropped) != 0 || string(again) != string(capped) {
		t.Error("capping an already-capped bundle must be a no-op")
	}
}

// Emptying service_account.key would invalidate every ServiceAccount token in
// the cluster, so an unparseable bundle is left exactly as it is.
func TestCapBundlesRefuseToEmptyUnparseableInput(t *testing.T) {
	junk := []byte("NEW-SA\nOLD-SA\n")

	capped, dropped := CapPublicKeyBundle(junk, 1)
	if string(capped) != string(junk) || dropped != nil {
		t.Errorf("public key bundle mangled: %q %v", capped, dropped)
	}

	certCapped, certDropped := CapCertBundle(junk, 1)
	if string(certCapped) != string(junk) || certDropped != 0 {
		t.Errorf("cert bundle mangled: %q %d", certCapped, certDropped)
	}
}

func TestCapPublicKeyBundleUnlimited(t *testing.T) {
	k1, _ := testPubPEM(t)
	k2, _ := testPubPEM(t)
	bundle := append(append([]byte{}, k1...), k2...)

	_, dropped := CapPublicKeyBundle(bundle, 0)
	if len(dropped) != 0 {
		t.Errorf("max<=0 means unlimited, got %v", dropped)
	}
}

func TestCapCertBundleKeepsNewestFirst(t *testing.T) {
	mk := func(cn string) []byte {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               pkix.Name{CommonName: cn},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(time.Hour),
			IsCA:                  true,
			BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatalf("create cert: %v", err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}

	var bundle []byte
	for _, cn := range []string{"newest", "middle", "older", "oldest"} {
		bundle = append(bundle, mk(cn)...)
	}

	capped, dropped := CapCertBundle(bundle, 3)
	if dropped != 1 {
		t.Fatalf("expected 1 cert dropped, got %d", dropped)
	}
	certs, err := loadCACerts2(capped)
	if err != nil {
		t.Fatalf("parse capped: %v", err)
	}
	if len(certs) != 3 {
		t.Fatalf("expected 3 certs, got %d", len(certs))
	}
	if certs[0].Subject.CommonName != "newest" || certs[2].Subject.CommonName != "older" {
		t.Errorf("wrong certs kept: %s..%s", certs[0].Subject.CommonName, certs[2].Subject.CommonName)
	}
}

// loadCACerts2 is a local parser so the test does not depend on unexported
// helpers changing shape.
func loadCACerts2(bundle []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out, nil
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
}
