package integration

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"

	hairpinv1 "github.com/rxbynerd/hairpin/gen/hairpin/v1"
	"github.com/rxbynerd/hairpin/gen/hairpin/v1/hairpinv1connect"
	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/gen/harness/v1/harnessv1connect"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/telemetry"
	"github.com/rxbynerd/hairpin/internal/telemetry/telemetrytest"
)

// TestTelemetryAcrossTheLoop runs one submit-to-done loop over the wire
// and asserts what an operator would see afterwards: RPC spans on both
// surfaces, hairpin's own spans beneath them, and the harness stream
// linked back to the submission that created its job.
func TestTelemetryAcrossTheLoop(t *testing.T) {
	baseURL, client, collector := startInstrumentedServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs := hairpinv1connect.NewJobServiceClient(client, baseURL)
	sub, err := jobs.SubmitJob(ctx, connect.NewRequest(&hairpinv1.SubmitJobRequest{RunConfigJson: testRunConfig}))
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	jobID := sub.Msg.Job.Id
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS)

	harness := harnessv1connect.NewHarnessServiceClient(client, baseURL, connect.WithGRPC())
	stream := harness.RunTask(ctx)
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready", Id: sub.Msg.HarnessSession, HarnessVersion: "test"}); err != nil {
		t.Fatalf("send ready: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("receive assignment: %v", err)
	}
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "text_delta", Text: "working"}); err != nil {
		t.Fatalf("send delta: %v", err)
	}
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "done", StopReason: "success"}); err != nil {
		t.Fatalf("send done: %v", err)
	}
	_ = stream.CloseRequest()
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_SUCCEEDED)

	// The session span ends as the stream tears down, just after the
	// job record reaches its terminal status.
	waitForSpan(t, collector, "hairpin.harness_session")

	submit := collector.Span(t, "hairpin.submit")
	session := collector.Span(t, "hairpin.harness_session")
	if !hasSpan(t, collector, "hairpin.v1.JobService/SubmitJob") {
		t.Error("no RPC span for SubmitJob; the connect interceptor is not wired in")
	}
	if !hasSpan(t, collector, "stirrup.harness.v1.HarnessService/RunTask") {
		t.Error("no RPC span for RunTask; the control plane is not instrumented")
	}
	if session.SpanContext.TraceID() == submit.SpanContext.TraceID() {
		t.Error("the harness stream joined the submitter's trace; it is a separate caller")
	}
	if len(session.Links) != 1 || session.Links[0].SpanContext.TraceID() != submit.SpanContext.TraceID() {
		t.Errorf("session span links = %v, want one link to the submission trace", session.Links)
	}

	if got := collector.Sum(t, "hairpin.job.completions",
		attribute.String("hairpin.job.status", string(job.StatusSucceeded)),
		attribute.String("hairpin.job.stop_reason", "success")); got != 1 {
		t.Errorf("successful completions = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.harness.sessions",
		attribute.String("hairpin.session.disposition", telemetry.SessionAssigned)); got != 1 {
		t.Errorf("assigned sessions = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.harness.sessions.active"); got != 0 {
		t.Errorf("active sessions after the run = %d, want 0", got)
	}
}

func hasSpan(t *testing.T, collector *telemetrytest.Collector, name string) bool {
	t.Helper()
	for _, s := range collector.Spans(t) {
		if s.Name == name {
			return true
		}
	}
	return false
}

func waitForSpan(t *testing.T, collector *telemetrytest.Collector, name string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if hasSpan(t, collector, name) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("span %q never finished", name)
}
