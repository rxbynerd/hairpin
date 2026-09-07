package controlplane

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/memory"
	"github.com/rxbynerd/hairpin/internal/telemetry"
	"github.com/rxbynerd/hairpin/internal/telemetry/telemetrytest"
)

func TestRunTaskRecordsSessionEventsAndOutcome(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	h, st, _ := testHandler(t, WithTelemetry(rec))
	clock := newTestClock()
	h.now = clock.now
	seedJob(t, st, "hp-1", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-1"),
		delta("hel"),
		delta("lo"),
		&harnessv1.HarnessEvent{Type: evHeartbeat},
		&harnessv1.HarnessEvent{Type: "cartwheel"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	s.onReceive = func(n int) {
		if n > 0 {
			clock.advance(time.Second)
		}
	}

	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if got := collector.Sum(t, "hairpin.harness.sessions",
		attribute.String("hairpin.session.disposition", telemetry.SessionAssigned)); got != 1 {
		t.Errorf("assigned sessions = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.harness.sessions.active"); got != 0 {
		t.Errorf("active sessions after the stream ended = %d, want 0", got)
	}
	if got := collector.Sum(t, "hairpin.harness.events",
		attribute.String("hairpin.harness.event.type", "text_delta")); got != 2 {
		t.Errorf("text_delta events = %d, want 2", got)
	}
	if got := collector.Sum(t, "hairpin.harness.events",
		attribute.String("hairpin.harness.event.type", "other")); got != 1 {
		t.Errorf("unrecognised events = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.job.completions",
		attribute.String("hairpin.job.status", string(job.StatusSucceeded)),
		attribute.String("hairpin.job.stop_reason", "success")); got != 1 {
		t.Errorf("successful completions = %d, want 1", got)
	}
	if got := collector.Count(t, "hairpin.job.run.duration"); got != 1 {
		t.Errorf("run duration recordings = %d, want 1", got)
	}
}

func TestHarnessSessionSpanLinksToSubmission(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	h, st, _ := testHandler(t, WithTelemetry(rec))

	// Stand in for the submission that created the job.
	submitCtx, submit := rec.Start(context.Background(), "hairpin.submit")
	traceParent := telemetry.TraceParent(submitCtx)
	submit.End()

	seedJob(t, st, "hp-1", job.StatusAwaitingHarness, func(j *job.Job) {
		j.TraceParent = traceParent
	})

	s := newFakeStream(ready("hp-1"), &harnessv1.HarnessEvent{Type: evDone, StopReason: "success"})
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	session := collector.Span(t, "hairpin.harness_session")
	var jobID string
	for _, kv := range session.Attributes {
		if kv.Key == "hairpin.job.id" {
			jobID = kv.Value.AsString()
		}
	}
	if jobID != "hp-1" {
		t.Errorf("session span job id = %q, want hp-1", jobID)
	}
	if len(session.Links) != 1 {
		t.Fatalf("session span has %d links, want a link to the submission", len(session.Links))
	}
	if got := session.Links[0].SpanContext.TraceID(); got != submit.SpanContext().TraceID() {
		t.Errorf("link trace = %s, want the submission's %s", got, submit.SpanContext().TraceID())
	}
}

func TestRejectedSessionsAreRecordedByReason(t *testing.T) {
	tests := []struct {
		name        string
		seed        func(t *testing.T, h *Handler)
		readyID     string
		disposition string
	}{
		{
			name:        "no session id",
			readyID:     "",
			disposition: telemetry.SessionNoID,
		},
		{
			name:        "unknown job",
			readyID:     "hp-missing",
			disposition: telemetry.SessionUnknownJob,
		},
		{
			name: "wrong token",
			seed: func(t *testing.T, h *Handler) {
				seedJob(t, h.store, "hp-1", job.StatusAwaitingHarness, func(j *job.Job) {
					j.HarnessToken = "the-real-token"
				})
			},
			readyID:     "hp-1.a-guess",
			disposition: telemetry.SessionBadToken,
		},
		{
			name: "already finished",
			seed: func(t *testing.T, h *Handler) {
				seedJob(t, h.store, "hp-1", job.StatusSucceeded, nil)
			},
			readyID:     "hp-1",
			disposition: telemetry.SessionClosedJob,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, collector := telemetrytest.New(t)
			h, _, _ := testHandler(t, WithTelemetry(rec))
			if tc.seed != nil {
				tc.seed(t, h)
			}

			if err := h.runTask(context.Background(), newFakeStream(ready(tc.readyID))); err != nil {
				t.Fatalf("runTask: %v", err)
			}

			if got := collector.Sum(t, "hairpin.harness.sessions",
				attribute.String("hairpin.session.disposition", tc.disposition)); got != 1 {
				t.Errorf("sessions recorded as %s = %d, want 1", tc.disposition, got)
			}
			if got := collector.Sum(t, "hairpin.harness.sessions.active"); got != 0 {
				t.Errorf("active sessions = %d, want 0 for a rejected stream", got)
			}
		})
	}
}

func TestNonReadyFirstEventIsRecorded(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	h, _, _ := testHandler(t, WithTelemetry(rec))

	if err := h.runTask(context.Background(), newFakeStream(&harnessv1.HarnessEvent{Type: evHeartbeat})); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if got := collector.Sum(t, "hairpin.harness.sessions",
		attribute.String("hairpin.session.disposition", telemetry.SessionNotReady)); got != 1 {
		t.Errorf("not-ready sessions = %d, want 1", got)
	}
}

// tickingMemory advances a test clock while a call is in flight, so the
// recorded call duration is deterministic.
type tickingMemory struct {
	memory.Client
	clock *testClock
	by    time.Duration
}

func (m tickingMemory) Search(ctx context.Context, query string, limit int32) ([]memory.Record, error) {
	m.clock.advance(m.by)
	return m.Client.Search(ctx, query, limit)
}

func TestMemoryCallsAreMeasured(t *testing.T) {
	rec, collector := telemetrytest.New(t)
	h, st, _ := testHandler(t, WithTelemetry(rec))
	clock := newTestClock()
	h.now = clock.now
	h.memory = tickingMemory{Client: &fakeMemory{}, clock: clock, by: 2 * time.Second}
	seedToolJob(t, st, "hp-mem", memory.ToolSearch)

	s := newFakeStream(
		ready("hp-mem"),
		toolRequest("t-1", memory.ToolSearch, `{"query":"proxy"}`),
		toolRequest("t-2", "ask_the_operator", `{}`),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	s.onReceive = func(n int) {
		if n == 3 {
			waitFor(t, "both tool answers", func() bool { return len(toolResponses(s)) == 2 })
		}
	}
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}
	h.WaitForMemoryCalls()

	if got := collector.Sum(t, "hairpin.memory.calls",
		attribute.String("hairpin.memory.tool", memory.ToolSearch),
		attribute.String("hairpin.memory.outcome", telemetry.MemoryOK)); got != 1 {
		t.Errorf("fulfilled searches = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.memory.calls",
		attribute.String("hairpin.memory.tool", "other"),
		attribute.String("hairpin.memory.outcome", telemetry.MemoryRefused)); got != 1 {
		t.Errorf("refused calls for an unknown tool = %d, want 1", got)
	}
	if got := collector.Count(t, "hairpin.memory.call.duration"); got != 1 {
		t.Errorf("duration recordings = %d, want 1 (a refusal never reached Billet)", got)
	}

	call := collector.Span(t, "hairpin.memory.call")
	attrs := map[string]string{}
	for _, kv := range call.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["hairpin.memory.tool"] != memory.ToolSearch {
		t.Errorf("memory span tool = %q, want %s", attrs["hairpin.memory.tool"], memory.ToolSearch)
	}
	if attrs["hairpin.request.id"] != "t-1" {
		t.Errorf("memory span request id = %q, want t-1", attrs["hairpin.request.id"])
	}
	if attrs["hairpin.job.id"] != "hp-mem" {
		t.Errorf("memory span job id = %q, want hp-mem", attrs["hairpin.job.id"])
	}
}
