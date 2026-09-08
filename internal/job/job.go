// Package job defines hairpin's job model: the caller-visible record of
// one submitted task, tracked from submission through harness execution
// to a terminal outcome.
package job

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Status is the hairpin-side lifecycle state of a job.
type Status string

const (
	// StatusQueued: accepted and persisted; launcher not yet invoked.
	StatusQueued Status = "queued"
	// StatusLaunching: launcher invoked; harness being created.
	StatusLaunching Status = "launching"
	// StatusAwaitingHarness: harness launched but not yet dialled in.
	StatusAwaitingHarness Status = "awaiting_harness"
	// StatusRunning: task_assignment sent; the harness is executing.
	StatusRunning Status = "running"
	// StatusSucceeded: terminal; done.stop_reason was "success".
	StatusSucceeded Status = "succeeded"
	// StatusFailed: terminal; any non-success, non-cancelled outcome,
	// including launch errors and a stream closed without done.
	StatusFailed Status = "failed"
	// StatusCancelled: terminal; cancelled via the API before or during
	// execution.
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether s is a final state.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Job is the persisted record of one submitted task. ID is also the
// stirrup RunConfig run_id. Harnesses correlate with the authenticated
// session string returned by SessionString, not with the ID alone.
type Job struct {
	ID            string
	Status        Status
	Prompt        string
	Profile       string
	RunConfigJSON string
	// StopReason is stirrup's done.stop_reason verbatim; unknown values
	// are preserved so new stirrup outcomes pass through.
	StopReason string
	Error      string
	// FinalText accumulates text_delta events, capped at MaxFinalTextBytes.
	FinalText   string
	CreatedAt   time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	LastEventAt time.Time
	// CancelRequested is set when cancellation was asked for before the
	// harness connected, so the control plane cancels instead of
	// assigning.
	CancelRequested bool
	// TraceParent is the W3C trace context of the submission that
	// created this job, when tracing was enabled. The control plane
	// links a harness stream's span back to it; it is never trusted as
	// an inbound trace parent, and is empty for jobs submitted with
	// tracing off.
	TraceParent string
	// HarnessToken is the per-job bearer secret a harness must present
	// (inside CONTROL_PLANE_SESSION_ID, echoed back in ready.id) to
	// claim this job's stream. Never exposed on the Job API surface.
	HarnessToken string
	// RepoScope lists the "haybale.dev/repos" glob patterns minted into
	// this job's sandbox identity token, when the control plane issues
	// one. Empty means any issued token carries no repo grant.
	RepoScope []string
}

// MaxFinalTextBytes caps FinalText accumulation, mirroring stirrup's
// RunResult default.
const MaxFinalTextBytes = 128 * 1024

// NewID returns a fresh job ID: "hp-" plus a lowercase ULID. Lowercase
// alphanumerics satisfy stirrup's run_id constraints (no path
// separators, "..", or control bytes) and stay readable in URLs, pod
// names, and Redis keys.
func NewID() string {
	return "hp-" + strings.ToLower(ulid.MustNew(ulid.Now(), rand.Reader).String())
}

// idRe matches IDs produced by NewID. Store keys and pod names are
// built from IDs, so anything else is rejected at the API boundary.
var idRe = regexp.MustCompile(`^hp-[0-9a-z]{26}$`)

// ValidID reports whether id has the shape NewID produces.
func ValidID(id string) bool { return idRe.MatchString(id) }

// NewHarnessToken returns a fresh 128-bit hex bearer token for
// harness-session authentication. ULID job IDs are time-ordered and
// visible in pod names and URLs, so they authenticate nothing on their
// own.
func NewHarnessToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// SessionString encodes the authenticated value launchers place in
// CONTROL_PLANE_SESSION_ID: "<job id>.<token>". The bare-ID form exists
// only for legacy records that predate harness tokens.
func (j *Job) SessionString() string {
	if j.HarnessToken == "" {
		return j.ID
	}
	return j.ID + "." + j.HarnessToken
}

// ParseSession splits a ready.id session string into job ID and token.
// A bare ID yields an empty token.
func ParseSession(s string) (id, token string) {
	id, token, _ = strings.Cut(s, ".")
	return id, token
}

// AcceptsToken reports whether the presented token matches the job's,
// in constant time. A legacy job with no stored token accepts only an
// empty token.
func (j *Job) AcceptsToken(token string) bool {
	return subtle.ConstantTimeCompare([]byte(j.HarnessToken), []byte(token)) == 1
}

// StatusForStopReason maps a done.stop_reason to the terminal status.
// The value is stirrup's run outcome ("success", "error", "timeout",
// "max_turns", ...), not the narrower loop stop-reason enum.
func StatusForStopReason(stopReason string) Status {
	switch stopReason {
	case "success":
		return StatusSucceeded
	case "cancelled":
		return StatusCancelled
	default:
		return StatusFailed
	}
}
