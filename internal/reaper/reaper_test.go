package reaper

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

func runConfigJSON(t *testing.T, timeoutSeconds int32) string {
	t.Helper()
	cfg := &harnessv1.RunConfig{
		Mode:     "execution",
		Provider: &harnessv1.ProviderConfig{Type: "anthropic"},
		MaxTurns: 20,
		Executor: &harnessv1.ExecutorConfig{Type: "local"},
		Timeout:  proto.Int32(timeoutSeconds),
	}
	raw, err := protojson.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal run config: %v", err)
	}
	return string(raw)
}

func newAwaitingHarnessJob(id string, createdAt time.Time, cfgJSON string) *job.Job {
	return &job.Job{
		ID:            id,
		Status:        job.StatusAwaitingHarness,
		CreatedAt:     createdAt,
		RunConfigJSON: cfgJSON,
	}
}

func TestAwaitingHarnessDeadlineParsesTimeout(t *testing.T) {
	slack := 10 * time.Minute
	cfgJSON := runConfigJSON(t, 600)

	got := awaitingHarnessDeadline(cfgJSON, slack)
	want := 600*time.Second + slack + awaitingHarnessMargin
	if got != want {
		t.Fatalf("deadline = %v, want %v", got, want)
	}
}

func TestAwaitingHarnessDeadlineFallsBackOnUnparseable(t *testing.T) {
	slack := 10 * time.Minute

	got := awaitingHarnessDeadline("not json", slack)
	want := awaitingHarnessFallback + slack
	if got != want {
		t.Fatalf("deadline = %v, want %v", got, want)
	}
}

func TestAwaitingHarnessDeadlineFallsBackOnMissingTimeout(t *testing.T) {
	slack := 10 * time.Minute
	cfg := &harnessv1.RunConfig{Mode: "execution"}
	raw, err := protojson.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := awaitingHarnessDeadline(string(raw), slack)
	want := awaitingHarnessFallback + slack
	if got != want {
		t.Fatalf("deadline = %v, want %v", got, want)
	}
}

func TestReapAwaitingHarnessFailsStuckJob(t *testing.T) {
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
	if got.Error != stuckHarnessError {
		t.Fatalf("error = %q, want %q", got.Error, stuckHarnessError)
	}
	if got.FinishedAt.IsZero() {
		t.Fatal("FinishedAt not set")
	}

	events, err := st.ReadEvents(ctx, stuck.ID, "", 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 1 || events[0].Type != eventStatusChange {
		t.Fatalf("expected one status_change event, got %+v", events)
	}
}

func TestReapAwaitingHarnessLeavesJobWithinDeadline(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	slack := 10 * time.Minute
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	fresh := newAwaitingHarnessJob("hp-fresh", now.Add(-time.Minute), runConfigJSON(t, 600))
	if err := st.CreateJob(ctx, fresh); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := New(st, 0, slack, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }

	r.reapAwaitingHarness(ctx, []*job.Job{fresh})

	got, err := st.GetJob(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusAwaitingHarness {
		t.Fatalf("status = %s, want awaiting_harness untouched", got.Status)
	}
}

func TestReapAwaitingHarnessIgnoresRunningJob(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	slack := 10 * time.Minute
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	running := &job.Job{
		ID:            "hp-running",
		Status:        job.StatusRunning,
		CreatedAt:     now.Add(-24 * time.Hour),
		RunConfigJSON: runConfigJSON(t, 600),
	}
	if err := st.CreateJob(ctx, running); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := New(st, 0, slack, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }

	r.reapAwaitingHarness(ctx, []*job.Job{running})

	got, err := st.GetJob(ctx, running.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusRunning {
		t.Fatalf("status = %s, want running untouched", got.Status)
	}
}

// TestFailStuckHarnessLosesRaceToConcurrentProgress models a harness
// that dials in and moves the job to running after the reaper's
// ListJobs snapshot but before its UpdateJob call lands: the atomic
// status re-check must leave the newer state alone.
func TestFailStuckHarnessLosesRaceToConcurrentProgress(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	j := &job.Job{
		ID:            "hp-race",
		Status:        job.StatusAwaitingHarness,
		CreatedAt:     time.Now().Add(-2 * time.Hour),
		RunConfigJSON: runConfigJSON(t, 600),
	}
	if err := st.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Simulate the concurrent progression that happened between the
	// reaper's list snapshot and its settle attempt.
	if _, err := st.UpdateJob(ctx, j.ID, func(j *job.Job) error {
		j.Status = job.StatusRunning
		j.StartedAt = time.Now()
		return nil
	}); err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}

	r := New(st, 0, 10*time.Minute, slog.New(slog.DiscardHandler))
	moved, err := r.failStuckHarness(ctx, j.ID)
	if err != nil {
		t.Fatalf("failStuckHarness: %v", err)
	}
	if moved {
		t.Fatal("expected failStuckHarness to lose the race, but it reported moved=true")
	}

	got, err := st.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusRunning {
		t.Fatalf("status = %s, want running preserved", got.Status)
	}

	events, err := st.ReadEvents(ctx, j.ID, "", 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no status_change event on a lost race, got %+v", events)
	}
}

func TestReapRetentionDeletesOldTerminalJobs(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	retention := 24 * time.Hour

	old := &job.Job{ID: "hp-old", Status: job.StatusSucceeded, CreatedAt: now.Add(-48 * time.Hour), FinishedAt: now.Add(-25 * time.Hour)}
	recent := &job.Job{ID: "hp-recent", Status: job.StatusFailed, CreatedAt: now.Add(-2 * time.Hour), FinishedAt: now.Add(-time.Hour)}
	running := &job.Job{ID: "hp-running", Status: job.StatusRunning, CreatedAt: now.Add(-48 * time.Hour)}
	for _, j := range []*job.Job{old, recent, running} {
		if err := st.CreateJob(ctx, j); err != nil {
			t.Fatalf("CreateJob(%s): %v", j.ID, err)
		}
	}

	r := New(st, retention, 10*time.Minute, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }

	r.reapRetention(ctx, []*job.Job{old, recent, running})

	if _, err := st.GetJob(ctx, old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old job: got %v, want deleted", err)
	}
	if _, err := st.GetJob(ctx, recent.ID); err != nil {
		t.Fatalf("recent job should survive retention: %v", err)
	}
	if _, err := st.GetJob(ctx, running.ID); err != nil {
		t.Fatalf("non-terminal job must never be deleted: %v", err)
	}
}

func TestReapRetentionDisabledLeavesJobsAlone(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := &job.Job{ID: "hp-old", Status: job.StatusSucceeded, CreatedAt: now.Add(-48 * time.Hour), FinishedAt: now.Add(-48 * time.Hour)}
	if err := st.CreateJob(ctx, old); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := New(st, 0, 10*time.Minute, slog.New(slog.DiscardHandler))
	r.now = func() time.Time { return now }

	r.sweep(ctx)

	if _, err := st.GetJob(ctx, old.ID); err != nil {
		t.Fatalf("job should survive with retention disabled: %v", err)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })

	r := New(st, 0, 10*time.Minute, slog.New(slog.DiscardHandler), WithInterval(time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
