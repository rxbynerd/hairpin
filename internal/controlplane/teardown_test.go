package controlplane

import (
	"context"
	"log/slog"
	"testing"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

// ctxStore fails writes once their context is dead, the way a real
// Redis client does — memstore ignores contexts, which would let this
// regression pass silently.
type ctxStore struct{ store.Store }

func (c ctxStore) UpdateJob(ctx context.Context, id string, fn func(*job.Job) error) (*job.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.Store.UpdateJob(ctx, id, fn)
}

func (c ctxStore) AppendEvent(ctx context.Context, jobID string, ev store.Event) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.Store.AppendEvent(ctx, jobID, ev)
}

// A harness process exits the instant it has sent done, so the stream
// context is typically cancelled while the terminal write is in flight.
// The pump's writes must land anyway.
func TestDoneSurvivesStreamContextCancellation(t *testing.T) {
	mem := store.NewMemStore(0)
	t.Cleanup(func() { _ = mem.Close() })
	st := ctxStore{mem}
	h := New(st, registry.New(), WithLogger(slog.New(slog.DiscardHandler)))
	seedJob(t, mem, "hp-1", job.StatusAwaitingHarness, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newFakeStream(
		ready("hp-1"),
		delta("partial output"),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	// Cancel before the done event is handed to the pump: every write
	// it triggers happens under an already-dead stream context.
	s.onReceive = func(n int) {
		if n == 2 {
			cancel()
		}
	}

	if err := h.runTask(ctx, s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	j := getJob(t, st, "hp-1")
	if j.Status != job.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded (terminal write lost to stream teardown)", j.Status)
	}
	if j.StopReason != "success" || j.FinalText != "partial output" {
		t.Errorf("stop_reason = %q, final_text = %q", j.StopReason, j.FinalText)
	}

	types := eventTypes(events(t, st, "hp-1"))
	if len(types) == 0 || types[len(types)-1] != EventStatusChange {
		t.Errorf("timeline missing terminal status_change: %v", types)
	}
}
