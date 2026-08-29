package controlplane

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

// pump drains one assigned harness stream into the store. All state is
// session-local so the hot path (text_delta) never touches the store
// per event.
type eventPump struct {
	h     *Handler
	ctx   context.Context
	jobID string
	sess  *session

	finalText  strings.Builder
	errMessage string

	deltas     strings.Builder
	deltasFrom time.Time

	lastEventAt    time.Time
	lastEventFlush time.Time

	permissionsSeen int
}

func (h *Handler) pump(ctx context.Context, jobID string, sess *session) *eventPump {
	now := h.now()
	return &eventPump{
		h:              h,
		ctx:            ctx,
		jobID:          jobID,
		sess:           sess,
		lastEventAt:    now,
		lastEventFlush: now,
	}
}

func (p *eventPump) run() error {
	for {
		ev, err := p.sess.s.Receive()
		if err != nil {
			p.flushDeltas()
			// The stream is gone but the record must still reach a
			// terminal state, even when the recv error is hairpin
			// shutting down.
			p.closeUnfinished(context.WithoutCancel(p.ctx))
			if isStreamEnd(err) {
				p.h.log.Info("harness stream ended", "job_id", p.jobID, "error", err)
				return nil
			}
			p.h.log.Error("harness stream failed", "job_id", p.jobID, "error", err)
			return nil
		}

		if done := p.handle(ev); done {
			return nil
		}
	}
}

// handle processes one harness event, reporting whether the run reached
// its terminal done event.
func (p *eventPump) handle(ev *harnessv1.HarnessEvent) bool {
	now := p.h.now()
	p.lastEventAt = now

	switch ev.GetType() {
	case evTextDelta:
		p.appendFinalText(ev.GetText())
		if p.deltas.Len() == 0 {
			p.deltasFrom = now
		}
		p.deltas.WriteString(ev.GetText())
		if p.deltas.Len() >= p.h.deltaFlushBytes || now.Sub(p.deltasFrom) >= p.h.deltaFlushEvery {
			p.flushDeltas()
		}
		p.maybeFlushLastEventAt(now)
		return false

	case evDone:
		p.flushDeltas()
		p.finish(ev, now)
		return true
	}

	// Every non-delta event closes the current delta run so the
	// timeline keeps harness ordering.
	p.flushDeltas()

	switch ev.GetType() {
	case evHeartbeat:
		// Type only: a heartbeat carries no fields worth persisting.
		p.append(store.Event{Type: evHeartbeat, At: now})

	case evPermissionRequest:
		p.putPermission(ev, now)
		p.appendProto(ev, now)

	case evError:
		p.errMessage = ev.GetMessage()
		p.h.log.Warn("harness reported error", "job_id", p.jobID, "message", ev.GetMessage())
		p.appendProto(ev, now)

	case evSandboxTokenRequest:
		p.appendProto(ev, now)
		p.refuseSandboxToken(ev)

	case evBatchSubmission, evToolResultRequest:
		p.h.log.Warn("unsupported harness request ignored",
			"job_id", p.jobID, "type", ev.GetType(), "request_id", ev.GetRequestId())
		p.appendProto(ev, now)

	case evReady:
		p.h.log.Warn("duplicate ready event ignored", "job_id", p.jobID)

	default:
		// Unknown types are recorded verbatim: stirrup adds events over
		// time and the timeline should not lose them.
		p.h.log.Info("unknown harness event type", "job_id", p.jobID, "type", ev.GetType())
		p.appendProto(ev, now)
	}

	p.maybeFlushLastEventAt(now)
	return false
}

func (p *eventPump) appendFinalText(text string) {
	room := p.h.maxFinalTextByte - p.finalText.Len()
	if room <= 0 {
		return
	}
	if len(text) > room {
		text = text[:room]
	}
	p.finalText.WriteString(text)
}

// flushDeltas records buffered text fragments as a single coalesced
// text_delta event.
func (p *eventPump) flushDeltas() {
	if p.deltas.Len() == 0 {
		return
	}
	merged := &harnessv1.HarnessEvent{Type: evTextDelta, Text: p.deltas.String()}
	p.deltas.Reset()
	p.appendProto(merged, p.h.now())
}

func (p *eventPump) appendProto(ev *harnessv1.HarnessEvent, at time.Time) {
	payload, err := protojson.Marshal(ev)
	if err != nil {
		p.h.log.Error("failed to encode harness event",
			"job_id", p.jobID, "type", ev.GetType(), "error", err)
		return
	}
	p.append(store.Event{Type: ev.GetType(), PayloadJSON: string(payload), At: at})
}

func (p *eventPump) append(ev store.Event) {
	if _, err := p.h.store.AppendEvent(p.ctx, p.jobID, ev); err != nil {
		p.h.log.Error("failed to append event", "job_id", p.jobID, "type", ev.Type, "error", err)
	}
}

