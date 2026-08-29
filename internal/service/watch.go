package service

import (
	"context"
	"encoding/json"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

// historyPage bounds one ReadEvents call when replaying a finished
// job's timeline.
const historyPage = 500

// Watch streams a job's events after afterID: recorded history first,
// then live appends. The channel closes once the job reaches a terminal
// status (after delivering the event that announced it), when ctx is
// done, or on store failure — so callers can range over it without a
// separate completion signal.
func (s *Service) Watch(ctx context.Context, id, afterID string) (<-chan store.Event, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.Status.Terminal() {
		return s.replay(ctx, id, afterID), nil
	}

	// A dedicated context releases the store watch as soon as this
	// wrapper stops (terminal event), not when the caller's ctx ends.
	wctx, wcancel := context.WithCancel(ctx)
	src, err := s.store.WatchEvents(wctx, id, afterID)
	if err != nil {
		wcancel()
		return nil, err
	}
	out := make(chan store.Event)
	go func() {
		defer close(out)
		defer wcancel()
		for ev := range src {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
			if isTerminalStatusEvent(ev) {
				return
			}
		}
	}()
	return out, nil
}

// replay serves the timeline of an already-terminal job. No further
// events are expected, so the channel closes when history runs out.
func (s *Service) replay(ctx context.Context, id, afterID string) <-chan store.Event {
	out := make(chan store.Event)
	go func() {
		defer close(out)
		cursor := afterID
		for {
			evs, err := s.store.ReadEvents(ctx, id, cursor, historyPage)
			if err != nil {
				s.log.Error("read job events", "job", id, "err", err)
				return
			}
			if len(evs) == 0 {
				return
			}
			for _, ev := range evs {
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
			cursor = evs[len(evs)-1].ID
		}
	}()
	return out
}

// isTerminalStatusEvent reports whether ev announces a terminal status.
func isTerminalStatusEvent(ev store.Event) bool {
	if ev.Type != EventTypeStatusChange {
		return false
	}
	var p statusPayload
	if err := json.Unmarshal([]byte(ev.PayloadJSON), &p); err != nil {
		return false
	}
	return job.Status(p.Status).Terminal()
}
