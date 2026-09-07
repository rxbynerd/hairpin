package telemetry

import "go.opentelemetry.io/otel/attribute"

// Metric attribute keys. Every value placed on a metric attribute is
// drawn from a bounded set: harness- and caller-supplied strings (job
// IDs, request IDs, unknown event types) belong on spans, never on
// metric series.
const (
	attrProfile            = attribute.Key("hairpin.profile")
	attrSubmissionOutcome  = attribute.Key("hairpin.submission.outcome")
	attrLaunchOutcome      = attribute.Key("hairpin.launch.outcome")
	attrJobStatus          = attribute.Key("hairpin.job.status")
	attrStopReason         = attribute.Key("hairpin.job.stop_reason")
	attrSessionDisposition = attribute.Key("hairpin.session.disposition")
	attrEventType          = attribute.Key("hairpin.harness.event.type")
	attrPermissionDecision = attribute.Key("hairpin.permission.decision")
	attrDelivered          = attribute.Key("hairpin.permission.delivered")
	attrMemoryTool         = attribute.Key("hairpin.memory.tool")
	attrMemoryOutcome      = attribute.Key("hairpin.memory.outcome")
)

// Span attribute keys. Job and request identifiers are unbounded, so
// they correlate traces and appear nowhere in metrics.
const (
	attrJobID     = attribute.Key("hairpin.job.id")
	attrRequestID = attribute.Key("hairpin.request.id")
)

// ProfileAttr labels a span with the profile a submission resolved to.
// Profile names come from the operator's profiles directory, so they
// are also safe on metrics.
func ProfileAttr(profile string) attribute.KeyValue { return attrProfile.String(profile) }

// MemoryToolAttr labels a span with the memory tool being fulfilled.
// Tool names outside hairpin's own reach metrics as "other"; on a span
// the value is recorded as sent.
func MemoryToolAttr(tool string) attribute.KeyValue { return attrMemoryTool.String(tool) }

// Submission outcomes.
const (
	SubmissionAccepted = "accepted"
	// SubmissionRejected is a caller mistake: an unknown profile, a
	// RunConfig that fails preflight, a tool the deployment cannot
	// fulfil.
	SubmissionRejected = "rejected"
	// SubmissionFailed is a hairpin-side failure, such as an
	// unreachable store.
	SubmissionFailed = "failed"
)

// Launch outcomes.
const (
	LaunchSucceeded = "succeeded"
	LaunchFailed    = "failed"
	// LaunchSkipped covers a job that left the queue before its
	// launcher ran — cancelled, or already claimed by a harness.
	LaunchSkipped = "skipped"
)

// Harness session dispositions, recorded once per inbound RunTask
// stream.
const (
	SessionAssigned          = "assigned"
	SessionClosedBeforeReady = "closed_before_ready"
	SessionNotReady          = "not_ready"
	SessionNoID              = "no_session_id"
	SessionUnknownJob        = "unknown_job"
	SessionBadToken          = "bad_token"
	SessionClosedJob         = "closed_job"
	SessionDuplicate         = "duplicate"
	SessionUnusableRunConfig = "unusable_run_config"
	SessionAssignmentFailed  = "assignment_failed"
)

// Memory call outcomes.
const (
	MemoryOK = "ok"
	// MemoryError is a call that reached the fulfilment path and came
	// back as an error result.
	MemoryError = "error"
	// MemoryRefused is a call hairpin declined before making it:
	// undeclared, unknown, duplicated, or over a limit.
	MemoryRefused = "refused"
)

// otherLabel replaces any value outside a known set, keeping a metric's
// cardinality bounded no matter what the harness sends.
const otherLabel = "other"

// knownStopReasons is stirrup's run-outcome vocabulary as carried by
// done.stop_reason. Anything else is counted as "other" rather than
// opening a metric series per novel value.
var knownStopReasons = map[string]struct{}{
	"success":     {},
	"error":       {},
	"cancelled":   {},
	"timeout":     {},
	"max_turns":   {},
	"max_tokens":  {},
	"end_turn":    {},
	"tool_use":    {},
	"aborted":     {},
	"interrupted": {},
	"refusal":     {},
}

// knownEventTypes is the HarnessEvent.type vocabulary hairpin serves.
// A type stirrup adds later is counted as "other" until it is listed.
var knownEventTypes = map[string]struct{}{
	"ready":                  {},
	"text_delta":             {},
	"heartbeat":              {},
	"permission_request":     {},
	"error":                  {},
	"warning":                {},
	"done":                   {},
	"sandbox_token_request":  {},
	"batch_submission":       {},
	"tool_result_request":    {},
	"tool_result_response":   {},
	"sandbox_token_response": {},
}

// knownMemoryTools mirrors internal/memory's control-plane tool names.
var knownMemoryTools = map[string]struct{}{
	"search_memory": {},
	"save_memory":   {},
}

func bounded(value string, known map[string]struct{}) string {
	if value == "" {
		return ""
	}
	if _, ok := known[value]; ok {
		return value
	}
	return otherLabel
}
