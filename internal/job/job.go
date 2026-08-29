// Package job defines hairpin's job model: the caller-visible record of
// one submitted task, tracked from submission through harness execution
// to a terminal outcome.
package job

import (
	"crypto/rand"
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

// Job is the persisted record of one submitted task. The ID doubles as
// the stirrup RunConfig run_id and the CONTROL_PLANE_SESSION_ID the
// launched harness echoes back for stream correlation.
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

// StatusForStopReason maps a done.stop_reason to the terminal status.
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
