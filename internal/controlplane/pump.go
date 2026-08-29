package controlplane

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/memory"
	"github.com/rxbynerd/hairpin/internal/store"
	"github.com/rxbynerd/hairpin/internal/telemetry"
)

// pump drains one assigned harness stream into the store. All state is
// session-local so the hot path (text_delta) never touches the store
// per event.
type eventPump struct {
	h     *Handler
	ctx   context.Context
	jobID string
	sess  *session

	// startedAt is when the task assignment landed, the origin of the
	// run duration reported at a terminal event.
	startedAt time.Time

	finalText  strings.Builder
	errMessage string

	deltas     strings.Builder
	deltasFrom time.Time

	lastEventAt    time.Time
	lastEventFlush time.Time

	permissionsSeen int

	// Memory admission state, all owned by the pump goroutine. The
	// semaphore is the one piece the fulfilment goroutines touch, and
	// only to release their slot.
	declaredTools  map[string]struct{}
	memoryInFlight chan struct{}
	memoryRequests map[string]struct{}
	memoryCalls    int
}

func (h *Handler) pump(ctx context.Context, jobID string, sess *session, declaredTools map[string]struct{}) *eventPump {
	now := h.now()
	return &eventPump{
		h: h,
		// Detached from stream cancellation: the harness closes its
		// stream the instant it has sent done, and a terminal write (or
		// any recorded event) racing that teardown must still land.
		ctx:            context.WithoutCancel(ctx),
		jobID:          jobID,
		sess:           sess,
		startedAt:      now,
		lastEventAt:    now,
		lastEventFlush: now,
		declaredTools:  declaredTools,
		memoryInFlight: make(chan struct{}, h.maxInFlightMemory),
		memoryRequests: make(map[string]struct{}),
	}
}

