// Package store defines hairpin's persistence contract and an in-memory
// implementation. The Redis implementation lives in the redisstore
// subpackage.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/rxbynerd/hairpin/internal/job"
)

// ErrNotFound is returned when a job, event, or permission request does
// not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a create collides with an existing key or
// an update lost a concurrent-modification race beyond retry.
var ErrConflict = errors.New("conflict")

// Event is one recorded harness (or hairpin-synthesised) event on a
// job's timeline.
type Event struct {
	// ID is the store-assigned, monotonically increasing position (in
	// Redis stream ID form, "millis-seq"). Callers treat it as an opaque
	// resume cursor.
	ID string
	// Type is the HarnessEvent type discriminator, or a hairpin
	// synthetic type such as "status_change".
	Type string
	// PayloadJSON carries the originating event in protobuf-JSON form.
	// May be empty for payload-free events (heartbeats).
	PayloadJSON string
	At          time.Time
}

// PermissionState is the lifecycle of a recorded permission request.
type PermissionState string

const (
	PermissionPending PermissionState = "pending"
	PermissionAllowed PermissionState = "allowed"
	PermissionDenied  PermissionState = "denied"
)

// PermissionRequest is a harness permission_request and its decision.
type PermissionRequest struct {
	RequestID   string
	ToolName    string
	InputJSON   string
	State       PermissionState
	Reason      string
	RequestedAt time.Time
	AnsweredAt  time.Time
}

// Store persists jobs, their event timelines, and permission requests.
// All methods are safe for concurrent use.
type Store interface {
	// CreateJob persists a new job. ErrConflict if the ID exists.
	CreateJob(ctx context.Context, j *job.Job) error

	// GetJob returns the job or ErrNotFound.
	GetJob(ctx context.Context, id string) (*job.Job, error)

	// ListJobs returns up to limit jobs, newest first, resuming from
	// pageToken (empty for the first page). The returned token is empty
	// on the last page.
	ListJobs(ctx context.Context, limit int, pageToken string) ([]*job.Job, string, error)

	// UpdateJob atomically applies fn to the current job record and
	// persists the result. fn may be retried on contention; it must be
	// side-effect free. Returns the updated job.
	UpdateJob(ctx context.Context, id string, fn func(*job.Job) error) (*job.Job, error)

	// DeleteJob permanently removes a job: its record, its event
	// timeline, its permission requests, and its entry in the job
	// index. ErrNotFound if the job does not exist.
	DeleteJob(ctx context.Context, id string) error

	// AppendEvent appends to the job's timeline and returns the assigned
	// event ID. Implementations cap the timeline length (oldest events
	// are dropped).
	AppendEvent(ctx context.Context, jobID string, ev Event) (string, error)

	// ReadEvents returns up to limit events strictly after afterID
	// (empty afterID reads from the start).
	ReadEvents(ctx context.Context, jobID string, afterID string, limit int) ([]Event, error)

	// WatchEvents streams events strictly after afterID: recorded
	// history first, then live appends, until ctx is done. The channel
	// is closed on ctx cancellation or unrecoverable error.
	WatchEvents(ctx context.Context, jobID string, afterID string) (<-chan Event, error)

	// PutPermission upserts a permission request keyed by RequestID.
	PutPermission(ctx context.Context, jobID string, p PermissionRequest) error

	// GetPermission returns one permission request or ErrNotFound.
	GetPermission(ctx context.Context, jobID, requestID string) (PermissionRequest, error)

	// ListPermissions returns the job's permission requests, pending
	// first, then by request time.
	ListPermissions(ctx context.Context, jobID string) ([]PermissionRequest, error)

	// Close releases underlying resources.
	Close() error
}
