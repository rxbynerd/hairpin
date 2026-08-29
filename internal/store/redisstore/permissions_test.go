package redisstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rxbynerd/hairpin/internal/store"
)

func TestPermissionLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-perms")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	req := store.PermissionRequest{
		RequestID:   "req-1",
		ToolName:    "bash",
		InputJSON:   `{"cmd":"ls"}`,
		State:       store.PermissionPending,
		RequestedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := s.PutPermission(ctx, j.ID, req); err != nil {
		t.Fatalf("PutPermission: %v", err)
	}

	got, err := s.GetPermission(ctx, j.ID, "req-1")
	if err != nil {
		t.Fatalf("GetPermission: %v", err)
	}
	if got != req {
		t.Fatalf("GetPermission mismatch: got %+v, want %+v", got, req)
	}

	req.State = store.PermissionAllowed
	req.AnsweredAt = time.Now().UTC().Truncate(time.Millisecond)
	if err := s.PutPermission(ctx, j.ID, req); err != nil {
		t.Fatalf("PutPermission (update): %v", err)
	}

	got, err = s.GetPermission(ctx, j.ID, "req-1")
	if err != nil {
		t.Fatalf("GetPermission after update: %v", err)
	}
	if got.State != store.PermissionAllowed || !got.AnsweredAt.Equal(req.AnsweredAt) {
		t.Fatalf("permission not updated: %+v", got)
	}
}

func TestGetPermissionNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-perms-missing")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	_, err := s.GetPermission(ctx, j.ID, "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetPermission: got %v, want ErrNotFound", err)
	}
}

func TestPutPermissionJobNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})

	err := s.PutPermission(ctx, "hp-missing", store.PermissionRequest{RequestID: "req-1"})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PutPermission: got %v, want ErrNotFound", err)
	}
}

func TestListPermissionsOrdering(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-perms-list")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Millisecond)
	reqs := []store.PermissionRequest{
		{RequestID: "answered-early", State: store.PermissionAllowed, RequestedAt: base},
		{RequestID: "pending-late", State: store.PermissionPending, RequestedAt: base.Add(2 * time.Second)},
		{RequestID: "pending-early", State: store.PermissionPending, RequestedAt: base.Add(time.Second)},
		{RequestID: "answered-late", State: store.PermissionDenied, RequestedAt: base.Add(3 * time.Second)},
	}
	for _, r := range reqs {
		if err := s.PutPermission(ctx, j.ID, r); err != nil {
			t.Fatalf("PutPermission(%s): %v", r.RequestID, err)
		}
	}

	got, err := s.ListPermissions(ctx, j.ID)
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d permissions, want 4", len(got))
	}
	want := []string{"pending-early", "pending-late", "answered-early", "answered-late"}
	for i, id := range want {
		if got[i].RequestID != id {
			t.Fatalf("position %d: got %s, want %s (full: %+v)", i, got[i].RequestID, id, got)
		}
	}
}

func TestListPermissionsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, Options{})
	j := newTestJob("hp-perms-empty")
	if err := s.CreateJob(ctx, j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	got, err := s.ListPermissions(ctx, j.ID)
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no permissions, got %+v", got)
	}
}
