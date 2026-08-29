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
	if err := os.WriteFile(*out, pemBytes, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}

	jwks, err := tokenissuer.JWKSDocument(&priv.PublicKey)
	if err != nil {
		return fmt.Errorf("build JWKS: %w", err)
	}
	if *jwksOut == "" {
		if _, err := fmt.Println(string(jwks)); err != nil {
			return err
		}
	} else if err := os.WriteFile(*jwksOut, append(jwks, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *jwksOut, err)
	}

	kid, err := tokenissuer.KeyID(&priv.PublicKey)
	if err != nil {
		return fmt.Errorf("compute kid: %w", err)
	}
	fmt.Fprintln(os.Stderr, "kid:", kid)
	return nil
}
