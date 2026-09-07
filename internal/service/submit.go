package service

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/telemetry"
)

// SubmitParams is one task submission. Profile and RunConfigJSON are
// mutually exclusive sources for the RunConfig; an empty Profile with
// no RunConfigJSON selects the configured default profile.
type SubmitParams struct {
	Prompt        string
	Profile       string
	RunConfigJSON string
}

// Submit resolves, validates, and persists a job, then launches its
// harness in the background. It returns once the job is durable: launch
// failures surface as a transition to failed, not as an error here.
func (s *Service) Submit(ctx context.Context, p SubmitParams) (*job.Job, error) {
	ctx, span := s.tel.Start(ctx, "hairpin.submit")
	defer span.End()

	// A profile name is recorded only once it has resolved: an unknown
	// name is whatever the caller typed.
	fail := func(profile, outcome string, err error) (*job.Job, error) {
		s.tel.JobSubmitted(ctx, profile, outcome)
		telemetry.Fail(span, err)
		return nil, err
	}

	cfg, profile, err := s.resolveConfig(p)
	if err != nil {
		return fail("", telemetry.SubmissionRejected, err)
	}
	if profile != "" {
		span.SetAttributes(telemetry.ProfileAttr(profile))
	}

	id := job.NewID()
	cfg.RunId = id
	if p.Prompt != "" {
		cfg.Prompt = p.Prompt
	}
	applyExecutorDefaults(cfg, s.executorDefaults)
	if err := validateRunConfig(cfg); err != nil {
		return fail(profile, telemetry.SubmissionRejected, err)
	}
	if err := validateControlPlaneTools(cfg.GetTools().GetControlPlane(), s.memoryTools); err != nil {
		return fail(profile, telemetry.SubmissionRejected, err)
	}

	runConfigJSON, err := protojson.Marshal(cfg)
	if err != nil {
		return fail(profile, telemetry.SubmissionFailed, fmt.Errorf("encode run config: %w", err))
	}

	j := &job.Job{
		ID:            id,
		Status:        job.StatusQueued,
		Prompt:        cfg.GetPrompt(),
		Profile:       profile,
		RunConfigJSON: string(runConfigJSON),
		CreatedAt:     time.Now().UTC(),
		HarnessToken:  job.NewHarnessToken(),
		TraceParent:   telemetry.TraceParent(ctx),
	}
	if err := s.store.CreateJob(ctx, j); err != nil {
		return fail(profile, telemetry.SubmissionFailed, err)
	}
	telemetry.SetJobID(span, id)
	s.tel.JobSubmitted(ctx, profile, telemetry.SubmissionAccepted)
	s.appendStatusEvent(ctx, id, job.StatusQueued, "")

	s.launches.Add(1)
	go s.launch(j)

	return j, nil
}

// resolveConfig picks the RunConfig source and returns a mutable copy
// plus the profile name to record (empty for an explicit config).
func (s *Service) resolveConfig(p SubmitParams) (*harnessv1.RunConfig, string, error) {
	if p.RunConfigJSON != "" {
		if p.Profile != "" {
			return nil, "", fmt.Errorf("profile and run_config_json are mutually exclusive: %w", ErrInvalidArgument)
		}
		var cfg harnessv1.RunConfig
		if err := protojson.Unmarshal([]byte(p.RunConfigJSON), &cfg); err != nil {
			return nil, "", fmt.Errorf("parse run_config_json: %v: %w", err, ErrInvalidArgument)
		}
		return &cfg, "", nil
	}

	name := p.Profile
	if name == "" {
		name = s.profiles.Default()
	}
	if name == "" {
		return nil, "", fmt.Errorf("no profile given and no default profile configured; supply run_config_json: %w", ErrInvalidArgument)
	}
	cfg, ok := s.profiles.Get(name)
	if !ok {
		return nil, "", fmt.Errorf("unknown profile %q (have %v): %w", name, s.profiles.Names(), ErrInvalidArgument)
	}
	return cfg, name, nil
}

