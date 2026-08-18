package certutil

import (
	"crypto"
	"crypto/ecdh"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
)

// This file deals with *public* key material only — specifically the service
// account verification keys kube-apiserver loads from
// --service-account-key-file. That flag accepts a bundle of PEM blocks, which
// is how a cluster can trust more than one SA signing key at a time (CA
// rotation already relies on this: it writes a new+old two-key file).

// PublicKeyPEM encodes a public key as a PKIX "PUBLIC KEY" PEM block.
func PublicKeyPEM(pub crypto.PublicKey) ([]byte, error) {
	der, err := marshalPKIX(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// PublicKeyPEMFromPrivateKeyFile reads a private key and returns its public
// half as a PKIX PEM block. Used to learn which SA key the local
// kube-controller-manager signs with.
func PublicKeyPEMFromPrivateKeyFile(path string) ([]byte, error) {
	key, err := loadPrivateKey(path)
	if err != nil {
		return nil, err
	}
	pub := publicKey(key)
	if pub == nil {
		return nil, fmt.Errorf("unsupported private key type in %s", path)
	}
	return PublicKeyPEM(pub)
}

// ParsePublicKeyPEMs splits a PEM bundle into individually re-encoded PKIX
// public key blocks, in file order, skipping anything that does not parse.
// Re-encoding normalises formatting (headers, line wrapping, PKCS#1 vs PKIX) so
// two files carrying the same keys compare equal byte for byte.
//
// A malformed block is skipped rather than fatal: this material comes partly
// from peers over the network, and one bad key must never cost us the good ones.
func ParsePublicKeyPEMs(data []byte) [][]byte {
	var out [][]byte
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		pub, err := parsePublicKeyDER(block)
		if err != nil {
			continue
		}
		encoded, err := PublicKeyPEM(pub)
		if err != nil {
			continue
		}
		out = append(out, encoded)
	}
	return out
}

// PublicKeyID returns the key ID kube-apiserver publishes for a PEM-encoded
// public key at /openid/v1/jwks: base64url(SHA-256(PKIX DER)), unpadded. Having
// the same identifier the API serves makes a logged warning directly
// comparable with what an operator sees in the JWKS document.
func PublicKeyID(pubPEM []byte) (string, error) {
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return "", fmt.Errorf("invalid public key PEM")
	}
	pub, err := parsePublicKeyDER(block)
	if err != nil {
		return "", err
	}
	der, err := marshalPKIX(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// JWKSPublicKeyPEMs converts a JWKS document (as served by an apiserver at
// /openid/v1/jwks) into PKIX PEM public keys. Unsupported or malformed keys are
// skipped; a document that is not JSON at all is an error.
func JWKSPublicKeyPEMs(doc []byte) ([][]byte, error) {
	var parsed struct {
		Keys []struct {
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}

	var out [][]byte
	for _, key := range parsed.Keys {
		pub, err := jwkPublicKey(key.Kty, key.N, key.E, key.Crv, key.X, key.Y)
		if err != nil {
			continue
		}
		encoded, err := PublicKeyPEM(pub)
		if err != nil {
			continue
		}
		out = append(out, encoded)
	}
	return out, nil
}

func marshalPKIX(pub crypto.PublicKey) ([]byte, error) {
	if pub == nil {
		return nil, fmt.Errorf("nil public key")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	return der, nil
}

// parsePublicKeyDER accepts the PEM block types that can legitimately carry an
// SA verification key: PKIX ("PUBLIC KEY"), PKCS#1 ("RSA PUBLIC KEY"), and a
// certificate, whose SubjectPublicKeyInfo is used.
func parsePublicKeyDER(block *pem.Block) (crypto.PublicKey, error) {
	switch block.Type {
	case "RSA PUBLIC KEY":
		return x509.ParsePKCS1PublicKey(block.Bytes)
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		return cert.PublicKey, nil
	default:
		return x509.ParsePKIXPublicKey(block.Bytes)
	}
}

func jwkPublicKey(kty, n, e, crv, x, y string) (crypto.PublicKey, error) {
	switch kty {
	case "RSA":
		modulus, err := b64uBigInt(n)
		if err != nil {
			return nil, err
		}
		exponent, err := b64uBigInt(e)
		if err != nil {
			return nil, err
		}
		if !exponent.IsInt64() || exponent.Int64() <= 0 {
			return nil, fmt.Errorf("jwk: bad RSA exponent")
		}
		return &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}, nil
	case "EC":
		var curve ecdh.Curve
		var size int
		switch crv {
		case "P-256":
			curve, size = ecdh.P256(), 32
		case "P-384":
			curve, size = ecdh.P384(), 48
		case "P-521":
			curve, size = ecdh.P521(), 66
		default:
			return nil, fmt.Errorf("jwk: unsupported curve %q", crv)
		}
		xRaw, err := b64uBytes(x)
		if err != nil {
			return nil, err
		}
		yRaw, err := b64uBytes(y)
		if err != nil {
			return nil, err
		}
		if len(xRaw) > size || len(yRaw) > size {
			return nil, fmt.Errorf("jwk: coordinate too wide for curve %s", crv)
		}
		// Uncompressed SEC 1 point, left-padded to the curve's field size.
		// NewPublicKey performs the on-curve check for us.
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(xRaw):1+size], xRaw)
		copy(point[1+2*size-len(yRaw):], yRaw)
		return curve.NewPublicKey(point)
	default:
		return nil, fmt.Errorf("jwk: unsupported key type %q", kty)
	}
}

func b64uBytes(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("jwk: empty field")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		// Tolerate padded encodings even though RFC 7515 forbids them.
		raw, err = base64.URLEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("jwk: decode field: %w", err)
		}
	}
	return raw, nil
}

