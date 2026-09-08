package service

import (
	"context"
	"log/slog"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

const testHarnessTelemetryEndpoint = "steeplechase.hairpin.svc:4317"

// newTraceEmitterService builds a Service whose default profile is
// tmpl, with endpoint offered as the harness telemetry endpoint.
func newTraceEmitterService(t *testing.T, tmpl *harnessv1.RunConfig, endpoint string) *Service {
	t.Helper()
	profiles, err := NewProfiles(map[string]*harnessv1.RunConfig{"default": tmpl}, "default")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	return New(st, registry.New(), &fakeLauncher{}, profiles, slog.New(slog.DiscardHandler),
		WithHarnessTelemetryEndpoint(endpoint))
}

// submittedTraceEmitter submits against svc and returns the trace
// emitter as it was persisted.
func submittedTraceEmitter(t *testing.T, svc *Service) *harnessv1.TraceEmitterConfig {
	t.Helper()
	j, err := svc.Submit(context.Background(), SubmitParams{Prompt: "do the thing"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	svc.WaitForLaunches()
	var cfg harnessv1.RunConfig
	if err := protojson.Unmarshal([]byte(j.RunConfigJSON), &cfg); err != nil {
		t.Fatalf("unmarshal persisted run config: %v", err)
	}
	return cfg.GetTraceEmitter()
}

func TestSubmitInjectsHarnessTelemetryEndpoint(t *testing.T) {
	em := submittedTraceEmitter(t, newTraceEmitterService(t, testTemplate(), testHarnessTelemetryEndpoint))

	if em.GetType() != "otel" {
		t.Errorf("trace_emitter.type = %q, want otel", em.GetType())
	}
	if em.GetEndpoint() != testHarnessTelemetryEndpoint {
		t.Errorf("trace_emitter.endpoint = %q, want %q", em.GetEndpoint(), testHarnessTelemetryEndpoint)
	}
	if em.GetProtocol() != "" {
		t.Errorf("trace_emitter.protocol = %q, want unset so the harness defaults to grpc", em.GetProtocol())
	}
}

func TestSubmitKeepsProfileTraceEmitter(t *testing.T) {
	tmpl := testTemplate()
	tmpl.TraceEmitter = &harnessv1.TraceEmitterConfig{Type: "jsonl", FilePath: "/tmp/run.jsonl"}

	em := submittedTraceEmitter(t, newTraceEmitterService(t, tmpl, testHarnessTelemetryEndpoint))

	if em.GetType() != "jsonl" {
		t.Errorf("trace_emitter.type = %q, want the profile's own jsonl", em.GetType())
	}
	if em.GetFilePath() != "/tmp/run.jsonl" {
		t.Errorf("trace_emitter.file_path = %q, want the profile's own", em.GetFilePath())
	}
}

func TestSubmitLeavesTraceEmitterUnsetWithoutAnEndpoint(t *testing.T) {
	if em := submittedTraceEmitter(t, newTraceEmitterService(t, testTemplate(), "")); em != nil {
		t.Errorf("trace_emitter = %v, want unset with no harness telemetry endpoint configured", em)
	}
}
