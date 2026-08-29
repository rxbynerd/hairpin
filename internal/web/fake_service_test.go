package web

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

// fakeService is a minimal in-package Service double for handler tests.
// It is not a general-purpose fake: List returns jobs in insertion
// order (newest last reversed), and Watch replays whatever events were
// queued via addEvents before returning history, then streams anything
// pushed afterward until closeWatch or the job is marked terminal.
type fakeService struct {
	mu sync.Mutex

	jobs        map[string]*job.Job
	order       []string
	permissions map[string][]store.PermissionRequest
	events      map[string][]store.Event

	submitErr error
	getErr    error

	answered []answeredPermission
	watchers map[string][]chan store.Event
}

type answeredPermission struct {
	jobID     string
	requestID string
	allow     bool
	reason    string
}

func newFakeService() *fakeService {
	return &fakeService{
		jobs:        make(map[string]*job.Job),
		permissions: make(map[string][]store.PermissionRequest),
		events:      make(map[string][]store.Event),
		watchers:    make(map[string][]chan store.Event),
	}
}

func (f *fakeService) addJob(j *job.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[j.ID] = j
	f.order = append(f.order, j.ID)
}

func (f *fakeService) setPermissions(jobID string, perms []store.PermissionRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissions[jobID] = perms
}

func (f *fakeService) Submit(_ context.Context, p SubmitParams) (*job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	j := &job.Job{
		ID:        job.NewID(),
		Status:    job.StatusQueued,
		Prompt:    p.Prompt,
		Profile:   p.Profile,
		CreatedAt: time.Now(),
	}
	f.jobs[j.ID] = j
	f.order = append(f.order, j.ID)
	return j, nil
}

func (f *fakeService) Get(_ context.Context, id string) (*job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	j, ok := f.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s: %w", id, store.ErrNotFound)
	}
	return j, nil
}

func (f *fakeService) List(_ context.Context, limit int, _ string) ([]*job.Job, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*job.Job
	for i := len(f.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, f.jobs[f.order[i]])
	}
	return out, "", nil
}

func (f *fakeService) Cancel(_ context.Context, id string) (*job.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s: %w", id, store.ErrNotFound)
	}
	j.Status = job.StatusCancelled
	return j, nil
}

func (f *fakeService) ListPermissions(_ context.Context, jobID string) ([]store.PermissionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.permissions[jobID], nil
}

func (f *fakeService) AnswerPermission(_ context.Context, jobID, requestID string, allow bool, reason string) (store.PermissionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, answeredPermission{jobID: jobID, requestID: requestID, allow: allow, reason: reason})
	state := store.PermissionAllowed
	if !allow {
		state = store.PermissionDenied
	}
	return store.PermissionRequest{RequestID: requestID, State: state, Reason: reason}, nil
}

func (f *fakeService) Watch(ctx context.Context, id, afterID string) (<-chan store.Event, error) {
	f.mu.Lock()
	if _, ok := f.jobs[id]; !ok {
		f.mu.Unlock()
		return nil, fmt.Errorf("job %s: %w", id, store.ErrNotFound)
	}
	ch := make(chan store.Event, 16)
	for _, ev := range f.events[id] {
		ch <- ev
	}
	f.watchers[id] = append(f.watchers[id], ch)
	f.mu.Unlock()

	go func() {
		<-ctx.Done()
	}()
	return ch, nil
}

// pushEvent sends ev to every open watcher of jobID.
func (f *fakeService) pushEvent(jobID string, ev store.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.watchers[jobID] {
		ch <- ev
	}
}

// closeWatchers closes every open watcher channel for jobID, simulating
// the job reaching a terminal status.
func (f *fakeService) closeWatchers(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.watchers[jobID] {
		close(ch)
	}
	f.watchers[jobID] = nil
}
