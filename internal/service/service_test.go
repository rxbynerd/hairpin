package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

// fakeLauncher records the jobs it was asked to launch and can be made
// to fail.
type fakeLauncher struct {
	mu   sync.Mutex
	jobs []*job.Job
	err  error
}

func (f *fakeLauncher) Launch(_ context.Context, j *job.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, j)
	return f.err
}

func (f *fakeLauncher) launched() []*job.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*job.Job(nil), f.jobs...)
}

// fakeSession captures the control events routed to a harness.
type fakeSession struct {
	mu     sync.Mutex
	events []*harnessv1.ControlEvent
	err    error
}

func (f *fakeSession) Send(ev *harnessv1.ControlEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}

func (f *fakeSession) sent() []*harnessv1.ControlEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*harnessv1.ControlEvent(nil), f.events...)
}

func testTemplate() *harnessv1.RunConfig {
	return &harnessv1.RunConfig{
		Mode:     "planning",
		Provider: &harnessv1.ProviderConfig{Type: "anthropic"},
		MaxTurns: 20,
		Executor: &harnessv1.ExecutorConfig{Type: "local"},
		Timeout:  proto.Int32(600),
	}
}

type fixture struct {
	svc      *Service
	store    store.Store
	registry *registry.Registry
	launcher *fakeLauncher
}

func newFixture(t *testing.T, opts ...Option) *fixture {
	t.Helper()
	profiles, err := NewProfiles(map[string]*harnessv1.RunConfig{"default": testTemplate()}, "default")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	reg := registry.New()
	l := &fakeLauncher{}
	return &fixture{
		svc:      New(st, reg, l, profiles, slog.New(slog.DiscardHandler), opts...),
		store:    st,
		registry: reg,
		launcher: l,
	}
}

// submitted runs a submit and waits for the background launch to settle.
func (f *fixture) submitted(t *testing.T, p SubmitParams) *job.Job {
	t.Helper()
	j, err := f.svc.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	f.svc.WaitForLaunches()
	return j
}

func TestSubmitResolvesProfileAndLaunches(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "audit the config"})

	if !strings.HasPrefix(j.ID, "hp-") {
		t.Errorf("job ID %q lacks hp- prefix", j.ID)
	}
	if j.Status != job.StatusQueued {
		t.Errorf("returned status = %s, want queued", j.Status)
	}
	if j.Profile != "default" {
		t.Errorf("profile = %q, want default", j.Profile)
	}

	var cfg harnessv1.RunConfig
	if err := protojson.Unmarshal([]byte(j.RunConfigJSON), &cfg); err != nil {
		t.Fatalf("stored run config: %v", err)
	}
	if cfg.GetRunId() != j.ID {
		t.Errorf("run_id = %q, want %q", cfg.GetRunId(), j.ID)
	}
	if cfg.GetPrompt() != "audit the config" {
		t.Errorf("prompt = %q", cfg.GetPrompt())
	}
	if cfg.GetMode() != "planning" {
		t.Errorf("mode = %q, want planning from the profile", cfg.GetMode())
	}

	launched := f.launcher.launched()
	if len(launched) != 1 || launched[0].ID != j.ID {
		t.Fatalf("launcher got %v, want one launch of %s", launched, j.ID)
	}

	got, err := f.store.GetJob(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusAwaitingHarness {
		t.Errorf("status after launch = %s, want awaiting_harness", got.Status)
	}
	assertStatusEvents(t, f.store, j.ID, "queued", "launching", "awaiting_harness")
}

func TestSubmitProfileTemplateIsNotMutated(t *testing.T) {
	f := newFixture(t)
	f.submitted(t, SubmitParams{Prompt: "first"})
	f.submitted(t, SubmitParams{Prompt: "second"})

	cfg, ok := f.svc.Profiles().Get("default")
	if !ok {
		t.Fatal("default profile disappeared")
	}
	if cfg.GetRunId() != "" || cfg.GetPrompt() != "" {
		t.Errorf("template mutated: run_id=%q prompt=%q", cfg.GetRunId(), cfg.GetPrompt())
	}
}

