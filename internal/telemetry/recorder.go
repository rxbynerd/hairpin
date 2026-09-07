package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/rxbynerd/hairpin/internal/job"
)

// scopeName identifies hairpin's own instrumentation, distinguishing it
// from the RPC spans and metrics otelconnect records.
const scopeName = "github.com/rxbynerd/hairpin"

// Recorder records hairpin's spans and metrics.
//
// A nil *Recorder is a working no-op: every method accepts it, spans it
// starts are non-recording, and no measurement is taken. Instrumented
// packages therefore need no telemetry configured, and tests need none
// at all.
type Recorder struct {
	tracer trace.Tracer

	submissions    metric.Int64Counter
	launches       metric.Int64Counter
	launchDuration metric.Float64Histogram
	completions    metric.Int64Counter
	runDuration    metric.Float64Histogram
	sessions       metric.Int64Counter
	activeSessions metric.Int64UpDownCounter
	harnessEvents  metric.Int64Counter
	permissions    metric.Int64Counter
	decisions      metric.Int64Counter
	memoryCalls    metric.Int64Counter
	memoryDuration metric.Float64Histogram
}

// nonRecording serves spans for a nil Recorder.
var nonRecording = noop.NewTracerProvider().Tracer(scopeName)

// New builds a Recorder against the given providers. Passing the global
// providers is the usual choice; Setup does it for the configured
// pipeline.
func New(tp trace.TracerProvider, mp metric.MeterProvider) (*Recorder, error) {
	meter := mp.Meter(scopeName)
	r := &Recorder{tracer: tp.Tracer(scopeName)}

	var err error
	build := func(name string, fn func() error) {
		if err != nil {
			return
		}
		if e := fn(); e != nil {
			err = fmt.Errorf("create instrument %s: %w", name, e)
		}
	}

	build("hairpin.job.submissions", func() (e error) {
		r.submissions, e = meter.Int64Counter("hairpin.job.submissions",
			metric.WithDescription("Task submissions, by outcome and profile."),
			metric.WithUnit("{submission}"))
		return e
	})
	build("hairpin.job.launches", func() (e error) {
		r.launches, e = meter.Int64Counter("hairpin.job.launches",
			metric.WithDescription("Harness launches attempted, by outcome."),
			metric.WithUnit("{launch}"))
		return e
	})
	build("hairpin.job.launch.duration", func() (e error) {
		r.launchDuration, e = meter.Float64Histogram("hairpin.job.launch.duration",
			metric.WithDescription("Time spent handing a harness to its launcher."),
			metric.WithUnit("s"))
		return e
	})
	build("hairpin.job.completions", func() (e error) {
		r.completions, e = meter.Int64Counter("hairpin.job.completions",
			metric.WithDescription("Jobs reaching a terminal status, by status and stop reason."),
			metric.WithUnit("{job}"))
		return e
	})
	build("hairpin.job.run.duration", func() (e error) {
		r.runDuration, e = meter.Float64Histogram("hairpin.job.run.duration",
			metric.WithDescription("Time from task assignment to terminal status."),
			metric.WithUnit("s"))
		return e
	})
	build("hairpin.harness.sessions", func() (e error) {
		r.sessions, e = meter.Int64Counter("hairpin.harness.sessions",
			metric.WithDescription("Inbound harness streams, by disposition."),
			metric.WithUnit("{session}"))
		return e
	})
	build("hairpin.harness.sessions.active", func() (e error) {
		r.activeSessions, e = meter.Int64UpDownCounter("hairpin.harness.sessions.active",
			metric.WithDescription("Harness streams currently assigned a task."),
			metric.WithUnit("{session}"))
		return e
	})
	build("hairpin.harness.events", func() (e error) {
		r.harnessEvents, e = meter.Int64Counter("hairpin.harness.events",
			metric.WithDescription("Events received on harness streams, by type."),
			metric.WithUnit("{event}"))
		return e
	})
	build("hairpin.permission.requests", func() (e error) {
		r.permissions, e = meter.Int64Counter("hairpin.permission.requests",
			metric.WithDescription("Permission requests received from harnesses."),
			metric.WithUnit("{request}"))
		return e
	})
	build("hairpin.permission.decisions", func() (e error) {
		r.decisions, e = meter.Int64Counter("hairpin.permission.decisions",
			metric.WithDescription("Permission decisions answered, by decision and delivery."),
			metric.WithUnit("{decision}"))
		return e
	})
	build("hairpin.memory.calls", func() (e error) {
		r.memoryCalls, e = meter.Int64Counter("hairpin.memory.calls",
			metric.WithDescription("Control-plane memory tool calls, by tool and outcome."),
			metric.WithUnit("{call}"))
		return e
	})
	build("hairpin.memory.call.duration", func() (e error) {
		r.memoryDuration, e = meter.Float64Histogram("hairpin.memory.call.duration",
			metric.WithDescription("Time spent fulfilling one memory tool call."),
			metric.WithUnit("s"))
		return e
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Start begins a span. On a nil Recorder it returns ctx unchanged and a
// non-recording span.
func (r *Recorder) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if r == nil {
		return nonRecording.Start(ctx, name, opts...)
	}
	return r.tracer.Start(ctx, name, opts...)
}

// SetJobID records the job a span concerns. Job IDs are unbounded, so
// they belong on spans only.
func SetJobID(span trace.Span, jobID string) {
	if jobID == "" {
		return
	}
	span.SetAttributes(attrJobID.String(jobID))
}

// SetRequestID records the harness-supplied correlation id a span
// concerns. Like job IDs, request IDs are unbounded and belong on spans
// only.
func SetRequestID(span trace.Span, requestID string) {
	if requestID == "" {
		return
	}
	span.SetAttributes(attrRequestID.String(requestID))
}

// Fail marks a span as failed with err.
func Fail(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// JobSubmitted counts one submission. The profile is recorded only when
// it resolved to a configured profile: an unknown name is caller-
// supplied and would open a metric series per typo.
func (r *Recorder) JobSubmitted(ctx context.Context, profile, outcome string) {
	if r == nil {
		return
	}
	attrs := []attribute.KeyValue{attrSubmissionOutcome.String(outcome)}
	if profile != "" {
		attrs = append(attrs, attrProfile.String(profile))
	}
	r.submissions.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// JobLaunched counts one launcher call and how long it took.
func (r *Recorder) JobLaunched(ctx context.Context, outcome string, d time.Duration) {
	if r == nil {
		return
	}
	attrs := metric.WithAttributes(attrLaunchOutcome.String(outcome))
	r.launches.Add(ctx, 1, attrs)
	if d > 0 {
		r.launchDuration.Record(ctx, d.Seconds(), attrs)
	}
}

// JobCompleted counts one job reaching a terminal status. A positive
// runDuration also records the assignment-to-terminal latency; jobs
// that never ran (a launch failure, a cancellation before the harness
// dialled in) pass zero.
func (r *Recorder) JobCompleted(ctx context.Context, status job.Status, stopReason string, runDuration time.Duration) {
	if r == nil {
		return
	}
	attrs := metric.WithAttributes(
		attrJobStatus.String(string(status)),
		attrStopReason.String(bounded(stopReason, knownStopReasons)),
	)
	r.completions.Add(ctx, 1, attrs)
	if runDuration > 0 {
		r.runDuration.Record(ctx, runDuration.Seconds(),
			metric.WithAttributes(attrJobStatus.String(string(status))))
	}
}

// HarnessSession counts one inbound stream's disposition.
func (r *Recorder) HarnessSession(ctx context.Context, disposition string) {
	if r == nil {
		return
	}
	r.sessions.Add(ctx, 1, metric.WithAttributes(attrSessionDisposition.String(disposition)))
}

// SessionOpened and SessionClosed bracket an assigned stream, tracking
// how many harnesses are executing right now.
func (r *Recorder) SessionOpened(ctx context.Context) {
	if r == nil {
		return
	}
	r.activeSessions.Add(ctx, 1)
}

func (r *Recorder) SessionClosed(ctx context.Context) {
	if r == nil {
		return
	}
	r.activeSessions.Add(ctx, -1)
}

// HarnessEvent counts one received event. Types outside the protocol
// vocabulary are counted as "other".
func (r *Recorder) HarnessEvent(ctx context.Context, eventType string) {
	if r == nil {
		return
	}
	r.harnessEvents.Add(ctx, 1, metric.WithAttributes(
		attrEventType.String(bounded(eventType, knownEventTypes))))
}

// PermissionRequested counts one permission request from a harness.
func (r *Recorder) PermissionRequested(ctx context.Context) {
	if r == nil {
		return
	}
	r.permissions.Add(ctx, 1)
}

// PermissionAnswered counts one operator decision and whether it
// reached the harness.
func (r *Recorder) PermissionAnswered(ctx context.Context, allow, delivered bool) {
	if r == nil {
		return
	}
	decision := "deny"
	if allow {
		decision = "allow"
	}
	r.decisions.Add(ctx, 1, metric.WithAttributes(
		attrPermissionDecision.String(decision),
		attrDelivered.Bool(delivered),
	))
}

// MemoryCall counts one control-plane memory tool call. Tools outside
// hairpin's own are counted as "other"; a refused call passes a zero
// duration, having never reached the backend.
func (r *Recorder) MemoryCall(ctx context.Context, tool, outcome string, d time.Duration) {
	if r == nil {
		return
	}
	attrs := metric.WithAttributes(
		attrMemoryTool.String(bounded(tool, knownMemoryTools)),
		attrMemoryOutcome.String(outcome),
	)
	r.memoryCalls.Add(ctx, 1, attrs)
	if d > 0 {
		r.memoryDuration.Record(ctx, d.Seconds(), attrs)
	}
}

// traceContext carries hairpin's own correlation between a submission
// and the harness stream that later claims it. Only the W3C fields are
// used: the two ends are the same service, and nothing else is trusted
// across the gap.
var traceContext = propagation.TraceContext{}

// TraceParent encodes ctx's span for storage on a job record, or ""
// when no span is recording.
func TraceParent(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	traceContext.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// LinkToTraceParent returns a span-start option linking a new span to
// the trace a stored traceparent names, so a harness stream — a trace
// of its own, started by an inbound RPC — points back at the submission
// that created its job. An empty or malformed value links nothing.
func LinkToTraceParent(traceParent string) trace.SpanStartOption {
	if traceParent == "" {
		return trace.WithLinks()
	}
	ctx := traceContext.Extract(context.Background(), propagation.MapCarrier{"traceparent": traceParent})
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return trace.WithLinks()
	}
	return trace.WithLinks(trace.Link{SpanContext: sc})
}
