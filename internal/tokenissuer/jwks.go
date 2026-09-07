package tokenissuer

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// coordSize is the encoded length in bytes of a P-256 field element.
const coordSize = 32

// jwk is one entry of a JWKS document: an EC public signing key in the
// shape haybale (and any standard JOSE consumer) expects.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// jwks is a JWKS document: RFC 7517 §5.
type jwks struct {
	Keys []jwk `json:"keys"`
}

// KeyID returns the RFC 7638 JWK thumbprint of pub: the base64url
// (unpadded) SHA-256 digest of its canonical JSON representation. It is
// deterministic in the key alone, so re-deploying the same key always
// serves the same kid.
func KeyID(pub *ecdsa.PublicKey) (string, error) {
	x, y, err := coords(pub)
	if err != nil {
		return "", err
	}
	// RFC 7638 canonical form: required EC members only, lexicographic
	// key order (crv, kty, x, y), no whitespace.
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// JWKSDocument returns the JWKS JSON exposing pub as an ES256 signing
// key, keyed by its KeyID.
func JWKSDocument(pub *ecdsa.PublicKey) ([]byte, error) {
	kid, err := KeyID(pub)
	if err != nil {
		return nil, err
	}
	x, y, err := coords(pub)
	if err != nil {
		return nil, err
	}
	doc := jwks{Keys: []jwk{{
		Kty: "EC",
		Crv: "P-256",
		X:   x,
		Y:   y,
		Kid: kid,
		Alg: "ES256",
		Use: "sig",
	}}}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode JWKS: %w", err)
	}
	return b, nil
}

// coords returns pub's X and Y coordinates as fixed-width, unpadded
// base64url strings, rejecting anything not on the P-256 curve.
func coords(pub *ecdsa.PublicKey) (x, y string, err error) {
	if pub == nil || pub.Curve == nil || pub.Curve.Params().Name != "P-256" {
		return "", "", fmt.Errorf("key is not on the P-256 curve")
	}
	// Bytes yields the uncompressed SEC 1 point: 0x04 || X || Y.
	raw, err := pub.Bytes()
	if err != nil {
		return "", "", fmt.Errorf("encode public key: %w", err)
	}
	if len(raw) != 1+2*coordSize || raw[0] != 0x04 {
		return "", "", fmt.Errorf("unexpected public key encoding (%d bytes)", len(raw))
	}
	xb, yb := raw[1:1+coordSize], raw[1+coordSize:]
	return base64.RawURLEncoding.EncodeToString(xb), base64.RawURLEncoding.EncodeToString(yb), nil
}
