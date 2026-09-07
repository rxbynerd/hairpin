package service

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
	"github.com/rxbynerd/hairpin/internal/telemetry"
	"github.com/rxbynerd/hairpin/internal/telemetry/telemetrytest"
)

func newInstrumentedFixture(t *testing.T) (*fixture, *telemetrytest.Collector) {
	t.Helper()
	rec, collector := telemetrytest.New(t)
	return newFixture(t, WithTelemetry(rec)), collector
}

func TestSubmitRecordsSubmissionAndLaunch(t *testing.T) {
	f, collector := newInstrumentedFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "audit the config"})

	if got := collector.Sum(t, "hairpin.job.submissions",
		attribute.String("hairpin.submission.outcome", telemetry.SubmissionAccepted),
		attribute.String("hairpin.profile", "default")); got != 1 {
		t.Errorf("accepted submissions = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.job.launches",
		attribute.String("hairpin.launch.outcome", telemetry.LaunchSucceeded)); got != 1 {
		t.Errorf("successful launches = %d, want 1", got)
	}

	submit := collector.Span(t, "hairpin.submit")
	if id, ok := attrValue(submit.Attributes, "hairpin.job.id"); !ok || id != j.ID {
		t.Errorf("submit span job id = %q, want %s", id, j.ID)
	}
	if profile, _ := attrValue(submit.Attributes, "hairpin.profile"); profile != "default" {
		t.Errorf("submit span profile = %q, want default", profile)
	}

	// The launch runs after the submitting request has returned, so it
	// traces separately and links back.
	launch := collector.Span(t, "hairpin.launch")
	if len(launch.Links) != 1 {
		t.Fatalf("launch span has %d links, want a link to the submission", len(launch.Links))
	}
	if got := launch.Links[0].SpanContext.TraceID(); got != submit.SpanContext.TraceID() {
		t.Errorf("launch links trace %s, want the submission's %s", got, submit.SpanContext.TraceID())
	}
}

func TestSubmitRejectionRecordsNoProfile(t *testing.T) {
	f, collector := newInstrumentedFixture(t)

	if _, err := f.svc.Submit(context.Background(), SubmitParams{Prompt: "x", Profile: "no-such-profile"}); err == nil {
		t.Fatal("Submit accepted an unknown profile")
	}

	if got := collector.Sum(t, "hairpin.job.submissions",
		attribute.String("hairpin.submission.outcome", telemetry.SubmissionRejected)); got != 1 {
		t.Errorf("rejected submissions = %d, want 1", got)
	}
	for _, set := range collector.Attrs(t, "hairpin.job.submissions") {
		if _, ok := set.Value("hairpin.profile"); ok {
			t.Error("a caller-supplied profile name reached a metric attribute")
		}
	}
}

func TestLaunchFailureRecordsFailureAndCompletion(t *testing.T) {
	f, collector := newInstrumentedFixture(t)
	f.launcher.err = errors.New("no capacity in namespace")

	f.submitted(t, SubmitParams{Prompt: "x"})

	if got := collector.Sum(t, "hairpin.job.launches",
		attribute.String("hairpin.launch.outcome", telemetry.LaunchFailed)); got != 1 {
		t.Errorf("failed launches = %d, want 1", got)
	}
	if got := collector.Sum(t, "hairpin.job.completions",
		attribute.String("hairpin.job.status", string(job.StatusFailed))); got != 1 {
		t.Errorf("failed completions = %d, want 1", got)
	}
	// A job that never reached a harness has no run duration.
	if got := collector.Count(t, "hairpin.job.run.duration"); got != 0 {
		t.Errorf("run duration recordings = %d, want 0", got)
	}
}

func TestCancelBeforeHarnessRecordsCompletion(t *testing.T) {
	f, collector := newInstrumentedFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})

	if _, err := f.svc.Cancel(context.Background(), j.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if got := collector.Sum(t, "hairpin.job.completions",
		attribute.String("hairpin.job.status", string(job.StatusCancelled)),
		attribute.String("hairpin.job.stop_reason", "cancelled")); got != 1 {
		t.Errorf("cancelled completions = %d, want 1", got)
	}
}

func TestAnswerPermissionRecordsDecision(t *testing.T) {
	f, collector := newInstrumentedFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	sess := &fakeSession{}
	if err := f.registry.Register(j.ID, sess); err != nil {
		t.Fatalf("Register: %v", err)
	}
	putPending(t, f.store, j.ID, "req-1")

	if _, err := f.svc.AnswerPermission(context.Background(), j.ID, "req-1", false, "not that"); err != nil {
		t.Fatalf("AnswerPermission: %v", err)
	}

	if got := collector.Sum(t, "hairpin.permission.decisions",
		attribute.String("hairpin.permission.decision", "deny"),
		attribute.Bool("hairpin.permission.delivered", true)); got != 1 {
		t.Errorf("delivered denials = %d, want 1", got)
	}
}

func TestAnswerPermissionRecordsUndeliveredDecision(t *testing.T) {
	f, collector := newInstrumentedFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	putPending(t, f.store, j.ID, "req-1")

	_, err := f.svc.AnswerPermission(context.Background(), j.ID, "req-1", true, "")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("AnswerPermission err = %v, want ErrNotConnected", err)
	}

	if got := collector.Sum(t, "hairpin.permission.decisions",
		attribute.Bool("hairpin.permission.delivered", false)); got != 1 {
		t.Errorf("undelivered decisions = %d, want 1", got)
	}
	stored, err := f.store.GetPermission(context.Background(), j.ID, "req-1")
	if err != nil {
		t.Fatalf("GetPermission: %v", err)
	}
	if stored.State != store.PermissionPending {
		t.Errorf("state = %s, want the decision reverted to pending", stored.State)
	}
}

func attrValue(attrs []attribute.KeyValue, key string) (string, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsString(), true
		}
	}
	return "", false
}
