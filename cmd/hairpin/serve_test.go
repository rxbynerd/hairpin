package main

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/rxbynerd/hairpin/internal/telemetry"
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
		{name: "billet address with a named port", flag: "-billet-addr=billet.hairpin.svc:rpc", wantErr: "port must be a number"},
		{name: "billet address with an out-of-range port", flag: "-billet-addr=billet.hairpin.svc:70000", wantErr: "port must be a number"},
		{name: "harness telemetry endpoint without a port", flag: "-harness-telemetry-endpoint=steeplechase.hairpin.svc", wantErr: "harness telemetry endpoint must be host:port"},
		{name: "harness telemetry endpoint as a URL", flag: "-harness-telemetry-endpoint=http://steeplechase.hairpin.svc:4317", wantErr: "harness telemetry endpoint"},
		{name: "unknown telemetry exporter", flag: "-telemetry=jaeger", wantErr: "unknown telemetry exporter"},
		{name: "unknown telemetry protocol", flag: "-telemetry-protocol=thrift", wantErr: "unknown telemetry protocol"},
		{name: "sample ratio above one", flag: "-telemetry-sample-ratio=1.5", wantErr: "sample ratio must be between 0 and 1"},
		{name: "negative metric interval", flag: "-telemetry-metric-interval=-1s", wantErr: "metric interval must be non-negative"},
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

// The harness's exporter target and hairpin's own are separate flags:
// setting one must not imply the other.
func TestParseServeFlagsSeparatesHarnessAndServerTelemetryEndpoints(t *testing.T) {
	cfg, err := parseServeFlags([]string{
		"-launcher=none", "-advertise=hairpin.example:8130",
		"-harness-telemetry-endpoint=steeplechase.hairpin.svc:4317",
	})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfg.HarnessTelemetryEndpoint != "steeplechase.hairpin.svc:4317" {
		t.Errorf("harness telemetry endpoint = %q", cfg.HarnessTelemetryEndpoint)
	}
	if cfg.Telemetry.Endpoint != "" {
		t.Errorf("hairpin's own telemetry endpoint = %q, want unset", cfg.Telemetry.Endpoint)
	}
	if cfg.Telemetry.Enabled() {
		t.Errorf("telemetry exporter = %q, want no export of hairpin's own signals", cfg.Telemetry.Exporter)
	}
}

func TestParseServeFlagsTelemetryDefaultsToNoExport(t *testing.T) {
	cfg, err := parseServeFlags([]string{"-launcher=none", "-advertise=hairpin.example:8130"})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfg.Telemetry.Enabled() {
		t.Errorf("telemetry exporter = %q, want no export by default", cfg.Telemetry.Exporter)
	}
}

func TestParseServeFlagsTelemetryOptions(t *testing.T) {
	cfg, err := parseServeFlags([]string{
		"-launcher=none", "-advertise=hairpin.example:8130",
		"-telemetry=otlp",
		"-telemetry-protocol=http/protobuf",
		"-telemetry-endpoint=http://collector.observability.svc:4318",
		"-telemetry-sample-ratio=0.25",
		"-telemetry-service-name=hairpin-staging",
	})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if !cfg.Telemetry.Enabled() {
		t.Error("telemetry is disabled after asking for otlp")
	}
	if cfg.Telemetry.Protocol != telemetry.ProtocolHTTP {
		t.Errorf("protocol = %q, want %s", cfg.Telemetry.Protocol, telemetry.ProtocolHTTP)
	}
	if cfg.Telemetry.Endpoint != "http://collector.observability.svc:4318" {
		t.Errorf("endpoint = %q", cfg.Telemetry.Endpoint)
	}
	if cfg.Telemetry.SampleRatio != 0.25 {
		t.Errorf("sample ratio = %v, want 0.25", cfg.Telemetry.SampleRatio)
	}
	if cfg.Telemetry.ServiceName != "hairpin-staging" {
		t.Errorf("service name = %q", cfg.Telemetry.ServiceName)
	}
}

// The OTLP environment variables the SDK already honours should not
// need a flag repeated alongside them.
func TestParseServeFlagsTelemetryProtocolFromEnvironment(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", telemetry.ProtocolHTTP)
	cfg, err := parseServeFlags([]string{"-launcher=none", "-advertise=hairpin.example:8130", "-telemetry=otlp"})
	if err != nil {
		t.Fatalf("parseServeFlags: %v", err)
	}
	if cfg.Telemetry.Protocol != telemetry.ProtocolHTTP {
		t.Errorf("protocol = %q, want %s from the environment", cfg.Telemetry.Protocol, telemetry.ProtocolHTTP)
	}
}
