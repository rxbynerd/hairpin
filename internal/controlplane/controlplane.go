// Package controlplane implements the stirrup.harness.v1.HarnessService
// side of hairpin: it correlates an inbound RunTask stream to a stored
// job via the harness's ready event, hands the job's RunConfig across as
// a task assignment, and pumps every harness event into the store until
// the run reaches a terminal state.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/gen/harness/v1/harnessv1connect"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/memory"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

// Harness event types (HarnessEvent.type).
const (
	evReady               = "ready"
	evTextDelta           = "text_delta"
	evHeartbeat           = "heartbeat"
	evPermissionRequest   = "permission_request"
	evError               = "error"
	evWarning             = "warning"
	evDone                = "done"
	evSandboxTokenRequest = "sandbox_token_request"
	evBatchSubmission     = "batch_submission"
	evToolResultRequest   = "tool_result_request"
)

// Control event types (ControlEvent.type).
const (
	ctlTaskAssignment       = "task_assignment"
	ctlCancel               = "cancel"
	ctlSandboxTokenResponse = "sandbox_token_response"
	ctlToolResultResponse   = "tool_result_response"
)

// EventStatusChange is the synthetic timeline event type hairpin appends
// whenever it moves a job between statuses.
const EventStatusChange = "status_change"

// msgStreamClosed is recorded when a harness stream ends without a done
// event: the workload died, was evicted, or lost its connection.
const msgStreamClosed = "harness stream closed without done"

// sandboxTokenRefusal is returned for sandbox_token_request so opted-in
// run configs abort immediately instead of waiting out the harness's
// 60s fail-closed timeout.
const sandboxTokenRefusal = "hairpin does not issue sandbox identity tokens"

// Refusals for a tool_result_request hairpin will not answer. Each is
// sent at once so the harness does not block for its per-call timeout.
const (
	memoryDisabledRefusal   = "hairpin has no memory backend configured"
	memoryBusyRefusal       = "hairpin is already running the maximum number of concurrent memory calls for this job"
	memoryCallLimitRefusal  = "this job has exceeded hairpin's memory-call limit"
	duplicateRequestRefusal = "hairpin has already accepted a memory call with this request id"
)

// Bounds on the memory calls one harness stream may drive. The request
// ids, tool names, and call rate are all harness-controlled, and every
// call holds a copy of its input and an HTTP/2 stream to a single Billet
// deployment shared by every job.
const (
	defaultMaxInFlightMemory = 4
	defaultMaxMemoryCalls    = 1000
	maxRequestIDBytes        = 128
)

// Tuning defaults for the event pump.
const (
	defaultLastEventFlush  = 10 * time.Second
	defaultDeltaFlushEvery = 500 * time.Millisecond
	defaultDeltaFlushBytes = 4 << 10
)

// stream is the subset of connect's bidi stream the control plane uses.
type stream interface {
	Receive() (*harnessv1.HarnessEvent, error)
	Send(*harnessv1.ControlEvent) error
}

// Handler implements harnessv1connect.HarnessServiceHandler.
type Handler struct {
	store  store.Store
	reg    *registry.Registry
	log    *slog.Logger
	memory memory.Client

	now               func() time.Time
	lastEventFlush    time.Duration
	deltaFlushEvery   time.Duration
	deltaFlushBytes   int
	maxFinalTextByte  int
	maxInFlightMemory int
	maxMemoryCalls    int

	memoryWait sync.WaitGroup
}

// Option configures a Handler.
type Option func(*Handler)

// WithLogger sets the logger used for stream lifecycle and protocol
// warnings.
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) {
		if l != nil {
			h.log = l
		}
	}
}

// WithMemory fulfils the memory tools against c. Without it, a run that
// calls one is refused.
func WithMemory(c memory.Client) Option {
	return func(h *Handler) {
		h.memory = c
	}
}

