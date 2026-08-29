package redisstore

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rxbynerd/hairpin/internal/store"
)

// pollInterval is how often WatchEvents polls the stream tail for new
// entries once history has been replayed.
const pollInterval = 150 * time.Millisecond

func (r *redisStore) jobExists(ctx context.Context, jobID string) (bool, error) {
	n, err := r.client.Exists(ctx, jobKey(jobID)).Result()
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

func (r *redisStore) AppendEvent(ctx context.Context, jobID string, ev store.Event) (string, error) {
	ok, err := r.jobExists(ctx, jobID)
	if err != nil {
		return "", fmt.Errorf("append event job %s: %w", jobID, err)
	}
	if !ok {
		return "", notFoundf("job %s", jobID)
	}

	at := ev.At
	if at.IsZero() {
		at = time.Now()
	}
	id, err := r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: eventsKey(jobID),
		MaxLen: r.maxEvents,
		Approx: true,
		Values: map[string]any{
			"type":    ev.Type,
			"payload": ev.PayloadJSON,
			"at":      at.Format(time.RFC3339Nano),
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("append event job %s: %w", jobID, err)
	}
	return id, nil
}

func eventFromMessage(msg redis.XMessage) (store.Event, error) {
	ev := store.Event{ID: msg.ID}
	if v, ok := msg.Values["type"]; ok {
		ev.Type, _ = v.(string)
	}
	if v, ok := msg.Values["payload"]; ok {
		ev.PayloadJSON, _ = v.(string)
	}
	if v, ok := msg.Values["at"].(string); ok && v != "" {
		at, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return store.Event{}, fmt.Errorf("event %s field at: %w", msg.ID, err)
		}
		ev.At = at
	}
	return ev, nil
}

func (r *redisStore) ReadEvents(ctx context.Context, jobID string, afterID string, limit int) ([]store.Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	ok, err := r.jobExists(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("read events job %s: %w", jobID, err)
	}
	if !ok {
		return nil, notFoundf("job %s", jobID)
	}

	start := "-"
	if afterID != "" {
		start = "(" + afterID
	}
	msgs, err := r.client.XRangeN(ctx, eventsKey(jobID), start, "+", int64(limit)).Result()
	if err != nil {
		return nil, fmt.Errorf("read events job %s: %w", jobID, err)
	}

	out := make([]store.Event, 0, len(msgs))
	for _, msg := range msgs {
		ev, err := eventFromMessage(msg)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func (r *redisStore) WatchEvents(ctx context.Context, jobID string, afterID string) (<-chan store.Event, error) {
	ok, err := r.jobExists(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("watch events job %s: %w", jobID, err)
	}
	if !ok {
		return nil, notFoundf("job %s", jobID)
	}

	out := make(chan store.Event)
	go func() {
		defer close(out)
		cursor := afterID
		for {
			evs, err := r.ReadEvents(ctx, jobID, cursor, 100)
			if err != nil {
				return
			}
			for _, ev := range evs {
				select {
				case out <- ev:
					cursor = ev.ID
				case <-ctx.Done():
					return
				}
			}
			if len(evs) > 0 {
				continue
			}
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
