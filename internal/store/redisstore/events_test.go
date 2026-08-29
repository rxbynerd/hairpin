package redisstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rxbynerd/hairpin/internal/store"
)

func TestAppendAndReadEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-events")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	var ids []string
	for i := 0; i < 5; i++ {
		id, err := s.AppendEvent(ctx, j.ID, store.Event{
			Type:        "text_delta",
			PayloadJSON: fmt.Sprintf(`{"n":%d}`, i),
		})
		if err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		ids = append(ids, id)
	}

	all, err := s.ReadEvents(ctx, j.ID, "", 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("got %d events, want 5", len(all))
	}
	for i, ev := range all {
		if ev.ID != ids[i] {
			t.Fatalf("event %d ID mismatch: got %s, want %s", i, ev.ID, ids[i])
		}
		if ev.PayloadJSON != fmt.Sprintf(`{"n":%d}`, i) {
			t.Fatalf("event %d payload mismatch: %s", i, ev.PayloadJSON)
		}
	}

	after, err := s.ReadEvents(ctx, j.ID, ids[1], 0)
	if err != nil {
		t.Fatalf("ReadEvents after cursor: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("got %d events after cursor, want 3", len(after))
	}
	if after[0].ID != ids[2] {
		t.Fatalf("first event after cursor: got %s, want %s", after[0].ID, ids[2])
	}
}

func TestAppendEventHonoursAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-events-at")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	id, err := s.AppendEvent(ctx, j.ID, store.Event{Type: "heartbeat", At: at})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	evs, err := s.ReadEvents(ctx, j.ID, "", 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(evs) != 1 || evs[0].ID != id {
		t.Fatalf("unexpected events: %+v", evs)
	}
	if !evs[0].At.Equal(at) {
		t.Fatalf("payload At mismatch: got %v, want %v", evs[0].At, at)
	}
}

func TestAppendEventJobNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	_, err := s.AppendEvent(ctx, "hp-missing", store.Event{Type: "x"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AppendEvent: got %v, want ErrNotFound", err)
	}
}

func TestEventStreamCapping(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{MaxEvents: 10})
	j := newTestJob("hp-cap")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	for i := 0; i < 50; i++ {
		if _, err := s.AppendEvent(ctx, j.ID, store.Event{Type: "text_delta"}); err != nil {
			t.Fatalf("AppendEvent[%d]: %v", i, err)
		}
	}

	evs, err := s.ReadEvents(ctx, j.ID, "", 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	// XADD MAXLEN ~ trims approximately; assert it capped well below the
	// uncapped length rather than pinning an exact count.
	if len(evs) >= 50 {
		t.Fatalf("stream was not capped: got %d events", len(evs))
	}
	if len(evs) == 0 {
		t.Fatalf("stream capping dropped every event")
	}
}

func TestWatchEventsHistoryThenLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-watch")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	histID, err := s.AppendEvent(ctx, j.ID, store.Event{Type: "history"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	ch, err := s.WatchEvents(ctx, j.ID, "")
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}

	first := recvEvent(t, ch)
	if first.ID != histID || first.Type != "history" {
		t.Fatalf("history event mismatch: %+v", first)
	}

	liveID, err := s.AppendEvent(ctx, j.ID, store.Event{Type: "live"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	second := recvEvent(t, ch)
	if second.ID != liveID || second.Type != "live" {
		t.Fatalf("live event mismatch: %+v", second)
	}

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("expected channel closed after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("channel did not close after ctx cancellation")
	}
}

func TestWatchEventsResumeAfterCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-watch-resume")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	firstID, err := s.AppendEvent(ctx, j.ID, store.Event{Type: "first"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	secondID, err := s.AppendEvent(ctx, j.ID, store.Event{Type: "second"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	ch, err := s.WatchEvents(ctx, j.ID, firstID)
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	ev := recvEvent(t, ch)
	if ev.ID != secondID {
		t.Fatalf("expected resume from cursor to yield %s, got %s", secondID, ev.ID)
	}
}

func recvEvent(t *testing.T, ch <-chan store.Event) store.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event")
		return store.Event{}
	}
}