// New returns a control-plane handler backed by st, publishing live
// streams into reg so API handlers can route control events onto them.
func New(st store.Store, reg *registry.Registry, opts ...Option) *Handler {
	h := &Handler{
		store:             st,
		reg:               reg,
		log:               slog.Default(),
		now:               time.Now,
		lastEventFlush:    defaultLastEventFlush,
		deltaFlushEvery:   defaultDeltaFlushEvery,
		deltaFlushBytes:   defaultDeltaFlushBytes,
		maxFinalTextByte:  job.MaxFinalTextBytes,
		maxInFlightMemory: defaultMaxInFlightMemory,
		maxMemoryCalls:    defaultMaxMemoryCalls,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// WaitForMemoryCalls blocks until every in-flight memory call has
// answered. The calls are detached from their stream's context, so
// nothing else waits for them; shutdown does, to keep a late timeline
// write from racing the store's close.
func (h *Handler) WaitForMemoryCalls() { h.memoryWait.Wait() }

// MaxEventBytes caps a single received HarnessEvent message at 4 MiB,
// bounding memory used to decode a harness-controlled payload.
const MaxEventBytes = 4 << 20

// NewHTTPHandler returns the connect route path and handler for
// stirrup.harness.v1.HarnessService, ready to mount on hairpin's h2c mux.
func (h *Handler) NewHTTPHandler(opts ...connect.HandlerOption) (string, http.Handler) {
	opts = append([]connect.HandlerOption{connect.WithReadMaxBytes(MaxEventBytes)}, opts...)
	return harnessv1connect.NewHarnessServiceHandler(h, opts...)
}

// RunTask serves one harness connection for its whole lifetime.
func (h *Handler) RunTask(ctx context.Context, bs *connect.BidiStream[harnessv1.HarnessEvent, harnessv1.ControlEvent]) error {
	return h.runTask(ctx, connectStream{bs})
}

type connectStream struct {
	bs *connect.BidiStream[harnessv1.HarnessEvent, harnessv1.ControlEvent]
}

func (c connectStream) Receive() (*harnessv1.HarnessEvent, error) { return c.bs.Receive() }
func (c connectStream) Send(ev *harnessv1.ControlEvent) error     { return c.bs.Send(ev) }

// session is the registry-visible handle on a live stream. connect's
// bidi Send is not safe for concurrent use, so every writer — the event
// pump and any API goroutine delivering a decision — goes through this
// mutex.
type session struct {
	mu     sync.Mutex
	s      stream
	closed bool
}

func (s *session) Send(ev *harnessv1.ControlEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return registry.ErrNotConnected
	}
	return s.s.Send(ev)
}

func (s *session) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

func (h *Handler) runTask(ctx context.Context, s stream) error {
	first, err := s.Receive()
	if err != nil {
		h.log.Debug("harness stream closed before ready", "error", err)
		return nil
	}
	if first.GetType() != evReady {
		h.log.Warn("harness sent non-ready first event", "type", first.GetType())
		h.sendCancel(s, "")
		return nil
	}

	jobID, token := job.ParseSession(first.GetId())
	if jobID == "" {
		h.log.Warn("harness ready event carried no session id", "harness_version", first.GetHarnessVersion())
		h.sendCancel(s, "")
		return nil
	}

	j, err := h.store.GetJob(ctx, jobID)
	if err != nil {
		h.log.Warn("harness claimed unknown job", "job_id", jobID, "error", err)
		h.sendCancel(s, jobID)
		return nil
	}

	// The session string is the bearer credential: job IDs alone are
	// guessable (time-ordered ULIDs, visible in pod names and URLs).
	if !j.AcceptsToken(token) {
		h.log.Warn("harness presented wrong session token", "job_id", jobID)
		h.sendCancel(s, jobID)
		return nil
	}

	if j.Status.Terminal() || j.CancelRequested {
		h.log.Info("cancelling harness for closed job",
			"job_id", jobID, "status", string(j.Status), "cancel_requested", j.CancelRequested)
		h.sendCancel(s, jobID)
		if !j.Status.Terminal() {
			h.finaliseCancelled(ctx, jobID)
		}
		return nil
	}

	sess := &session{s: s}
	if err := h.reg.Register(jobID, sess); err != nil {
		// A second harness for the same job: the first stream owns the
		// run, so this one is dismissed without touching the record.
		h.log.Warn("rejecting duplicate harness session", "job_id", jobID, "error", err)
		h.sendCancel(s, jobID)
		return nil
	}
	defer func() {
		sess.close()
		h.reg.Unregister(jobID, sess)
	}()

	cfg, err := parseRunConfig(j.RunConfigJSON)
	if err != nil {
		h.log.Error("stored run config is unusable", "job_id", jobID, "error", err)
		h.sendCancel(s, jobID)
		h.failJob(ctx, jobID, fmt.Sprintf("stored run config is not a valid RunConfig: %v", err))
		return nil
	}

	if err := sess.Send(&harnessv1.ControlEvent{Type: ctlTaskAssignment, Task: cfg}); err != nil {
		h.log.Error("failed to send task assignment", "job_id", jobID, "error", err)
		h.failJob(ctx, jobID, fmt.Sprintf("failed to send task assignment: %v", err))
		return err
	}

	started := h.now()
	if _, err := h.store.UpdateJob(ctx, jobID, func(j *job.Job) error {
		j.Status = job.StatusRunning
		j.StartedAt = started
		j.LastEventAt = started
		return nil
	}); err != nil {
		h.log.Error("failed to mark job running", "job_id", jobID, "error", err)
	}
	h.appendStatus(ctx, jobID, job.StatusRunning, started)
	h.log.Info("harness assigned", "job_id", jobID, "harness_version", first.GetHarnessVersion())

	return h.pump(ctx, jobID, sess, declaredControlPlaneTools(cfg)).run()
}

// declaredControlPlaneTools is the set of control-plane tool names the
// job asked for. A tool the run never declared is refused even when
// hairpin could fulfil it, so a profile that omits the memory tools is
// genuinely opted out of them.
func declaredControlPlaneTools(cfg *harnessv1.RunConfig) map[string]struct{} {
	tools := cfg.GetTools().GetControlPlane()
	declared := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		declared[t.GetName()] = struct{}{}
	}
	return declared
}

