package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/rxbynerd/hairpin/internal/tokenissuer"
)

func TestRunKeygen(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	jwksPath := filepath.Join(dir, "jwks.json")

	if err := runKeygen([]string{"--out", keyPath, "--jwks-out", jwksPath}); err != nil {
		t.Fatalf("runKeygen: %v", err)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("stat key: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("key file mode = %v, want 0600", mode)
		}
	}

	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	priv, err := tokenissuer.ParsePrivateKeyPEM(pemBytes)
	if err != nil {
		t.Fatalf("parse generated key: %v", err)
	}

	jwksBytes, err := os.ReadFile(jwksPath)
	if err != nil {
		t.Fatalf("read jwks: %v", err)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(jwksBytes, &doc); err != nil {
		t.Fatalf("unmarshal jwks: %v", err)
	}
	if len(doc.Keys) != 1 {
		t.Fatalf("jwks has %d keys, want 1", len(doc.Keys))
	}

	wantKid, err := tokenissuer.KeyID(&priv.PublicKey)
	if err != nil {
		t.Fatalf("KeyID: %v", err)
	}
	if doc.Keys[0].Kid != wantKid {
		t.Errorf("jwks kid = %q, want %q (the generated key's own thumbprint)", doc.Keys[0].Kid, wantKid)
	}
}

func TestRunKeygenRequiresOut(t *testing.T) {
	if err := runKeygen(nil); err == nil {
		t.Fatal("runKeygen succeeded without --out")
	}
}
