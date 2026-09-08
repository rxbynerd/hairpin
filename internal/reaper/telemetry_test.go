package reaper

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
	"github.com/rxbynerd/hairpin/internal/telemetry"
	"github.com/rxbynerd/hairpin/internal/telemetry/telemetrytest"
)

// completions counts the reaper-settled series of hairpin.job.completions.
func completions(t *testing.T, c *telemetrytest.Collector) int64 {
	t.Helper()
	return c.Sum(t, "hairpin.job.completions",
		attribute.String("hairpin.job.status", string(job.StatusFailed)),
		attribute.String("hairpin.job.stop_reason", telemetry.StopHarnessNeverConnected))
}

func TestSettledJobIsCountedAsACompletion(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	rec, collector := telemetrytest.New(t)

	slack := 10 * time.Minute
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deadline := awaitingHarnessDeadline(runConfigJSON(t, 600), slack)

	stuck := newAwaitingHarnessJob("hp-stuck", now.Add(-deadline-time.Minute), runConfigJSON(t, 600))
	if err := st.CreateJob(ctx, stuck); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := New(st, 0, slack, slog.New(slog.DiscardHandler), WithTelemetry(rec))
	r.now = func() time.Time { return now }

	r.reapAwaitingHarness(ctx, []*job.Job{stuck})

	if got := completions(t, collector); got != 1 {
		t.Errorf("reaper-settled completions = %d, want 1", got)
	}
	// The job never left awaiting_harness, so there is no
	// assignment-to-terminal latency to report.
	if got := collector.Count(t, "hairpin.job.run.duration"); got != 0 {
		t.Errorf("run duration recordings = %d, want 0", got)
	}
}

// A settle that loses the race writes no status, so it must not be
// counted either: the harness that won will report the real outcome.
func TestLostRaceRecordsNoCompletion(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	rec, collector := telemetrytest.New(t)

	j := &job.Job{
		ID:            "hp-race",
		Status:        job.StatusAwaitingHarness,
		CreatedAt:     time.Now().Add(-2 * time.Hour),
		RunConfigJSON: runConfigJSON(t, 600),
	}
	if err := st.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.UpdateJob(ctx, j.ID, func(j *job.Job) error {
		j.Status = job.StatusRunning
		j.StartedAt = time.Now()
		return nil
	}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	r := New(st, 0, 10*time.Minute, slog.New(slog.DiscardHandler), WithTelemetry(rec))
	if moved, err := r.failStuckHarness(ctx, j.ID); err != nil || moved {
		t.Fatalf("failStuckHarness: moved=%v err=%v, want moved=false", moved, err)
	}

	if got := completions(t, collector); got != 0 {
		t.Errorf("reaper-settled completions = %d, want 0", got)
	}
}

// A Reaper built without WithTelemetry keeps working: telemetry.Recorder
// is a no-op when nil.
func TestSettlingWithoutTelemetry(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	slack := 10 * time.Minute
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deadline := awaitingHarnessDeadline(runConfigJSON(t, 600), slack)

	stuck := newAwaitingHarnessJob("hp-stuck", now.Add(-deadline-time.Minute), runConfigJSON(t, 600))
	if err := st.CreateJob(ctx, stuck); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := New(st, 0, slack, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }

	r.reapAwaitingHarness(ctx, []*job.Job{stuck})

	got, err := st.GetJob(ctx, stuck.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
}
