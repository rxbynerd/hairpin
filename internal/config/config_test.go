package config

import (
	"strings"
	"testing"
	"time"
)

// baseConfig returns a Config that passes Validate on its own, so each
// test case only has to set the field it's exercising.
func baseConfig() *Config {
	return &Config{
		ListenAddr:    ":8130",
		AdvertiseAddr: "hairpin.hairpin.svc:8130",
		Launcher:      "none",
	}
}

func TestValidateSandboxToken(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:   "disabled by default",
			mutate: func(*Config) {},
		},
		{
			name: "fully configured",
			mutate: func(c *Config) {
				c.SandboxToken = SandboxTokenConfig{
					KeyPath: "/etc/hairpin/sandbox-token.pem", Issuer: "https://hairpin.internal",
					Audience: "https://haybale.internal", TTL: 15 * time.Minute,
				}
			},
		},
		{
			name: "key without issuer",
			mutate: func(c *Config) {
				c.SandboxToken = SandboxTokenConfig{KeyPath: "key.pem", Audience: "aud", TTL: time.Minute}
			},
			wantErr: "sandbox-token-issuer is required",
		},
		{
			name: "key without audience",
			mutate: func(c *Config) {
				c.SandboxToken = SandboxTokenConfig{KeyPath: "key.pem", Issuer: "iss", TTL: time.Minute}
			},
			wantErr: "sandbox-token-audience is required",
		},
		{
			name: "key without positive ttl",
			mutate: func(c *Config) {
				c.SandboxToken = SandboxTokenConfig{KeyPath: "key.pem", Issuer: "iss", Audience: "aud"}
			},
			wantErr: "sandbox-token-ttl must be positive",
		},
		{
			name: "issuer and audience without a key is fine (issuance stays disabled)",
			mutate: func(c *Config) {
				c.SandboxToken = SandboxTokenConfig{Issuer: "iss", Audience: "aud"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate succeeded, want an error")
			}
			if got := err.Error(); !strings.Contains(got, tc.wantErr) {
				t.Errorf("error %q does not mention %q", got, tc.wantErr)
			}
		})
	}
}
