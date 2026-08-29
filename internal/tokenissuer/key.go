// Package tokenissuer mints and describes the ES256 JWT sandbox identity
// tokens hairpin issues on a harness's sandbox_token_request, and the
// JWKS document a git credential proxy (haybale) verifies them against.
// See docs/deployment.md#sandbox-identity-tokens.
package tokenissuer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// LoadKey reads and parses the ES256 signing key at path. It accepts a
// PEM-encoded SEC1 "EC PRIVATE KEY" block (openssl ecparam -genkey) or a
// PKCS#8 "PRIVATE KEY" block (openssl genpkey, and what GenerateKey
// produces); any other key type or curve is rejected.
func LoadKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	key, err := ParsePrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return key, nil
}

// ParsePrivateKeyPEM parses a single PEM-encoded EC private key, SEC1 or
// PKCS#8, and rejects anything not on the P-256 curve.
func ParsePrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}

	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse SEC1 EC private key: %w", err)
		}
		key = k
	case "PRIVATE KEY":
		raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKCS8 private key: %w", err)
		}
		k, ok := raw.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is %T, want *ecdsa.PrivateKey", raw)
		}
		key = k
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q (want EC PRIVATE KEY or PRIVATE KEY)", block.Type)
	}

	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("key curve is %s, want P-256", key.Curve.Params().Name)
	}
	return key, nil
}

// GenerateKey returns a fresh P-256 signing key.
func GenerateKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate P-256 key: %w", err)
	}
	return key, nil
}

// EncodePrivateKeyPEM encodes priv as a PKCS#8 "PRIVATE KEY" PEM block.
func EncodePrivateKeyPEM(priv *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
