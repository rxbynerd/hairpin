// Package redisstore is a Redis-backed implementation of
// internal/store.Store. Jobs are hashes, event timelines are capped
// streams, permission requests are a hash keyed by request ID, and job
// listing rides a lex-sorted zset keyed on the (time-sortable) job ID.
// See docs/design.md for the key layout.
//
// WatchEvents replays history with XRANGE and then polls the stream tail
// every pollInterval rather than blocking on XREAD BLOCK: it keeps
// behaviour identical against real Redis and miniredis (whose blocking
// XREAD support is not exercised here), at the cost of up to one poll
// interval of latency on live events.
package redisstore

import (
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/rxbynerd/hairpin/internal/store"
)

// DefaultMaxEvents is the default per-job event stream cap.
const DefaultMaxEvents = 10000

// Options configures a redisstore.
type Options struct {
	// MaxEvents caps each job's event stream (approximate trimming via
	// XADD MAXLEN ~). <=0 uses DefaultMaxEvents.
	MaxEvents int64
}

type redisStore struct {
	client    *redis.Client
	maxEvents int64
}

// New returns a Store backed by client. The caller owns client
// construction and lifetime beyond Close, which closes it.
func New(client *redis.Client, opts Options) store.Store {
	maxEvents := opts.MaxEvents
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	return &redisStore{client: client, maxEvents: maxEvents}
}

func (r *redisStore) Close() error {
	return r.client.Close()
}

const jobsIndexKey = "hairpin:jobs"

func jobKey(id string) string {
	return "hairpin:job:" + id
}

func eventsKey(id string) string {
	return jobKey(id) + ":events"
}

func permsKey(id string) string {
	return jobKey(id) + ":perms"
}

// notFoundf wraps store.ErrNotFound with context.
func notFoundf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, store.ErrNotFound)...)
}

// conflictf wraps store.ErrConflict with context.
func conflictf(format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, store.ErrConflict)...)
}
