package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rxbynerd/hairpin/internal/job"
)

// memStore is an in-memory Store for tests and single-process dev runs.
type memStore struct {
	mu       sync.Mutex
	jobs     map[string]*job.Job
	order    []string // job IDs in creation order
	events   map[string][]Event
	perms    map[string]map[string]PermissionRequest
	watchers map[string][]*memWatcher
	seq      uint64
	maxLen   int
}

type memWatcher struct {
	ch     chan Event
	ctx    context.Context
	jobID  string
	closed bool
}

// NewMemStore returns an in-memory Store. Event timelines are capped at
// maxEvents per job (<=0 uses the default 10000, matching redisstore).
func NewMemStore(maxEvents int) Store {
	if maxEvents <= 0 {
		maxEvents = 10000
	}
	return &memStore{
		jobs:     make(map[string]*job.Job),
		events:   make(map[string][]Event),
		perms:    make(map[string]map[string]PermissionRequest),
		watchers: make(map[string][]*memWatcher),
		maxLen:   maxEvents,
	}
}

func (m *memStore) CreateJob(_ context.Context, j *job.Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[j.ID]; ok {
		return fmt.Errorf("job %s: %w", j.ID, ErrConflict)
	}
	cp := *j
	m.jobs[j.ID] = &cp
	m.order = append(m.order, j.ID)
	return nil
}

func (m *memStore) GetJob(_ context.Context, id string) (*job.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s: %w", id, ErrNotFound)
	}
	cp := *j
	return &cp, nil
}

func (m *memStore) ListJobs(_ context.Context, limit int, pageToken string) ([]*job.Job, string, error) {
	if limit <= 0 {
		limit = 50
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Newest first: walk creation order backwards from the cursor.
	start := len(m.order) - 1
	if pageToken != "" {
		var idx int
		if _, err := fmt.Sscanf(pageToken, "%d", &idx); err != nil {
			return nil, "", fmt.Errorf("bad page token %q", pageToken)
		}
		start = idx
	}
	var out []*job.Job
	i := start
	for ; i >= 0 && len(out) < limit; i-- {
		if j, ok := m.jobs[m.order[i]]; ok {
			cp := *j
			out = append(out, &cp)
		}
	}
	next := ""
	if i >= 0 {
		next = fmt.Sprintf("%d", i)
	}
	return out, next, nil
}

func (m *memStore) UpdateJob(_ context.Context, id string, fn func(*job.Job) error) (*job.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s: %w", id, ErrNotFound)
	}
	cp := *j
	if err := fn(&cp); err != nil {
		return nil, err
	}
	m.jobs[id] = &cp
	out := cp
	return &out, nil
}

func (m *memStore) DeleteJob(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[id]; !ok {
		return fmt.Errorf("job %s: %w", id, ErrNotFound)
	}
	delete(m.jobs, id)
	delete(m.events, id)
	delete(m.perms, id)
	delete(m.watchers, id)
	for i, cand := range m.order {
		if cand == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return nil
}

func (m *memStore) AppendEvent(_ context.Context, jobID string, ev Event) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[jobID]; !ok {
		return "", fmt.Errorf("job %s: %w", jobID, ErrNotFound)
	}
	m.seq++
	// Redis-stream-shaped IDs keep cursor semantics identical across
	// implementations.
	ev.ID = fmt.Sprintf("%d-%d", ev.At.UnixMilli(), m.seq)
	if ev.At.IsZero() {
		ev.At = time.Now()
		ev.ID = fmt.Sprintf("%d-%d", ev.At.UnixMilli(), m.seq)
	}
	evs := append(m.events[jobID], ev)
	if len(evs) > m.maxLen {
		evs = evs[len(evs)-m.maxLen:]
	}
	m.events[jobID] = evs
	// Non-blocking: a stalled watcher loses events rather than wedging
	// the store mutex for every other caller. The buffer is sized to the
	// full timeline cap, so this only trips on a pathological consumer.
	for _, w := range m.watchers[jobID] {
		select {
		case w.ch <- ev:
		default:
		}
	}
	return ev.ID, nil
}

func (m *memStore) ReadEvents(_ context.Context, jobID string, afterID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[jobID]; !ok {
		return nil, fmt.Errorf("job %s: %w", jobID, ErrNotFound)
	}
	var out []Event
	for _, ev := range m.events[jobID] {
		if afterID != "" && !streamIDLess(afterID, ev.ID) {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) WatchEvents(ctx context.Context, jobID string, afterID string) (<-chan Event, error) {
	m.mu.Lock()
	if _, ok := m.jobs[jobID]; !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("job %s: %w", jobID, ErrNotFound)
	}
	// Buffer sized so history replay never blocks the appender.
	w := &memWatcher{ch: make(chan Event, m.maxLen+64), ctx: ctx, jobID: jobID}
	for _, ev := range m.events[jobID] {
		if afterID != "" && !streamIDLess(afterID, ev.ID) {
			continue
		}
		w.ch <- ev
	}
	m.watchers[jobID] = append(m.watchers[jobID], w)
	m.mu.Unlock()

	out := make(chan Event)
	go func() {
		defer close(out)
		defer m.removeWatcher(w)
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-w.ch:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (m *memStore) removeWatcher(w *memWatcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws := m.watchers[w.jobID]
	for i, cand := range ws {
		if cand == w {
			m.watchers[w.jobID] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
}

func (m *memStore) PutPermission(_ context.Context, jobID string, p PermissionRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[jobID]; !ok {
		return fmt.Errorf("job %s: %w", jobID, ErrNotFound)
	}
	if m.perms[jobID] == nil {
		m.perms[jobID] = make(map[string]PermissionRequest)
	}
	m.perms[jobID][p.RequestID] = p
	return nil
}

func (m *memStore) GetPermission(_ context.Context, jobID, requestID string) (PermissionRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.perms[jobID][requestID]
	if !ok {
		return PermissionRequest{}, fmt.Errorf("permission %s/%s: %w", jobID, requestID, ErrNotFound)
	}
	return p, nil
}

func (m *memStore) ListPermissions(_ context.Context, jobID string) ([]PermissionRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PermissionRequest
	for _, p := range m.perms[jobID] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, k int) bool {
		if (out[i].State == PermissionPending) != (out[k].State == PermissionPending) {
			return out[i].State == PermissionPending
		}
		return out[i].RequestedAt.Before(out[k].RequestedAt)
	})
	return out, nil
}

func (m *memStore) Close() error { return nil }

// streamIDLess compares two Redis-stream-shaped IDs ("millis-seq")
// numerically per component.
func streamIDLess(a, b string) bool {
	var am, as, bm, bs uint64
	_, _ = fmt.Sscanf(a, "%d-%d", &am, &as)
	_, _ = fmt.Sscanf(b, "%d-%d", &bm, &bs)
	if am != bm {
		return am < bm
	}
	return as < bs
}
