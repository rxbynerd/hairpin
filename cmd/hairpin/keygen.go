package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rxbynerd/hairpin/internal/tokenissuer"
)

// runKeygen implements `hairpin keygen`: generates a fresh P-256 signing
// key for sandbox identity token issuance, writes it to --out, and
// writes the matching JWKS document to --jwks-out (or stdout). Deploy
// scripts use this to provision hairpin's signing-key Secret and
// haybale's jwksFile ConfigMap from the same key in one step.
func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "path to write the PEM-encoded ES256 private key (required)")
	jwksOut := fs.String("jwks-out", "", "path to write the JWKS JSON (empty: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("usage: hairpin keygen --out <path> [--jwks-out <path>]")
	}

	priv, err := tokenissuer.GenerateKey()
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	pemBytes, err := tokenissuer.EncodePrivateKeyPEM(priv)
	if err != nil {
		return fmt.Errorf("encode key: %w", err)
	}
	if err := writeNew(*out, pemBytes, 0o600); err != nil {
		return err
	}

	jwks, err := tokenissuer.JWKSDocument(&priv.PublicKey)
	if err != nil {
		return fmt.Errorf("build JWKS: %w", err)
	}
	if *jwksOut == "" {
		if _, err := fmt.Println(string(jwks)); err != nil {
			return err
		}
	} else if err := writeNew(*jwksOut, append(jwks, '\n'), 0o644); err != nil {
		return err
	}

	kid, err := tokenissuer.KeyID(&priv.PublicKey)
	if err != nil {
		return fmt.Errorf("compute kid: %w", err)
	}
	fmt.Fprintln(os.Stderr, "kid:", kid)
	return nil
}

// writeNew creates path exclusively with mode, so an existing key is
// never overwritten and a pre-existing file cannot lend the key its
// wider permissions.
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