func TestSubmitExplicitRunConfigJSON(t *testing.T) {
	f := newFixture(t)
	raw, err := protojson.Marshal(&harnessv1.RunConfig{
		Mode:     "execution",
		Prompt:   "from the config",
		Provider: &harnessv1.ProviderConfig{Type: "anthropic"},
		MaxTurns: 5,
		Executor: &harnessv1.ExecutorConfig{Type: "local"},
		Timeout:  proto.Int32(60),
		RunId:    "caller-supplied",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	j := f.submitted(t, SubmitParams{RunConfigJSON: string(raw)})
	if j.Profile != "" {
		t.Errorf("profile = %q, want empty for an explicit config", j.Profile)
	}
	if j.Prompt != "from the config" {
		t.Errorf("prompt = %q, want the config's own", j.Prompt)
	}

	var cfg harnessv1.RunConfig
	if err := protojson.Unmarshal([]byte(j.RunConfigJSON), &cfg); err != nil {
		t.Fatalf("stored run config: %v", err)
	}
	if cfg.GetRunId() != j.ID {
		t.Errorf("run_id = %q, want it forced to %q", cfg.GetRunId(), j.ID)
	}
	if cfg.GetMode() != "execution" {
		t.Errorf("mode = %q, want the config used verbatim", cfg.GetMode())
	}
}

func TestSubmitPromptOverridesRunConfigPrompt(t *testing.T) {
	f := newFixture(t)
	raw, err := protojson.Marshal(&harnessv1.RunConfig{
		Mode:     "planning",
		Prompt:   "config prompt",
		Provider: &harnessv1.ProviderConfig{Type: "anthropic"},
		MaxTurns: 5,
		Executor: &harnessv1.ExecutorConfig{Type: "local"},
		Timeout:  proto.Int32(60),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	j := f.submitted(t, SubmitParams{Prompt: "request prompt", RunConfigJSON: string(raw)})
	if j.Prompt != "request prompt" {
		t.Errorf("prompt = %q, want the request's", j.Prompt)
	}
}

func TestSubmitValidationFailures(t *testing.T) {
	valid := func(mutate func(*harnessv1.RunConfig)) string {
		cfg := testTemplate()
		cfg.Prompt = "do a thing"
		mutate(cfg)
		raw, err := protojson.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(raw)
	}

	cases := []struct {
		name   string
		params SubmitParams
		want   string
	}{
		{"no prompt", SubmitParams{}, "prompt is required"},
		{"unknown profile", SubmitParams{Prompt: "x", Profile: "nope"}, "unknown profile"},
		{
			"both sources",
			SubmitParams{Prompt: "x", Profile: "default", RunConfigJSON: valid(func(*harnessv1.RunConfig) {})},
			"mutually exclusive",
		},
		{"malformed config", SubmitParams{RunConfigJSON: "{"}, "parse run_config_json"},
		{
			"missing mode",
			SubmitParams{RunConfigJSON: valid(func(c *harnessv1.RunConfig) { c.Mode = "" })},
			"mode is required",
		},
		{
			"missing provider",
			SubmitParams{RunConfigJSON: valid(func(c *harnessv1.RunConfig) { c.Provider = nil })},
			"provider.type",
		},
		{
			"max turns out of range",
			SubmitParams{RunConfigJSON: valid(func(c *harnessv1.RunConfig) { c.MaxTurns = 0 })},
			"max_turns must be 1-100",
		},
		{
			"missing timeout",
			SubmitParams{RunConfigJSON: valid(func(c *harnessv1.RunConfig) { c.Timeout = nil })},
			"timeout is required",
		},
		{
			"timeout out of range",
			SubmitParams{RunConfigJSON: valid(func(c *harnessv1.RunConfig) { c.Timeout = proto.Int32(9999) })},
			"timeout must be 1-3600",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := f.svc.Submit(context.Background(), tc.params)
			if err == nil {
				t.Fatal("Submit succeeded, want an error")
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("error %v is not ErrInvalidArgument", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if jobs, _, _ := f.store.ListJobs(context.Background(), 10, ""); len(jobs) != 0 {
				t.Errorf("rejected submit persisted %d jobs", len(jobs))
			}
		})
	}
}

func TestSubmitWithoutProfilesRequiresRunConfig(t *testing.T) {
	profiles, err := NewProfiles(nil, "")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, registry.New(), &fakeLauncher{}, profiles, slog.New(slog.DiscardHandler))

	_, err = svc.Submit(context.Background(), SubmitParams{Prompt: "x"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "no default profile") {
		t.Errorf("error %q does not explain the missing default", err)
	}
}

func TestSubmitLaunchFailureMarksJobFailed(t *testing.T) {
	f := newFixture(t)
	f.launcher.err = errors.New("no capacity in namespace")

	j := f.submitted(t, SubmitParams{Prompt: "x"})

	got, err := f.store.GetJob(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != job.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "no capacity") {
		t.Errorf("error = %q, want the launcher's message", got.Error)
	}
	if got.FinishedAt.IsZero() {
		t.Error("finished_at not set on a failed job")
	}
	assertStatusEvents(t, f.store, j.ID, "queued", "launching", "failed")

	evs, err := f.store.ReadEvents(context.Background(), j.ID, "", 10)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	last := evs[len(evs)-1]
	var payload statusPayload
	if err := json.Unmarshal([]byte(last.PayloadJSON), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !strings.Contains(payload.Error, "no capacity") {
		t.Errorf("failure event payload = %q, want the launch error", last.PayloadJSON)
	}
}

func TestCancelWithoutSessionCancelsImmediately(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})

	got, err := f.svc.Cancel(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got.Status != job.StatusCancelled {
		t.Errorf("status = %s, want cancelled", got.Status)
	}
	if !got.CancelRequested {
		t.Error("cancel_requested not set")
	}
	if got.FinishedAt.IsZero() {
		t.Error("finished_at not set")
	}
	assertStatusEvents(t, f.store, j.ID, "queued", "launching", "awaiting_harness", "cancelled")
}

func TestCancelWithLiveSessionSendsControlEvent(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	sess := &fakeSession{}
	if err := f.registry.Register(j.ID, sess); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := f.svc.Cancel(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !got.CancelRequested {
		t.Error("cancel_requested not set")
	}
	if got.Status.Terminal() {
		t.Errorf("status = %s, want the harness to finalise", got.Status)
	}
	sent := sess.sent()
	if len(sent) != 1 || sent[0].GetType() != "cancel" {
		t.Fatalf("session got %v, want one cancel event", sent)
	}
}

func TestCancelIsIdempotentOnTerminalJobs(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	first, err := f.svc.Cancel(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("first Cancel: %v", err)
	}

	second, err := f.svc.Cancel(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	if !second.FinishedAt.Equal(first.FinishedAt) {
		t.Errorf("finished_at moved from %v to %v", first.FinishedAt, second.FinishedAt)
	}
	assertStatusEvents(t, f.store, j.ID, "queued", "launching", "awaiting_harness", "cancelled")
}

func TestCancelUnknownJob(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Cancel(context.Background(), "hp-00000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestAnswerPermission(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	sess := &fakeSession{}
	if err := f.registry.Register(j.ID, sess); err != nil {
		t.Fatalf("Register: %v", err)
	}
	putPending(t, f.store, j.ID, "req-1")

	p, err := f.svc.AnswerPermission(context.Background(), j.ID, "req-1", true, "looks fine")
	if err != nil {
		t.Fatalf("AnswerPermission: %v", err)
	}
	if p.State != store.PermissionAllowed {
		t.Errorf("state = %s, want allowed", p.State)
	}
	if p.AnsweredAt.IsZero() {
		t.Error("answered_at not set")
	}

	sent := sess.sent()
	if len(sent) != 1 {
		t.Fatalf("session got %d events, want 1", len(sent))
	}
	ev := sent[0]
	if ev.GetType() != "permission_response" || ev.GetRequestId() != "req-1" || !ev.GetAllowed().GetValue() {
		t.Errorf("control event = %+v, want an allow for req-1", ev)
	}
	if ev.GetReason() != "looks fine" {
		t.Errorf("reason = %q", ev.GetReason())
	}

	stored, err := f.store.GetPermission(context.Background(), j.ID, "req-1")
	if err != nil {
		t.Fatalf("GetPermission: %v", err)
	}
	if stored.State != store.PermissionAllowed {
		t.Errorf("stored state = %s, want allowed", stored.State)
	}
}

func TestAnswerPermissionDeny(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	sess := &fakeSession{}
	if err := f.registry.Register(j.ID, sess); err != nil {
		t.Fatalf("Register: %v", err)
	}
	putPending(t, f.store, j.ID, "req-1")

	p, err := f.svc.AnswerPermission(context.Background(), j.ID, "req-1", false, "writes outside the workspace")
	if err != nil {
		t.Fatalf("AnswerPermission: %v", err)
	}
	if p.State != store.PermissionDenied {
		t.Errorf("state = %s, want denied", p.State)
	}
	if sess.sent()[0].GetAllowed().GetValue() {
		t.Error("control event allowed a denied request")
	}
}

func TestAnswerPermissionRejectsSecondAnswer(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	sess := &fakeSession{}
	if err := f.registry.Register(j.ID, sess); err != nil {
		t.Fatalf("Register: %v", err)
	}
	putPending(t, f.store, j.ID, "req-1")

	if _, err := f.svc.AnswerPermission(context.Background(), j.ID, "req-1", true, ""); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	_, err := f.svc.AnswerPermission(context.Background(), j.ID, "req-1", false, "")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if len(sess.sent()) != 1 {
		t.Errorf("session got %d events, want the second answer suppressed", len(sess.sent()))
	}
}

func TestAnswerPermissionWithoutSession(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	putPending(t, f.store, j.ID, "req-1")

	_, err := f.svc.AnswerPermission(context.Background(), j.ID, "req-1", true, "")
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("error = %v, want ErrNotConnected", err)
	}
	stored, err := f.store.GetPermission(context.Background(), j.ID, "req-1")
	if err != nil {
		t.Fatalf("GetPermission: %v", err)
	}
	if stored.State != store.PermissionPending {
		t.Errorf("state = %s, want it left pending when the send failed", stored.State)
	}
}

func TestAnswerPermissionUnknownRequest(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})
	_, err := f.svc.AnswerPermission(context.Background(), j.ID, "missing", true, "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestWatchClosesOnTerminalStatus(t *testing.T) {
	f := newFixture(t)
	j := f.submitted(t, SubmitParams{Prompt: "x"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := f.svc.Watch(ctx, j.ID, "")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	go func() {
		_, _ = f.store.UpdateJob(context.Background(), j.ID, func(cur *job.Job) error {
			cur.Status = job.StatusSucceeded
			return nil
		})
		f.svc.appendStatusEvent(context.Background(), j.ID, job.StatusSucceeded, "")
		// An event after the terminal transition must not be delivered.
		_, _ = f.store.AppendEvent(context.Background(), j.ID, store.Event{Type: "text_delta", At: time.Now()})
	}()

	var types []string
	for ev := range events {
		types = append(types, ev.Type+":"+ev.PayloadJSON)
	}
	last := types[len(types)-1]
	if !strings.Contains(last, "succeeded") {
		t.Errorf("last delivered event = %q, want the succeeded status change", last)
	}
}

func TestWatchOnTerminalJobReplaysHistoryThenCloses(t *testing.T) {
	f := newFixture(t)
	f.launcher.err = errors.New("boom")
	j := f.submitted(t, SubmitParams{Prompt: "x"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := f.svc.Watch(ctx, j.ID, "")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	var got int
	for range events {
		got++
	}
	if got != 3 {
		t.Errorf("replayed %d events, want the 3 status changes", got)
	}
}

func TestWatchResumesAfterCursor(t *testing.T) {
	f := newFixture(t)
	f.launcher.err = errors.New("boom")
	j := f.submitted(t, SubmitParams{Prompt: "x"})

	all, err := f.store.ReadEvents(context.Background(), j.ID, "", 10)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := f.svc.Watch(ctx, j.ID, all[0].ID)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	var got int
	for range events {
		got++
	}
	if got != len(all)-1 {
		t.Errorf("replayed %d events after the cursor, want %d", got, len(all)-1)
	}
}

func TestWatchUnknownJob(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Watch(context.Background(), "hp-00000000000000000000000000", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestListAndGet(t *testing.T) {
	f := newFixture(t)
	first := f.submitted(t, SubmitParams{Prompt: "one"})
	second := f.submitted(t, SubmitParams{Prompt: "two"})

	got, err := f.svc.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Prompt != "one" {
		t.Errorf("prompt = %q", got.Prompt)
	}

	jobs, _, err := f.svc.List(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != second.ID {
		t.Fatalf("List returned %d jobs, newest first = %v", len(jobs), jobs)
	}
}

func putPending(t *testing.T, st store.Store, jobID, requestID string) {
	t.Helper()
	err := st.PutPermission(context.Background(), jobID, store.PermissionRequest{
		RequestID:   requestID,
		ToolName:    "bash",
		InputJSON:   `{"command":"ls"}`,
		State:       store.PermissionPending,
		RequestedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("PutPermission: %v", err)
	}
}

func assertStatusEvents(t *testing.T, st store.Store, jobID string, want ...string) {
	t.Helper()
	evs, err := st.ReadEvents(context.Background(), jobID, "", 100)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	var got []string
	for _, ev := range evs {
		if ev.Type != EventTypeStatusChange {
			continue
		}
		var p statusPayload
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &p); err != nil {
			t.Fatalf("decode %q: %v", ev.PayloadJSON, err)
		}
		got = append(got, p.Status)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("status events = %v, want %v", got, want)
	}
}
