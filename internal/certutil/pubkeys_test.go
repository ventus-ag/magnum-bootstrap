package certutil

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
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
