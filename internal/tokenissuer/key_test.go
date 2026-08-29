package tokenissuer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePrivateKeyPEM(t *testing.T) {
	p256SEC1 := mustPEM(t, mustP256(t), "EC PRIVATE KEY", false)
	p256PKCS8 := mustPEM(t, mustP256(t), "PRIVATE KEY", true)
	p384PKCS8 := mustPEM(t, mustCurveKey(t, elliptic.P384()), "PRIVATE KEY", true)

	cases := []struct {
		name    string
		pem     []byte
		wantErr string
	}{
		{"SEC1 P-256", p256SEC1, ""},
		{"PKCS8 P-256", p256PKCS8, ""},
		{"PKCS8 P-384 rejected", p384PKCS8, "P-256"},
		{"garbage", []byte("not a pem"), "no PEM block"},
		{"wrong block type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}), "unsupported PEM block type"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := ParsePrivateKeyPEM(tc.pem)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParsePrivateKeyPEM: %v", err)
				}
				if key.Curve != elliptic.P256() {
					t.Errorf("curve = %s, want P-256", key.Curve.Params().Name)
				}
				return
			}
			if err == nil {
				t.Fatal("ParsePrivateKeyPEM succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, mustPEM(t, mustP256(t), "PRIVATE KEY", true), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	key, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if key.Curve != elliptic.P256() {
		t.Errorf("curve = %s, want P-256", key.Curve.Params().Name)
	}
}

func TestLoadKeyMissingFile(t *testing.T) {
	if _, err := LoadKey(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("LoadKey succeeded on a missing file")
	}
}

func TestGenerateKeyRoundTrip(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if priv.Curve != elliptic.P256() {
		t.Fatalf("curve = %s, want P-256", priv.Curve.Params().Name)
	}

	encoded, err := EncodePrivateKeyPEM(priv)
	if err != nil {
		t.Fatalf("EncodePrivateKeyPEM: %v", err)
	}
	block, _ := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("encoded PEM block = %+v, want type PRIVATE KEY", block)
	}

	parsed, err := ParsePrivateKeyPEM(encoded)
	if err != nil {
		t.Fatalf("ParsePrivateKeyPEM(round trip): %v", err)
	}
	if parsed.D.Cmp(priv.D) != 0 {
		t.Error("round-tripped private scalar does not match the generated key")
	}
}

// mustP256 returns a fresh P-256 key for test fixtures.
func mustP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	return mustCurveKey(t, elliptic.P256())
}

func mustCurveKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", curve.Params().Name, err)
	}
	return key
}

// mustPEM encodes key as either a SEC1 "EC PRIVATE KEY" or PKCS#8
// "PRIVATE KEY" PEM block.
func mustPEM(t *testing.T, key *ecdsa.PrivateKey, blockType string, pkcs8 bool) []byte {
	t.Helper()
	var der []byte
	var err error
	if pkcs8 {
		der, err = x509.MarshalPKCS8PrivateKey(key)
	} else {
		der, err = x509.MarshalECPrivateKey(key)
	}
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
