// Package service holds hairpin's caller-facing operations — submit,
// inspect, watch, cancel, and permission answering — shared by the
// connect API (internal/api) and the web UI (internal/web). It owns job
// lifecycle transitions that originate from callers; transitions driven
// by the harness stream belong to internal/controlplane.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/launcher"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

// ErrInvalidArgument marks a caller mistake: a malformed RunConfig, an
// unknown profile, a missing prompt, or an answer to a request that is
// no longer pending. The API layer maps it to CodeInvalidArgument.
var ErrInvalidArgument = errors.New("invalid argument")

// ErrNotFound aliases store.ErrNotFound so callers need only import
// this package.
var ErrNotFound = store.ErrNotFound

// ErrNotConnected aliases registry.ErrNotConnected: the operation
// needed a live harness stream and there was none.
var ErrNotConnected = registry.ErrNotConnected

// EventTypeStatusChange is the synthetic event type hairpin appends to
// a job's timeline whenever its status changes. The payload is a JSON
// object with a "status" field and, on failure, an "error" field.
const EventTypeStatusChange = "status_change"

// defaultLaunchTimeout bounds the background launch of one harness.
const defaultLaunchTimeout = 60 * time.Second

// Service performs hairpin's caller-facing job operations.
type Service struct {
	store    store.Store
	registry *registry.Registry
	launcher launcher.Launcher
	profiles *Profiles
	log      *slog.Logger

	launchTimeout time.Duration
	launches      sync.WaitGroup
}

// New returns a Service. A nil logger discards output; a nil profiles
// set means every submit must carry its own run_config_json.
func New(st store.Store, reg *registry.Registry, l launcher.Launcher, profiles *Profiles, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if profiles == nil {
		profiles = &Profiles{}
	}
	return &Service{
		store:         st,
		registry:      reg,
		launcher:      l,
		profiles:      profiles,
		log:           logger,
		launchTimeout: defaultLaunchTimeout,
	}
}

// Profiles returns the configured RunConfig templates, for surfaces
// that offer a profile picker.
func (s *Service) Profiles() *Profiles { return s.profiles }

// Get returns one job, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (*job.Job, error) {
	return s.store.GetJob(ctx, id)
}

// List returns up to limit jobs newest-first, resuming from pageToken,
// plus the token for the next page ("" on the last page).
func (s *Service) List(ctx context.Context, limit int, pageToken string) ([]*job.Job, string, error) {
	return s.store.ListJobs(ctx, limit, pageToken)
}

// ListPermissions returns a job's permission requests, pending first.
func (s *Service) ListPermissions(ctx context.Context, jobID string) ([]store.PermissionRequest, error) {
	return s.store.ListPermissions(ctx, jobID)
}

// WaitForLaunches blocks until every in-flight background launch has
// settled. Callers use it for graceful shutdown.
func (s *Service) WaitForLaunches() { s.launches.Wait() }

// Cancel requests cancellation of a job.
//
// A job with a live harness session is asked to stop over that stream;
// it reaches its terminal status when the harness reports done. A job
// with no session is cancelled here and now — a harness that dials in
// later is cancelled by the control plane instead of being assigned
// work. Cancelling a terminal job is a no-op.
func (s *Service) Cancel(ctx context.Context, id string) (*job.Job, error) {
	j, err := s.store.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.Status.Terminal() {
		return j, nil
	}

	if s.registry.Connected(id) {
		j, err = s.store.UpdateJob(ctx, id, func(j *job.Job) error {
			j.CancelRequested = true
			return nil
		})
		if err != nil {
			return nil, err
		}
		err = s.registry.Send(id, &harnessv1.ControlEvent{Type: "cancel"})
		switch {
		case err == nil:
			return j, nil
		case errors.Is(err, registry.ErrNotConnected):
			// The session went away between the check and the send;
			// fall through and finalise locally.
		default:
			return nil, fmt.Errorf("send cancel to job %s: %w", id, err)
		}
	}

	return s.cancelUnassigned(ctx, id)
}

// cancelUnassigned finalises a job that has no live harness to stop.
func (s *Service) cancelUnassigned(ctx context.Context, id string) (*job.Job, error) {
	var cancelled bool
	j, err := s.store.UpdateJob(ctx, id, func(j *job.Job) error {
		if j.Status.Terminal() {
			return nil
		}
		j.Status = job.StatusCancelled
		j.CancelRequested = true
		j.FinishedAt = time.Now().UTC()
		cancelled = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cancelled {
		s.appendStatusEvent(ctx, id, job.StatusCancelled, "")
	}
	return j, nil
}

// AnswerPermission resolves a pending permission request by routing the
// decision onto the job's live harness stream. The decision is recorded
// only once the harness has accepted it.
func (s *Service) AnswerPermission(ctx context.Context, jobID, requestID string, allow bool, reason string) (store.PermissionRequest, error) {
	p, err := s.store.GetPermission(ctx, jobID, requestID)
	if err != nil {
		return store.PermissionRequest{}, err
	}
	if p.State != store.PermissionPending {
		return store.PermissionRequest{}, fmt.Errorf("permission %s is already %s: %w", requestID, p.State, ErrInvalidArgument)
	}

	err = s.registry.Send(jobID, &harnessv1.ControlEvent{
		Type:      "permission_response",
		RequestId: requestID,
		Allowed:   &harnessv1.OptionalBool{Value: allow},
		Reason:    reason,
	})
	if err != nil {
		if errors.Is(err, registry.ErrNotConnected) {
			return store.PermissionRequest{}, fmt.Errorf("job %s has no live harness to answer permission %s: %w", jobID, requestID, ErrNotConnected)
		}
		return store.PermissionRequest{}, fmt.Errorf("send permission response for job %s: %w", jobID, err)
	}

	p.State = store.PermissionAllowed
	if !allow {
		p.State = store.PermissionDenied
	}
	p.Reason = reason
	p.AnsweredAt = time.Now().UTC()
	if err := s.store.PutPermission(ctx, jobID, p); err != nil {
		return store.PermissionRequest{}, err
	}
	return p, nil
}

// statusPayload is the JSON body of a status_change event.
type statusPayload struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// appendStatusEvent records a status transition on the job timeline.
// Timeline writes are best-effort: the job record is authoritative, so
// a failed append is logged rather than surfaced.
func (s *Service) appendStatusEvent(ctx context.Context, jobID string, status job.Status, errText string) {
	payload, err := json.Marshal(statusPayload{Status: string(status), Error: errText})
	if err != nil {
		s.log.Error("encode status event", "job", jobID, "err", err)
		return
	}
	_, err = s.store.AppendEvent(ctx, jobID, store.Event{
		Type:        EventTypeStatusChange,
		PayloadJSON: string(payload),
		At:          time.Now().UTC(),
	})
	if err != nil {
		s.log.Error("append status event", "job", jobID, "status", status, "err", err)
	}
}
