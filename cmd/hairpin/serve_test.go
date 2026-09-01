package main

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestParseServeFlagsRejectsInvalidNumericAndRuntimeValues(t *testing.T) {
	base := []string{"-launcher=none", "-advertise=hairpin.example:8130"}
	tests := []struct {
		name    string
		flag    string
		wantErr string
	}{
		{name: "negative redis database", flag: "-redis-db=-1", wantErr: "redis database number must be non-negative"},
		{name: "negative job TTL", flag: "-job-ttl=-1", wantErr: "job TTL must be between"},
		{name: "overflowing job TTL", flag: "-job-ttl=" + strconv.FormatInt(int64(math.MaxInt32)+1, 10), wantErr: "job TTL must be between"},
		{name: "negative deadline slack", flag: "-deadline-slack=-1s", wantErr: "deadline slack must be non-negative"},
		{name: "unknown sandbox runtime", flag: "-sandbox-runtime=runsc", wantErr: "unknown sandbox runtime"},
		{name: "billet address without a port", flag: "-billet-addr=billet.hairpin.svc", wantErr: "billet address must be host:port"},
		{name: "billet address without a host", flag: "-billet-addr=:8141", wantErr: "billet address must be host:port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseServeFlags(append(append([]string{}, base...), tt.flag))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseServeFlags error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseServeFlagsAcceptsBilletAddr(t *testing.T) {
	cfg, err := parseServeFlags([]string{"-launcher=none", "-advertise=hairpin.example:8130", "-billet-addr=billet.hairpin.svc:8141"})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfg.BilletAddr != "billet.hairpin.svc:8141" {
		t.Errorf("billet address = %q", cfg.BilletAddr)
	}
}
