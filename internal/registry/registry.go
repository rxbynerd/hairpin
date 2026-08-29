// Package registry tracks live harness sessions so API handlers can
// route control events (permission decisions, cancels) onto the open
// RunTask stream for a job. In-process only: running multiple hairpin
// replicas requires moving this bridge to a shared channel such as
// Redis pub/sub.
package registry

import (
	"errors"
	"sync"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
)

// ErrNotConnected is returned when no live harness stream is registered
// for the job.
var ErrNotConnected = errors.New("no live harness session for job")

// ErrAlreadyConnected is returned when a second harness claims a job
// that already has a live session.
var ErrAlreadyConnected = errors.New("harness session already registered for job")

// Session is a live harness stream. Implementations must make Send safe
// for concurrent use.
type Session interface {
	Send(ev *harnessv1.ControlEvent) error
}

// Registry maps job IDs to live sessions.
type Registry struct {
	mu sync.Mutex
	m  map[string]Session
}

func New() *Registry {
	return &Registry{m: make(map[string]Session)}
}

// Register claims the job for s. ErrAlreadyConnected if another session
// holds it — the caller should reject the duplicate harness.
func (r *Registry) Register(jobID string, s Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[jobID]; ok {
		return ErrAlreadyConnected
	}
	r.m[jobID] = s
	return nil
}

// Unregister removes the mapping, but only if it still points at s, so
// a slow-exiting stream cannot evict its replacement.
func (r *Registry) Unregister(jobID string, s Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[jobID]; ok && cur == s {
		delete(r.m, jobID)
	}
}

// Send delivers a control event to the job's live session.
func (r *Registry) Send(jobID string, ev *harnessv1.ControlEvent) error {
	r.mu.Lock()
	s, ok := r.m[jobID]
	r.mu.Unlock()
	if !ok {
		return ErrNotConnected
	}
	return s.Send(ev)
}

// JobIDs returns the IDs of all jobs with live sessions.
func (r *Registry) JobIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.m))
	for id := range r.m {
		ids = append(ids, id)
	}
	return ids
}

// Connected reports whether a live session exists for the job.
func (r *Registry) Connected(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.m[jobID]
	return ok
}
