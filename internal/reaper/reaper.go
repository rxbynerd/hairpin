// Package reaper runs hairpin's periodic store maintenance: failing
// jobs stuck in StatusAwaitingHarness past their deadline, and, when a
// retention window is configured, deleting terminal jobs past it. Both
// sweeps run from a single ticking loop; the awaiting_harness sweep
// runs unconditionally, since a harness that never dials back would
// otherwise wedge its job forever regardless of retention settings.
package reaper

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
	"github.com/rxbynerd/hairpin/internal/telemetry"
)

// eventStatusChange is the synthetic timeline event type appended when
// the reaper moves a job to a terminal status, matching the constant
// internal/service and internal/controlplane each define for the same
// purpose.
const eventStatusChange = "status_change"

// stuckHarnessError is recorded on a job the reaper fails because its
// harness never dialled back.
const stuckHarnessError = "harness never connected before deadline"

// Tuning defaults.
const (
	defaultInterval = 5 * time.Minute
	defaultPageSize = 200

	// awaitingHarnessMargin pads a job's computed deadline to absorb
	// scheduling jitter around the reaper's own tick interval.
	awaitingHarnessMargin = 5 * time.Minute
	// awaitingHarnessFallback stands in for a job's RunConfig timeout
	// when RunConfigJSON cannot be parsed or carries no timeout.
	awaitingHarnessFallback = time.Hour
)

// Reaper periodically sweeps the store for jobs that need maintenance.
type Reaper struct {
	store store.Store
	log   *slog.Logger
	tel   *telemetry.Recorder

	// retention is how long a terminal job is kept after FinishedAt
	// before it is deleted. <=0 disables deletion; the
	// awaiting_harness sweep still runs.
	retention time.Duration
	// deadlineSlack matches config.HarnessConfig.ActiveDeadlineSlack:
	// how long past a job's RunConfig timeout its harness Job is given
	// before Kubernetes kills it.
	deadlineSlack time.Duration

	interval time.Duration
	pageSize int
	now      func() time.Time
}

// Option configures a Reaper.
type Option func(*Reaper)

// WithInterval overrides the default sweep interval (5m).
func WithInterval(d time.Duration) Option {
	return func(r *Reaper) {
		if d > 0 {
			r.interval = d
		}
	}
}

// WithPageSize overrides the default ListJobs page size (200) used
// while sweeping.
func WithPageSize(n int) Option {
	return func(r *Reaper) {
		if n > 0 {
			r.pageSize = n
		}
	}
}

// WithTelemetry records the terminal transitions the awaiting_harness
// sweep makes, so a reaper-settled job is counted like any other
// completion. A nil recorder records nothing.
func WithTelemetry(rec *telemetry.Recorder) Option {
	return func(r *Reaper) { r.tel = rec }
}

