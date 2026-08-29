package redisstore

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

// newTestStore returns a Store backed by a fresh miniredis instance,
// cleaned up when the test ends.
func newTestStore(t *testing.T, opts Options) store.Store {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	s := New(client, opts)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestJob(id string) *job.Job {
	return &job.Job{
		ID:        id,
		Status:    job.StatusQueued,
		Prompt:    "do the thing",
		Profile:   "default",
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
}