// validateRunConfig performs Hairpin's preflight checks for required
// wire fields and Kubernetes executor coordinates. Stirrup remains the
// authority for the full RunConfig schema and cross-field validation.
func validateRunConfig(cfg *harnessv1.RunConfig) error {
	if cfg.GetPrompt() == "" {
		return fmt.Errorf("prompt is required: %w", ErrInvalidArgument)
	}
	if cfg.GetMode() == "" {
		return fmt.Errorf("run config mode is required (execution, planning, review, research, toil): %w", ErrInvalidArgument)
	}
	if cfg.GetProvider().GetType() == "" && len(cfg.GetProviders()) == 0 {
		return fmt.Errorf("run config needs provider.type or a providers map: %w", ErrInvalidArgument)
	}
	if n := cfg.GetMaxTurns(); n < 1 || n > 100 {
		return fmt.Errorf("run config max_turns must be 1-100, got %d: %w", n, ErrInvalidArgument)
	}
	if cfg.Timeout == nil {
		return fmt.Errorf("run config timeout is required: %w", ErrInvalidArgument)
	}
	if t := cfg.GetTimeout(); t < 1 || t > 3600 {
		return fmt.Errorf("run config timeout must be 1-3600 seconds, got %d: %w", t, ErrInvalidArgument)
	}
	// An omitted executor silently defaults to "local" harness-side —
	// the agent's shell commands would run directly in the harness pod.
	// Require the operator to make that choice explicitly (in the
	// profile or the submitted config); stirrup validates the value.
	if cfg.GetExecutor().GetType() == "" {
		return fmt.Errorf("run config executor.type is required (local, container, k8s, k8s-sandbox, api, none): %w", ErrInvalidArgument)
	}
	return validateExecutor(cfg.GetExecutor())
}

// launch drives the queued → launching → awaiting_harness transitions
// around the Launcher call, detaching from the submitting request so a
// caller disconnect cannot orphan a harness.
func (s *Service) launch(j *job.Job) {
	defer s.launches.Done()

	ctx, cancel := context.WithTimeout(context.Background(), s.launchTimeout)
	defer cancel()

	// The launch outlives the submitting request, so it traces on its
	// own and links back to the submission rather than extending it.
	ctx, span := s.tel.Start(ctx, "hairpin.launch", telemetry.LinkToTraceParent(j.TraceParent))
	defer span.End()
	telemetry.SetJobID(span, j.ID)

	if !s.transition(ctx, j.ID, job.StatusQueued, job.StatusLaunching, "") {
		s.tel.JobLaunched(ctx, telemetry.LaunchSkipped, 0)
		return
	}

	started := time.Now()
	if err := s.launcher.Launch(ctx, j); err != nil {
		s.tel.JobLaunched(ctx, telemetry.LaunchFailed, time.Since(started))
		telemetry.Fail(span, err)
		s.log.Error("launch harness", "job", j.ID, "err", err)
		s.transition(ctx, j.ID, job.StatusLaunching, job.StatusFailed, err.Error())
		return
	}
	s.tel.JobLaunched(ctx, telemetry.LaunchSucceeded, time.Since(started))
	s.transition(ctx, j.ID, job.StatusLaunching, job.StatusAwaitingHarness, "")
}

// transition moves a job from one status to the next, recording a
// status_change event. It reports whether the move happened: a job that
// has already advanced (a harness that dialled in early, a cancellation
// that landed first) is left alone.
func (s *Service) transition(ctx context.Context, id string, from, to job.Status, errText string) bool {
	var moved bool
	_, err := s.store.UpdateJob(ctx, id, func(j *job.Job) error {
		if j.Status != from {
			return nil
		}
		j.Status = to
		if errText != "" {
			j.Error = errText
		}
		if to.Terminal() {
			j.FinishedAt = time.Now().UTC()
		}
		moved = true
		return nil
	})
	if err != nil {
		s.log.Error("job status transition", "job", id, "from", from, "to", to, "err", err)
		return false
	}
	if moved {
		s.appendStatusEvent(ctx, id, to, errText)
		if to.Terminal() {
			// A job that never ran has no run duration to report.
			s.tel.JobCompleted(ctx, to, "", 0)
		}
	}
	return moved
}