func b64uBigInt(value string) (*big.Int, error) {
	raw, err := b64uBytes(value)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(raw), nil
}

// CapPublicKeyBundle keeps at most max distinct public keys from a PEM bundle,
// preserving order, and returns the key IDs it dropped.
//
// Ordering is the whole trick: every rotation PREPENDS its new key, so position
// zero is always the newest and "keep the first max" is exactly "keep the newest
// max". Public keys carry no timestamps, so there is nothing else to sort by.
//
// max <= 0 means unlimited.
//
// If nothing in the input parses as a public key the ORIGINAL bundle is
// returned untouched. Emptying service_account.key would invalidate every
// ServiceAccount token in the cluster, so an unrecognised bundle is always left
// alone rather than replaced with a shorter one we are more confident about.
func CapPublicKeyBundle(bundle []byte, max int) ([]byte, []string) {
	keys := ParsePublicKeyPEMs(bundle)
	if len(keys) == 0 {
		return bundle, nil
	}

	var kept []byte
	var dropped []string
	seen := map[string]bool{}
	count := 0
	for _, key := range keys {
		id, err := PublicKeyID(key)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		if max > 0 && count >= max {
			dropped = append(dropped, id)
			continue
		}
		kept = append(kept, key...)
		count++
	}
	return kept, dropped
}

// CapCertBundle keeps at most max distinct certificates from a PEM bundle,
// preserving order, and reports how many it dropped. Same newest-first ordering
// contract as CapPublicKeyBundle, and the same refusal to replace a bundle it
// could not parse: no certificates recognised means the input is returned
// untouched.
func CapCertBundle(bundle []byte, max int) ([]byte, int) {
	var kept []byte
	parsed := 0
	seen := map[string]bool{}
	dropped := 0
	count := 0

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
		if err != nil {
			continue
		}
		parsed++
		fingerprint := string(sha256Sum(cert.Raw))
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		if max > 0 && count >= max {
			dropped++
			continue
		}
		kept = append(kept, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})...)
		count++
	}
	if parsed == 0 {
		return bundle, 0
	}
	return kept, dropped
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// PublicKeyPEMFile reads a PEM bundle from disk and returns its normalised
// public key blocks. A missing file yields no keys and no error — an absent
// bundle is a legitimate starting state.
func PublicKeyPEMFile(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read public key bundle %s: %w", path, err)
	}
	return ParsePublicKeyPEMs(data), nil
}
