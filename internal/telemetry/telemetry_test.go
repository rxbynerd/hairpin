package telemetry_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/telemetry"
	"github.com/rxbynerd/hairpin/internal/telemetry/telemetrytest"
)

func TestOptionsValidate(t *testing.T) {
	tests := []struct {
		name    string
		opts    telemetry.Options
		wantErr bool
	}{
		{name: "zero value exports nothing", opts: telemetry.Options{}},
		{name: "otlp over grpc", opts: telemetry.Options{Exporter: "otlp", Protocol: "grpc", SampleRatio: 1}},
		{name: "otlp over http", opts: telemetry.Options{Exporter: "otlp", Protocol: "http/protobuf"}},
		{name: "unknown exporter", opts: telemetry.Options{Exporter: "jaeger"}, wantErr: true},
		{name: "unknown protocol", opts: telemetry.Options{Exporter: "otlp", Protocol: "thrift"}, wantErr: true},
		{name: "ratio above one", opts: telemetry.Options{Exporter: "otlp", SampleRatio: 1.5}, wantErr: true},
		{name: "negative ratio", opts: telemetry.Options{Exporter: "otlp", SampleRatio: -1}, wantErr: true},
		{name: "negative interval", opts: telemetry.Options{Exporter: "otlp", MetricInterval: -time.Second}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSetupWithoutExporterInstallsNothing(t *testing.T) {
	rec, shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if rec != nil {
		t.Fatalf("Setup returned a recorder with export disabled")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestSetupRejectsInvalidOptions(t *testing.T) {
	_, shutdown, err := telemetry.Setup(context.Background(), telemetry.Options{Exporter: "zipkin"}, nil)
	if err == nil {
		t.Fatal("Setup accepted an unknown exporter")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown after a failed setup: %v", err)
	}
}

// A nil Recorder is the shape every uninstrumented deployment runs in,
// so every method must accept it.
func TestNilRecorderRecordsNothing(t *testing.T) {
	var rec *telemetry.Recorder
	ctx, span := rec.Start(context.Background(), "hairpin.submit")
	defer span.End()
	if span.IsRecording() {
		t.Error("nil recorder started a recording span")
	}
	if got := telemetry.TraceParent(ctx); got != "" {
		t.Errorf("TraceParent = %q, want empty without a recording span", got)
	}
	telemetry.SetJobID(span, "hp-01")
	telemetry.SetRequestID(span, "req-1")
	telemetry.Fail(span, context.Canceled)

	rec.JobSubmitted(ctx, "default", telemetry.SubmissionAccepted)
	rec.JobLaunched(ctx, telemetry.LaunchSucceeded, time.Second)
	rec.JobCompleted(ctx, job.StatusSucceeded, "success", time.Second)
	rec.HarnessSession(ctx, telemetry.SessionAssigned)
	rec.SessionOpened(ctx)
	rec.SessionClosed(ctx)
	rec.HarnessEvent(ctx, "text_delta")
	rec.PermissionRequested(ctx)
	rec.PermissionAnswered(ctx, true, true)
	rec.MemoryCall(ctx, "search_memory", telemetry.MemoryOK, time.Second)
}

func TestRecorderMeasurements(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	ctx := context.Background()

	rec.JobSubmitted(ctx, "default", telemetry.SubmissionAccepted)
	rec.JobSubmitted(ctx, "", telemetry.SubmissionRejected)
	rec.JobLaunched(ctx, telemetry.LaunchSucceeded, 250*time.Millisecond)
	rec.JobLaunched(ctx, telemetry.LaunchSkipped, 0)
	rec.JobCompleted(ctx, job.StatusSucceeded, "success", 3*time.Second)
	rec.JobCompleted(ctx, job.StatusFailed, "", 0)
	rec.SessionOpened(ctx)
	rec.SessionOpened(ctx)
	rec.SessionClosed(ctx)
	rec.PermissionRequested(ctx)
	rec.PermissionAnswered(ctx, false, true)
	rec.MemoryCall(ctx, "search_memory", telemetry.MemoryOK, 20*time.Millisecond)
	rec.MemoryCall(ctx, "search_memory", telemetry.MemoryRefused, 0)

	if got := collector.Sum(t, "hairpin.job.submissions",
		attribute.String("hairpin.submission.outcome", telemetry.SubmissionAccepted),
		attribute.String("hairpin.profile", "default")); got != 1 {
		t.Errorf("accepted submissions on the default profile = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.job.launches",
		attribute.String("hairpin.launch.outcome", telemetry.LaunchSucceeded)); got != 1 {
		t.Errorf("successful launches = %d, want 1", got)
	}
	// A launch that never ran has no duration to report.
	if got := collector.Count(t, "hairpin.job.launch.duration"); got != 1 {
		t.Errorf("launch duration recordings = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.job.completions",
		attribute.String("hairpin.job.status", string(job.StatusSucceeded)),
		attribute.String("hairpin.job.stop_reason", "success")); got != 1 {
		t.Errorf("successful completions = %d, want 1", got)
	}
	if got := collector.Count(t, "hairpin.job.run.duration"); got != 1 {
		t.Errorf("run duration recordings = %d, want 1 (a job that never ran records none)", got)
	}
	if got := collector.Sum(t, "hairpin.harness.sessions.active"); got != 1 {
		t.Errorf("active sessions = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.permission.requests"); got != 1 {
		t.Errorf("permission requests = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.permission.decisions",
		attribute.String("hairpin.permission.decision", "deny"),
		attribute.Bool("hairpin.permission.delivered", true)); got != 1 {
		t.Errorf("delivered denials = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.memory.calls",
		attribute.String("hairpin.memory.tool", "search_memory"),
		attribute.String("hairpin.memory.outcome", telemetry.MemoryRefused)); got != 1 {
		t.Errorf("refused memory calls = %d, want 1", got)
	}
	if got := collector.Count(t, "hairpin.memory.call.duration"); got != 1 {
		t.Errorf("memory duration recordings = %d, want 1 (a refusal never called Billet)", got)
	}
}

// A rejected submission names a profile the caller invented, so it must
// not open a metric series of its own.
func TestRejectedSubmissionCarriesNoProfile(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	rec.JobSubmitted(context.Background(), "", telemetry.SubmissionRejected)

	sets := collector.Attrs(t, "hairpin.job.submissions")
	if len(sets) != 1 {
		t.Fatalf("recorded %d series, want 1", len(sets))
	}
	if _, ok := sets[0].Value("hairpin.profile"); ok {
		t.Errorf("rejected submission carries a profile attribute: %v", sets[0].Encoded(attribute.DefaultEncoder()))
	}
}

// Stop reasons, event types, and tool names all arrive from the
// harness: anything outside the known vocabulary must collapse to one
// series rather than opening one per value.
func TestHarnessSuppliedLabelsAreBounded(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	ctx := context.Background()

	rec.JobCompleted(ctx, job.StatusFailed, "wandered_off", 0)
	rec.JobCompleted(ctx, job.StatusFailed, "vanished", 0)
	rec.HarnessEvent(ctx, "text_delta")
	rec.HarnessEvent(ctx, "unheard_of")
	rec.HarnessEvent(ctx, "also_unheard_of")
	rec.MemoryCall(ctx, "delete_everything", telemetry.MemoryRefused, 0)

	if got := collector.Sum(t, "hairpin.job.completions",
		attribute.String("hairpin.job.stop_reason", "other")); got != 2 {
		t.Errorf("completions bucketed as other = %d, want 2", got)
	}
	if got := collector.Sum(t, "hairpin.harness.events",
		attribute.String("hairpin.harness.event.type", "other")); got != 2 {
		t.Errorf("events bucketed as other = %d, want 2", got)
	}
	if got := collector.Sum(t, "hairpin.harness.events",
		attribute.String("hairpin.harness.event.type", "text_delta")); got != 1 {
		t.Errorf("text_delta events = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.memory.calls",
		attribute.String("hairpin.memory.tool", "other")); got != 1 {
		t.Errorf("memory calls bucketed as other = %d, want 1", got)
	}
}

func TestTraceParentLinksLaterSpans(t *testing.T) {
	rec, collector := telemetrytest.New(t)

	ctx, submit := rec.Start(context.Background(), "hairpin.submit")
	traceParent := telemetry.TraceParent(ctx)
	if traceParent == "" {
		t.Fatal("TraceParent is empty for a recording span")
	}
	submitTrace := submit.SpanContext().TraceID()
	submit.End()

	// The harness stream is a separate trace: the link is what ties it
	// back to the submission.
	_, session := rec.Start(context.Background(), "hairpin.harness_session", telemetry.LinkToTraceParent(traceParent))
	session.End()

	stub := collector.Span(t, "hairpin.harness_session")
	if stub.SpanContext.TraceID() == submitTrace {
		t.Fatal("the session span joined the submission trace instead of linking to it")
	}
	if len(stub.Links) != 1 {
		t.Fatalf("session span has %d links, want 1", len(stub.Links))
	}
	if got := stub.Links[0].SpanContext.TraceID(); got != submitTrace {
		t.Errorf("link trace = %s, want %s", got, submitTrace)
	}
}

func TestLinkToTraceParentIgnoresUnusableValues(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	for _, traceParent := range []string{"", "not-a-traceparent", "00-0-0-00"} {
		_, span := rec.Start(context.Background(), "hairpin.harness_session", telemetry.LinkToTraceParent(traceParent))
		span.End()
	}
	for _, stub := range collector.Spans(t) {
		if len(stub.Links) != 0 {
			t.Errorf("traceparent %q produced %d links, want none", stub.Name, len(stub.Links))
		}
	}
}
