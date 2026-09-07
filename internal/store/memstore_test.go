package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rxbynerd/hairpin/internal/job"
)

func newTestMemJob(id string) *job.Job {
	return &job.Job{
		ID:        id,
		Status:    job.StatusQueued,
		Prompt:    "do the thing",
		Profile:   "default",
		CreatedAt: time.Now().UTC(),
	}
}

func TestMemStoreDeleteJob(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore(0)
	t.Cleanup(func() { _ = s.Close() })

	j := newTestMemJob("hp-del")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := s.AppendEvent(ctx, j.ID, Event{Type: "text_delta"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := s.PutPermission(ctx, j.ID, PermissionRequest{RequestID: "req-1", State: PermissionPending}); err != nil {
		t.Fatalf("PutPermission: %v", err)
	}

	if err := s.DeleteJob(ctx, j.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	if _, err := s.GetJob(ctx, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetJob after delete: got %v, want ErrNotFound", err)
	}
	if _, err := s.ReadEvents(ctx, j.ID, "", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadEvents after delete: got %v, want ErrNotFound", err)
	}
	if _, err := s.GetPermission(ctx, j.ID, "req-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPermission after delete: got %v, want ErrNotFound", err)
	}

	jobs, _, err := s.ListJobs(ctx, 50, "")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, cand := range jobs {
		if cand.ID == j.ID {
			t.Fatalf("deleted job %s still present in ListJobs", j.ID)
		}
	}
}

func TestMemStoreDeleteJobNotFound(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore(0)
	t.Cleanup(func() { _ = s.Close() })

	if err := s.DeleteJob(ctx, "hp-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteJob: got %v, want ErrNotFound", err)
	}
}

func TestMemStoreDeleteJobLeavesOthersIntact(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore(0)
	t.Cleanup(func() { _ = s.Close() })

	a := newTestMemJob("hp-a")
	b := newTestMemJob("hp-b")
	if err := s.CreateJob(ctx, a); err != nil {
		t.Fatalf("CreateJob a: %v", err)
	}
	if err := s.CreateJob(ctx, b); err != nil {
		t.Fatalf("CreateJob b: %v", err)
	}

	if err := s.DeleteJob(ctx, a.ID); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}

	got, err := s.GetJob(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetJob b: %v", err)
	}
	if got.ID != b.ID {
		t.Fatalf("GetJob b mismatch: %+v", got)
	}
}

func TestMemStoreDeleteJobEndsWatch(t *testing.T) {
	m := NewMemStore(0)
	t.Cleanup(func() { _ = m.Close() })
	ctx := context.Background()
	if err := m.CreateJob(ctx, newTestMemJob("hp-watch")); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	ch, err := m.WatchEvents(ctx, "hp-watch", "")
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	if err := m.DeleteJob(ctx, "hp-watch"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("want closed channel after DeleteJob, got an event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watch channel not closed after DeleteJob")
	}
}