func (p *eventPump) run() error {
	for {
		ev, err := p.sess.s.Receive()
		if err != nil {
			p.flushDeltas()
			// The stream is gone but the record must still reach a
			// terminal state (p.ctx is already detached from stream and
			// shutdown cancellation).
			p.closeUnfinished(p.ctx)
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
	p.h.tel.HarnessEvent(p.ctx, ev.GetType())

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

	case evWarning:
		p.h.log.Info("harness warning", "job_id", p.jobID, "message", ev.GetMessage())
		p.appendProto(ev, now)

	case evSandboxTokenRequest:
		p.appendProto(ev, now)
		p.handleSandboxTokenRequest(ev)

	case evToolResultRequest:
		p.appendProto(ev, now)
		p.fulfilToolResult(ev)

	case evBatchSubmission:
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
	p.h.tel.PermissionRequested(p.ctx)
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

// handleSandboxTokenRequest answers a sandbox_token_request: a signed
// token when an issuer is configured, otherwise the explicit refusal
// that lets an opted-in run config fail fast rather than wait out the
// harness's 60s timeout.
func (p *eventPump) handleSandboxTokenRequest(ev *harnessv1.HarnessEvent) {
	if p.h.issuer == nil {
		p.sendSandboxTokenResponse(ev, "", time.Time{}, true, sandboxTokenRefusal)
		return
	}

	// The harness's requested audience is informational only (proto
	// contract): the configured audience always wins. A mismatch is
	// logged, not honoured, since minting for an unconfigured audience
	// would hand out a token no verifier trusts.
	if aud := ev.GetAudience(); aud != "" && aud != p.h.issuer.Audience() {
		p.h.log.Info("harness requested a sandbox token audience that differs from the configured one; using the configured audience",
			"job_id", p.jobID, "requested_audience", aud, "configured_audience", p.h.issuer.Audience())
	}

	j, err := p.h.store.GetJob(p.ctx, p.jobID)
	if err != nil {
		p.h.log.Error("failed to load job for sandbox token issuance",
			"job_id", p.jobID, "request_id", ev.GetRequestId(), "error", err)
		p.sendSandboxTokenResponse(ev, "", time.Time{}, true, sandboxTokenIssuanceFailure)
		return
	}

	token, expiresAt, err := p.h.issuer.Mint(p.jobID, j.RepoScope)
	if err != nil {
		p.h.log.Error("failed to mint sandbox identity token",
			"job_id", p.jobID, "request_id", ev.GetRequestId(), "error", err)
		p.sendSandboxTokenResponse(ev, "", time.Time{}, true, sandboxTokenIssuanceFailure)
		return
	}
	p.sendSandboxTokenResponse(ev, token, expiresAt, false, "")
}

// sendSandboxTokenResponse sends one sandbox_token_response. token is
// SENSITIVE: it is handed to Send verbatim and never logged, appended
// to the timeline, or otherwise persisted.
func (p *eventPump) sendSandboxTokenResponse(ev *harnessv1.HarnessEvent, token string, expiresAt time.Time, isError bool, reason string) {
	resp := &harnessv1.ControlEvent{
		Type:      ctlSandboxTokenResponse,
		RequestId: ev.GetRequestId(),
		Token:     token,
		IsError:   &harnessv1.OptionalBool{Value: isError},
		Reason:    reason,
	}
	if !isError && !expiresAt.IsZero() {
		resp.ExpiresAt = proto.Int64(expiresAt.Unix())
	}
	if err := p.sess.Send(resp); err != nil {
		p.h.log.Error("failed to send sandbox token response",
			"job_id", p.jobID, "request_id", ev.GetRequestId(), "error", err)
	}
}

// fulfilToolResult answers a control-plane tool call. An admitted memory
// tool is proxied to Billet on its own goroutine so a slow backend
// cannot stall the event pump: the harness keeps streaming deltas and
// heartbeats while the call is in flight, and the response is sent
// whenever it arrives. Every other request is refused inline.
func (p *eventPump) fulfilToolResult(ev *harnessv1.HarnessEvent) {
	tool := ev.GetToolName()
	requestID := ev.GetRequestId()

	// A correlation id this long cannot be echoed back safely, and the
	// harness has no use for an answer it cannot match.
	if len(requestID) > maxRequestIDBytes {
		p.h.log.Warn("ignoring tool_result_request with an oversized request id",
			"job_id", p.jobID, "tool", tool, "request_id_bytes", len(requestID))
		return
	}
	if refusal, ok := p.admitMemoryCall(tool, requestID); !ok {
		p.h.tel.MemoryCall(p.ctx, tool, telemetry.MemoryRefused, 0)
		p.sendToolResult(requestID, refusal, true)
		return
	}

	input := append([]byte(nil), ev.GetInput()...)
	p.h.memoryWait.Add(1)
	go func() {
		defer p.h.memoryWait.Done()
		defer func() { <-p.memoryInFlight }()
		// This goroutine is detached from the request handler, so an
		// unrecovered panic here would take the process down and leave
		// every live run without a terminal record.
		defer func() {
			if r := recover(); r != nil {
				p.h.log.Error("memory tool call panicked",
					"job_id", p.jobID, "tool", tool, "request_id", requestID, "panic", r)
				p.sendToolResult(requestID, memory.GenericFailureMessage, true)
			}
		}()

		ctx, span := p.h.tel.Start(p.ctx, "hairpin.memory.call")
		defer span.End()
		span.SetAttributes(telemetry.MemoryToolAttr(tool))
		telemetry.SetJobID(span, p.jobID)
		telemetry.SetRequestID(span, requestID)

		ctx, cancel := context.WithTimeout(ctx, memory.CallTimeout)
		defer cancel()
		started := p.h.now()
		content, isError, detail := memory.Fulfil(ctx, p.h.memory, tool, input)
		outcome := telemetry.MemoryOK
		if isError {
			outcome = telemetry.MemoryError
		}
		p.h.tel.MemoryCall(ctx, tool, outcome, p.h.now().Sub(started))
		if detail != nil {
			telemetry.Fail(span, detail)
			p.h.log.Error("memory tool call failed",
				"job_id", p.jobID, "tool", tool, "request_id", requestID, "error", detail)
		}
		p.sendToolResult(requestID, content, isError)
	}()
}

// admitMemoryCall decides whether one request may reach Billet, taking
// an in-flight slot when it may. It reports the refusal to send back
// otherwise. Undeclared and unknown tools share a refusal so the answer
// carries no information about the run's configuration.
func (p *eventPump) admitMemoryCall(tool, requestID string) (string, bool) {
	if _, declared := p.declaredTools[tool]; !declared || !memory.IsMemoryTool(tool) {
		return memory.UnsupportedToolMessage(tool), false
	}
	if p.h.memory == nil {
		return memoryDisabledRefusal, false
	}
	if _, seen := p.memoryRequests[requestID]; seen {
		return duplicateRequestRefusal, false
	}
	if p.memoryCalls >= p.h.maxMemoryCalls {
		return memoryCallLimitRefusal, false
	}
	select {
	case p.memoryInFlight <- struct{}{}:
	default:
		return memoryBusyRefusal, false
	}
	p.memoryCalls++
	p.memoryRequests[requestID] = struct{}{}
	return "", true
}

// sendToolResult answers one tool_result_request and records the answer
// on the timeline. The record is written whether or not the send lands:
// an answer that resolves after the harness hung up is exactly what an
// operator needs to see, and for save_memory it is the only trace that
// a memory was written.
func (p *eventPump) sendToolResult(requestID, content string, isError bool) {
	resp := &harnessv1.ControlEvent{
		Type:      ctlToolResultResponse,
		RequestId: requestID,
		Content:   content,
		IsError:   &harnessv1.OptionalBool{Value: isError},
	}
	if err := p.sess.Send(resp); err != nil {
		p.h.log.Warn("failed to send tool result",
			"job_id", p.jobID, "request_id", requestID, "error", err)
	}
	p.appendControl(resp, p.h.now())
}

func (p *eventPump) appendControl(ev *harnessv1.ControlEvent, at time.Time) {
	payload, err := protojson.Marshal(ev)
	if err != nil {
		p.h.log.Error("failed to encode control event",
			"job_id", p.jobID, "type", ev.GetType(), "error", err)
		return
	}
	p.append(store.Event{Type: ev.GetType(), PayloadJSON: string(payload), At: at})
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
	p.h.tel.JobCompleted(p.ctx, status, stopReason, at.Sub(p.startedAt))
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
	p.h.tel.JobCompleted(ctx, status, stopReasonFor(status), at.Sub(p.startedAt))
}

// stopReasonFor labels a run that ended without a done event: a
// cancellation explains itself, anything else has no stop reason to
// report.
func stopReasonFor(status job.Status) string {
	if status == job.StatusCancelled {
		return "cancelled"
	}
	return ""
}

// isStreamEnd distinguishes an orderly stream end from a transport
// failure; both settle the job, only the second is worth an error log.
func isStreamEnd(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		connect.CodeOf(err) == connect.CodeCanceled
}