// parseRunConfig decodes a stored protobuf-JSON RunConfig. It discards
// unknown fields so persisted records remain readable across compatible
// schema changes.
func parseRunConfig(runConfigJSON string) (*harnessv1.RunConfig, error) {
	if runConfigJSON == "" {
		return nil, errors.New("run config is empty")
	}
	var cfg harnessv1.RunConfig
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(runConfigJSON), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (h *Handler) sendCancel(s stream, jobID string) {
	if err := s.Send(&harnessv1.ControlEvent{Type: ctlCancel}); err != nil {
		h.log.Debug("failed to send cancel", "job_id", jobID, "error", err)
	}
}

// errNoUpdate aborts an UpdateJob transaction when the record already
// reflects the outcome.
var errNoUpdate = errors.New("no update required")

func (h *Handler) finaliseCancelled(ctx context.Context, jobID string) {
	at := h.now()
	if _, err := h.store.UpdateJob(ctx, jobID, func(j *job.Job) error {
		if j.Status.Terminal() {
			return errNoUpdate
		}
		j.Status = job.StatusCancelled
		if j.StopReason == "" {
			j.StopReason = "cancelled"
		}
		j.FinishedAt = at
		return nil
	}); err != nil {
		if errors.Is(err, errNoUpdate) {
			return
		}
		h.log.Error("failed to finalise cancelled job", "job_id", jobID, "error", err)
		return
	}
	h.appendStatus(ctx, jobID, job.StatusCancelled, at)
}

func (h *Handler) failJob(ctx context.Context, jobID, reason string) {
	at := h.now()
	if _, err := h.store.UpdateJob(ctx, jobID, func(j *job.Job) error {
		if j.Status.Terminal() {
			return errNoUpdate
		}
		j.Status = job.StatusFailed
		j.Error = reason
		j.FinishedAt = at
		return nil
	}); err != nil {
		if errors.Is(err, errNoUpdate) {
			return
		}
		h.log.Error("failed to mark job failed", "job_id", jobID, "error", err)
		return
	}
	h.appendStatus(ctx, jobID, job.StatusFailed, at)
}

func (h *Handler) appendStatus(ctx context.Context, jobID string, status job.Status, at time.Time) {
	ev := store.Event{
		Type:        EventStatusChange,
		PayloadJSON: fmt.Sprintf(`{"status":%q}`, string(status)),
		At:          at,
	}
	if _, err := h.store.AppendEvent(ctx, jobID, ev); err != nil {
		h.log.Error("failed to append status change", "job_id", jobID, "status", string(status), "error", err)
	}
}
