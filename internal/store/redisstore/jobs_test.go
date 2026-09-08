package redisstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

func TestCreateAndGetJob(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	j := newTestJob("hp-001")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ID != j.ID || got.Status != j.Status || got.Prompt != j.Prompt || got.Profile != j.Profile {
		t.Fatalf("GetJob mismatch: got %+v, want %+v", got, j)
	}
	if !got.CreatedAt.Equal(j.CreatedAt) {
		t.Fatalf("CreatedAt mismatch: got %v, want %v", got.CreatedAt, j.CreatedAt)
	}
	if !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() || !got.LastEventAt.IsZero() {
		t.Fatalf("expected zero optional times, got %+v", got)
	}
}

func TestJobRepoScopeRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	j := newTestJob("hp-scope")
	j.RepoScope = []string{"github.com/rxbynerd/*", "github.com/rxbynerd-forks/hairpin"}
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !slices.Equal(got.RepoScope, j.RepoScope) {
		t.Fatalf("RepoScope = %v, want %v", got.RepoScope, j.RepoScope)
	}
}

func TestJobRepoScopeEmptyOmitted(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	j := newTestJob("hp-noscope")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if len(got.RepoScope) != 0 {
		t.Fatalf("RepoScope = %v, want empty", got.RepoScope)
	}
}

func TestGetJobNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	_, err := s.GetJob(ctx, "hp-missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetJob: got %v, want ErrNotFound", err)
	}
}

func TestCreateJobConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	j := newTestJob("hp-dup")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	err := s.CreateJob(ctx, newTestJob("hp-dup"))
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateJob duplicate: got %v, want ErrConflict", err)
	}
}

func TestUpdateJob(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	j := newTestJob("hp-update")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	startedAt := time.Now().UTC().Truncate(time.Millisecond)
	updated, err := s.UpdateJob(ctx, j.ID, func(j *job.Job) error {
		j.Status = job.StatusRunning
		j.StartedAt = startedAt
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if updated.Status != job.StatusRunning || !updated.StartedAt.Equal(startedAt) {
		t.Fatalf("UpdateJob result mismatch: %+v", updated)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusRunning || !got.StartedAt.Equal(startedAt) {
		t.Fatalf("persisted job mismatch: %+v", got)
	}
}

func TestUpdateJobNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	_, err := s.UpdateJob(ctx, "hp-missing", func(j *job.Job) error { return nil })
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateJob: got %v, want ErrNotFound", err)
	}
}

func TestUpdateJobFnErrorNotPersisted(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	j := newTestJob("hp-fnerr")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	sentinel := errors.New("boom")
	_, err := s.UpdateJob(ctx, j.ID, func(j *job.Job) error {
		j.Status = job.StatusFailed
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("UpdateJob: got %v, want sentinel", err)
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusQueued {
		t.Fatalf("fn error must not persist: got status %v", got.Status)
	}
}

// TestUpdateJobCASContention drives concurrent increments through
// UpdateJob and checks every increment lands: the optimistic CAS loop
// must retry losers rather than silently drop updates.
func TestUpdateJobCASContention(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	j := newTestJob("hp-cas")
	j.Prompt = "0"
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	const n = 25
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.UpdateJob(ctx, j.ID, func(j *job.Job) error {
				cur, err := strconv.Atoi(j.Prompt)
				if err != nil {
					return err
				}
				j.Prompt = strconv.Itoa(cur + 1)
				return nil
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("UpdateJob[%d]: %v", i, err)
		}
	}

	got, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Prompt != strconv.Itoa(n) {
		t.Fatalf("counter mismatch: got %q, want %q", got.Prompt, strconv.Itoa(n))
	}
}

func TestListJobsPagination(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	const n = 25
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("hp-%03d", i)
		ids = append(ids, id)
		if err := s.CreateJob(ctx, newTestJob(id)); err != nil {
			t.Fatalf("CreateJob(%s): %v", id, err)
		}
	}

	var seen []string
	token := ""
	for page := 0; page < n+1; page++ {
		jobs, next, err := s.ListJobs(ctx, 7, token)
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		for _, j := range jobs {
			seen = append(seen, j.ID)
		}
		if next == "" {
			break
		}
		token = next
	}

	if len(seen) != n {
		t.Fatalf("got %d jobs across pages, want %d", len(seen), n)
	}
	for i, id := range seen {
		want := ids[n-1-i] // newest (highest suffix) first
		if id != want {
			t.Fatalf("position %d: got %s, want %s", i, id, want)
		}
	}
}

func TestListJobsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	jobs, next, err := s.ListJobs(ctx, 10, "")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 || next != "" {
		t.Fatalf("expected empty page, got %d jobs, next=%q", len(jobs), next)
	}
}