// maxPermissionRequests bounds the per-job permission hash: the request
// IDs are harness-controlled, and unlike the event stream the hash has
// no MAXLEN. A legitimate run answers requests one tool call at a time
// and stays far below this.
const maxPermissionRequests = 1000

func (p *eventPump) putPermission(ev *harnessv1.HarnessEvent, at time.Time) {
	if p.permissionsSeen >= maxPermissionRequests {
		p.h.log.Warn("permission request cap reached; not persisting",
			"job_id", p.jobID, "request_id", ev.GetRequestId())
		return
	}
	p.permissionsSeen++
	req := store.PermissionRequest{
		RequestID:   ev.GetRequestId(),
		ToolName:    ev.GetToolName(),
		InputJSON:   string(ev.GetInput()),
		State:       store.PermissionPending,
		RequestedAt: at,
	}
	if err := p.h.store.PutPermission(p.ctx, p.jobID, req); err != nil {
		p.h.log.Error("failed to persist permission request",
			"job_id", p.jobID, "request_id", req.RequestID, "error", err)
	}
}

func (p *eventPump) refuseSandboxToken(ev *harnessv1.HarnessEvent) {
	resp := &harnessv1.ControlEvent{
		Type:      ctlSandboxTokenResponse,
		RequestId: ev.GetRequestId(),
		IsError:   &harnessv1.OptionalBool{Value: true},
		Reason:    sandboxTokenRefusal,
	}
	if err := p.sess.Send(resp); err != nil {
		p.h.log.Error("failed to refuse sandbox token request",
			"job_id", p.jobID, "request_id", ev.GetRequestId(), "error", err)
	}
}

// maybeFlushLastEventAt persists the liveness timestamp at a bounded
// rate; every event would put a store round-trip on the text_delta path.
func (p *eventPump) maybeFlushLastEventAt(now time.Time) {
	if now.Sub(p.lastEventFlush) < p.h.lastEventFlush {
		return
	}
	p.lastEventFlush = now
	seen := p.lastEventAt
	if _, err := p.h.store.UpdateJob(p.ctx, p.jobID, func(j *job.Job) error {
		j.LastEventAt = seen
		return nil
	}); err != nil {
		p.h.log.Error("failed to record liveness", "job_id", p.jobID, "error", err)
	}
}

func (p *eventPump) finish(ev *harnessv1.HarnessEvent, at time.Time) {
	stopReason := ev.GetStopReason()
	status := job.StatusForStopReason(stopReason)
	final := p.finalText.String()
	errMsg := p.errMessage

	if _, err := p.h.store.UpdateJob(p.ctx, p.jobID, func(j *job.Job) error {
		if j.Status.Terminal() {
			return errNoUpdate
		}
		j.Status = status
		j.StopReason = stopReason
		if errMsg != "" {
			j.Error = errMsg
		}
		j.FinalText = final
		j.FinishedAt = at
		j.LastEventAt = at
		return nil
	}); err != nil {
		if errors.Is(err, errNoUpdate) {
			return
		}
		p.h.log.Error("failed to finalise job", "job_id", p.jobID, "error", err)
	}
	p.appendProto(ev, at)
	p.h.appendStatus(p.ctx, p.jobID, status, at)
	p.h.log.Info("harness run finished",
		"job_id", p.jobID, "status", string(status), "stop_reason", stopReason)
}

// closeUnfinished settles a job whose stream ended without a done event.
// A cancel already asked for over the API explains the closure; anything
// else is a crashed or evicted harness.
func (p *eventPump) closeUnfinished(ctx context.Context) {
	at := p.h.now()
	final := p.finalText.String()
	errMsg := p.errMessage
	var status job.Status

	if _, err := p.h.store.UpdateJob(ctx, p.jobID, func(j *job.Job) error {
		if j.Status.Terminal() {
			return errNoUpdate
		}
		if j.CancelRequested {
			status = job.StatusCancelled
			if j.StopReason == "" {
				j.StopReason = "cancelled"
			}
		} else {
			status = job.StatusFailed
			j.Error = msgStreamClosed
			if errMsg != "" {
				j.Error = msgStreamClosed + "; last error: " + errMsg
			}
		}
		j.Status = status
		j.FinalText = final
		j.FinishedAt = at
		j.LastEventAt = p.lastEventAt
		return nil
	}); err != nil {
		if !errors.Is(err, errNoUpdate) {
			p.h.log.Error("failed to settle unfinished job", "job_id", p.jobID, "error", err)
		}
		return
	}
	p.h.appendStatus(ctx, p.jobID, status, at)
}

// isStreamEnd distinguishes an orderly stream end from a transport
// failure; both settle the job, only the second is worth an error log.
func isStreamEnd(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		connect.CodeOf(err) == connect.CodeCanceled
}