// New returns a Reaper backed by st. retention <=0 disables
// terminal-job deletion; the awaiting_harness sweep always runs.
func New(st store.Store, retention, deadlineSlack time.Duration, logger *slog.Logger, opts ...Option) *Reaper {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	r := &Reaper{
		store:         st,
		log:           logger,
		retention:     retention,
		deadlineSlack: deadlineSlack,
		interval:      defaultInterval,
		pageSize:      defaultPageSize,
		now:           time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run sweeps immediately, then again on every tick, until ctx is done.
func (r *Reaper) Run(ctx context.Context) {
	r.sweep(ctx)
	t := time.NewTimer(jitter(r.interval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
			t.Reset(jitter(r.interval))
		}
	}
}

// jitter returns d plus up to 10% extra, spreading sweeps that started
// together (e.g. after a rolling restart) across time.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(int64(d)/10+1))
}

// sweep lists every job once and runs both maintenance passes over the
// same snapshot: acting on a page while still paging through ListJobs
// would shift a cursor built from live index positions.
func (r *Reaper) sweep(ctx context.Context) {
	jobs, err := r.listAllJobs(ctx)
	if err != nil {
		r.log.Error("reaper: list jobs", "err", err)
		return
	}
	r.reapAwaitingHarness(ctx, jobs)
	if r.retention > 0 {
		r.reapRetention(ctx, jobs)
	}
}

func (r *Reaper) listAllJobs(ctx context.Context) ([]*job.Job, error) {
	var all []*job.Job
	token := ""
	for {
		jobs, next, err := r.store.ListJobs(ctx, r.pageSize, token)
		if err != nil {
			return nil, err
		}
		all = append(all, jobs...)
		if next == "" {
			return all, nil
		}
		token = next
	}
}

// reapAwaitingHarness fails jobs whose harness never dialled back
// within their deadline.
func (r *Reaper) reapAwaitingHarness(ctx context.Context, jobs []*job.Job) {
	now := r.now()
	var failed int
	for _, j := range jobs {
		if j.Status != job.StatusAwaitingHarness {
			continue
		}
		if now.Sub(j.CreatedAt) <= awaitingHarnessDeadline(j.RunConfigJSON, r.deadlineSlack) {
			continue
		}
		moved, err := r.failStuckHarness(ctx, j.ID)
		if err != nil {
			r.log.Error("fail stuck awaiting_harness job", "job", j.ID, "err", err)
			continue
		}
		if moved {
			failed++
		}
	}
	if failed > 0 {
		r.log.Info("reaped jobs stuck in awaiting_harness", "count", failed)
	}
}

// awaitingHarnessDeadline is how long a job may sit in
// StatusAwaitingHarness before its harness is presumed lost.
func awaitingHarnessDeadline(runConfigJSON string, slack time.Duration) time.Duration {
	var cfg harnessv1.RunConfig
	if err := protojson.Unmarshal([]byte(runConfigJSON), &cfg); err != nil || cfg.Timeout == nil {
		return awaitingHarnessFallback + slack
	}
	return time.Duration(cfg.GetTimeout())*time.Second + slack + awaitingHarnessMargin
}

// failStuckHarness atomically moves id from awaiting_harness to failed.
// The status check inside UpdateJob's fn means a harness that connects
// concurrently, moving the job to running first, wins: moved reports
// false and nothing changes.
func (r *Reaper) failStuckHarness(ctx context.Context, id string) (moved bool, err error) {
	now := r.now()
	_, err = r.store.UpdateJob(ctx, id, func(j *job.Job) error {
		if j.Status != job.StatusAwaitingHarness {
			return nil
		}
		j.Status = job.StatusFailed
		j.Error = stuckHarnessError
		j.FinishedAt = now
		moved = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if moved {
		r.appendStatusEvent(ctx, id, job.StatusFailed, stuckHarnessError)
		// The job never left awaiting_harness, so it has no
		// assignment-to-terminal span to record: run duration is zero,
		// as it is for a launch failure.
		r.tel.JobCompleted(ctx, job.StatusFailed, telemetry.StopHarnessNeverConnected, 0)
	}
	return moved, nil
}

// reapRetention deletes terminal jobs whose FinishedAt is older than
// the configured retention window.
func (r *Reaper) reapRetention(ctx context.Context, jobs []*job.Job) {
	now := r.now()
	var deleted int
	for _, j := range jobs {
		if !j.Status.Terminal() || j.FinishedAt.IsZero() {
			continue
		}
		if now.Sub(j.FinishedAt) <= r.retention {
			continue
		}
		if err := r.store.DeleteJob(ctx, j.ID); err != nil {
			r.log.Error("delete job past retention window", "job", j.ID, "err", err)
			continue
		}
		deleted++
	}
	if deleted > 0 {
		r.log.Info("deleted jobs past retention window", "count", deleted, "retention", r.retention)
	}
}

// statusPayload is the JSON body of a status_change event, matching
// internal/service's shape.
type statusPayload struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (r *Reaper) appendStatusEvent(ctx context.Context, jobID string, status job.Status, errText string) {
	payload, err := json.Marshal(statusPayload{Status: string(status), Error: errText})
	if err != nil {
		r.log.Error("encode status event", "job", jobID, "err", err)
		return
	}
	if _, err := r.store.AppendEvent(ctx, jobID, store.Event{
		Type:        eventStatusChange,
		PayloadJSON: string(payload),
		At:          r.now(),
	}); err != nil {
		r.log.Error("append status event", "job", jobID, "status", status, "err", err)
	}
}
