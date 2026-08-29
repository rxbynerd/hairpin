package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

const testRunConfig = `{"runId":"hp-test","prompt":"do the thing","maxTurns":4}`

// fakeStream is a channel-backed stand-in for connect's bidi stream.
// Closing in ends the stream with endErr (io.EOF by default).
type fakeStream struct {
	in     chan *harnessv1.HarnessEvent
	endErr error
	// onReceive runs before the nth event is handed over, i.e. once the
	// previous event has been fully processed. Tests use it to advance a
	// fake clock or mutate the store at a known point in the run.
	onReceive func(n int)
	received  int

	mu      sync.Mutex
	sent    []*harnessv1.ControlEvent
	sendErr error
}

func newFakeStream(evs ...*harnessv1.HarnessEvent) *fakeStream {
	f := &fakeStream{in: make(chan *harnessv1.HarnessEvent, len(evs))}
	for _, ev := range evs {
		f.in <- ev
	}
	close(f.in)
	return f
}

func (f *fakeStream) Receive() (*harnessv1.HarnessEvent, error) {
	if f.onReceive != nil {
		f.onReceive(f.received)
	}
	f.received++
	ev, ok := <-f.in
	if !ok {
		if f.endErr != nil {
			return nil, f.endErr
		}
		return nil, io.EOF
	}
	return ev, nil
}

func (f *fakeStream) Send(ev *harnessv1.ControlEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, ev)
	return nil
}

// failSends makes every later Send fail, standing in for a harness that
// hung up.
func (f *fakeStream) failSends(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

func (f *fakeStream) controls() []*harnessv1.ControlEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*harnessv1.ControlEvent(nil), f.sent...)
}

func (f *fakeStream) types() []string {
	var out []string
	for _, ev := range f.controls() {
		out = append(out, ev.GetType())
	}
	return out
}

// testClock is a hand-advanced clock. Reads and advances are
// serialised because the pump answers memory calls on detached
// goroutines that read the clock while a test advances it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Unix(1700000000, 0)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func testHandler(t *testing.T, opts ...Option) (*Handler, store.Store, *registry.Registry) {
	t.Helper()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	reg := registry.New()
	h := New(st, reg, append([]Option{WithLogger(slog.New(slog.DiscardHandler))}, opts...)...)
	return h, st, reg
}

func testHandlerWithIssuer(t *testing.T, issuer SandboxTokenIssuer) (*Handler, store.Store, *registry.Registry) {
	t.Helper()
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	reg := registry.New()
	h := New(st, reg, WithLogger(slog.New(slog.DiscardHandler)), WithSandboxTokenIssuer(issuer))
	return h, st, reg
}

// fakeIssuer stands in for *internal/tokenissuer.Issuer, recording every
// Mint call so tests can assert what sub and repo scope reached it.
type fakeIssuer struct {
	audience string
	err      error

	mu     sync.Mutex
	minted []fakeMint
}

type fakeMint struct {
	sub   string
	scope []string
}

func (f *fakeIssuer) Audience() string { return f.audience }

func (f *fakeIssuer) Mint(sub string, scope []string) (string, time.Time, error) {
	f.mu.Lock()
	f.minted = append(f.minted, fakeMint{sub: sub, scope: append([]string(nil), scope...)})
	f.mu.Unlock()
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "signed." + sub, time.Now().Add(15 * time.Minute), nil
}

func seedJob(t *testing.T, st store.Store, id string, status job.Status, mutate func(*job.Job)) *job.Job {
	t.Helper()
	j := &job.Job{
		ID:            id,
		Status:        status,
		Prompt:        "do the thing",
		RunConfigJSON: testRunConfig,
		CreatedAt:     time.Now(),
	}
	if mutate != nil {
		mutate(j)
	}
	if err := st.CreateJob(context.Background(), j); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return j
}

func ready(id string) *harnessv1.HarnessEvent {
	return &harnessv1.HarnessEvent{Type: evReady, Id: id, HarnessVersion: "test"}
}

func delta(text string) *harnessv1.HarnessEvent {
	return &harnessv1.HarnessEvent{Type: evTextDelta, Text: text}
}

func getJob(t *testing.T, st store.Store, id string) *job.Job {
	t.Helper()
	j, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	return j
}

func events(t *testing.T, st store.Store, id string) []store.Event {
	t.Helper()
	evs, err := st.ReadEvents(context.Background(), id, "", 0)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	return evs
}

func eventTypes(evs []store.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

func decode(t *testing.T, payload string) *harnessv1.HarnessEvent {
	t.Helper()
	var ev harnessv1.HarnessEvent
	if err := protojson.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("decode payload %q: %v", payload, err)
	}
	return &ev
}

func TestRunTaskHappyPath(t *testing.T) {
	h, st, reg := testHandler(t)
	seedJob(t, st, "hp-1", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-1"),
		delta("hel"),
		delta("lo"),
		&harnessv1.HarnessEvent{Type: evHeartbeat},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)

	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	ctls := s.controls()
	if len(ctls) != 1 || ctls[0].GetType() != ctlTaskAssignment {
		t.Fatalf("want single task_assignment, got %v", s.types())
	}
	if got := ctls[0].GetTask().GetRunId(); got != "hp-test" {
		t.Errorf("assignment run_id = %q, want hp-test", got)
	}
	if got := ctls[0].GetTask().GetMaxTurns(); got != 4 {
		t.Errorf("assignment max_turns = %d, want 4", got)
	}

	j := getJob(t, st, "hp-1")
	if j.Status != job.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", j.Status)
	}
	if j.StopReason != "success" {
		t.Errorf("stop reason = %q, want success", j.StopReason)
	}
	if j.FinalText != "hello" {
		t.Errorf("final text = %q, want hello", j.FinalText)
	}
	if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
		t.Errorf("timestamps not set: started=%v finished=%v", j.StartedAt, j.FinishedAt)
	}

	evs := events(t, st, "hp-1")
	want := []string{EventStatusChange, evTextDelta, evHeartbeat, evDone, EventStatusChange}
	if got := eventTypes(evs); !equalStrings(got, want) {
		t.Fatalf("timeline = %v, want %v", got, want)
	}
	if got := decode(t, evs[1].PayloadJSON).GetText(); got != "hello" {
		t.Errorf("coalesced delta = %q, want hello", got)
	}
	if evs[2].PayloadJSON != "" {
		t.Errorf("heartbeat payload = %q, want empty", evs[2].PayloadJSON)
	}
	if !strings.Contains(evs[4].PayloadJSON, string(job.StatusSucceeded)) {
		t.Errorf("terminal status_change payload = %q", evs[4].PayloadJSON)
	}
	if reg.Connected("hp-1") {
		t.Error("session still registered after stream end")
	}
}

func TestRunTaskUnknownSession(t *testing.T) {
	h, st, _ := testHandler(t)

	for name, ev := range map[string]*harnessv1.HarnessEvent{
		"unknown job":  ready("hp-missing"),
		"empty id":     ready(""),
		"not ready":    {Type: evHeartbeat},
		"unknown type": {Type: "future_event", Id: "hp-missing"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newFakeStream(ev)
			if err := h.runTask(context.Background(), s); err != nil {
				t.Fatalf("runTask: %v", err)
			}
			if got := s.types(); !equalStrings(got, []string{ctlCancel}) {
				t.Fatalf("controls = %v, want [cancel]", got)
			}
		})
	}
	if _, err := st.GetJob(context.Background(), "hp-missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown session created a job: %v", err)
	}
}

func TestRunTaskDuplicateSession(t *testing.T) {
	h, st, reg := testHandler(t)
	seedJob(t, st, "hp-dup", job.StatusAwaitingHarness, nil)

	incumbent := &session{s: newFakeStream()}
	if err := reg.Register("hp-dup", incumbent); err != nil {
		t.Fatalf("Register: %v", err)
	}

	s := newFakeStream(ready("hp-dup"))
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if got := s.types(); !equalStrings(got, []string{ctlCancel}) {
		t.Fatalf("controls = %v, want [cancel]", got)
	}
	if j := getJob(t, st, "hp-dup"); j.Status != job.StatusAwaitingHarness {
		t.Errorf("status = %q, want awaiting_harness (record must be untouched)", j.Status)
	}
	if len(events(t, st, "hp-dup")) != 0 {
		t.Error("duplicate harness wrote to the timeline")
	}
	if !reg.Connected("hp-dup") {
		t.Error("incumbent session was evicted")
	}
}

func TestRunTaskCancelBeforeAssignment(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-cancel", job.StatusAwaitingHarness, func(j *job.Job) {
		j.CancelRequested = true
	})

	s := newFakeStream(ready("hp-cancel"))
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if got := s.types(); !equalStrings(got, []string{ctlCancel}) {
		t.Fatalf("controls = %v, want [cancel]", got)
	}
	j := getJob(t, st, "hp-cancel")
	if j.Status != job.StatusCancelled {
		t.Errorf("status = %q, want cancelled", j.Status)
	}
	if j.FinishedAt.IsZero() {
		t.Error("FinishedAt not set on cancelled job")
	}
	if got := eventTypes(events(t, st, "hp-cancel")); !equalStrings(got, []string{EventStatusChange}) {
		t.Errorf("timeline = %v, want [status_change]", got)
	}
}

func TestRunTaskTerminalJobLeftAlone(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-old", job.StatusSucceeded, func(j *job.Job) {
		j.StopReason = "success"
	})

	s := newFakeStream(ready("hp-old"))
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if got := s.types(); !equalStrings(got, []string{ctlCancel}) {
		t.Fatalf("controls = %v, want [cancel]", got)
	}
	if j := getJob(t, st, "hp-old"); j.Status != job.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", j.Status)
	}
	if len(events(t, st, "hp-old")) != 0 {
		t.Error("terminal job timeline was modified")
	}
}

func TestRunTaskBadRunConfigFailsJob(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-bad", job.StatusAwaitingHarness, func(j *job.Job) {
		j.RunConfigJSON = `{"runId":`
	})

	s := newFakeStream(ready("hp-bad"))
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if got := s.types(); !equalStrings(got, []string{ctlCancel}) {
		t.Fatalf("controls = %v, want [cancel]", got)
	}
	j := getJob(t, st, "hp-bad")
	if j.Status != job.StatusFailed {
		t.Errorf("status = %q, want failed", j.Status)
	}
	if !strings.Contains(j.Error, "run config") {
		t.Errorf("error = %q, want a run config complaint", j.Error)
	}
}

func TestRunTaskPersistsPermissionRequest(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-perm", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-perm"),
		&harnessv1.HarnessEvent{
			Type:      evPermissionRequest,
			RequestId: "req-1",
			ToolName:  "run_command",
			Input:     []byte(`{"cmd":"rm -rf /"}`),
		},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	p, err := st.GetPermission(context.Background(), "hp-perm", "req-1")
	if err != nil {
		t.Fatalf("GetPermission: %v", err)
	}
	if p.State != store.PermissionPending {
		t.Errorf("state = %q, want pending", p.State)
	}
	if p.ToolName != "run_command" {
		t.Errorf("tool = %q, want run_command", p.ToolName)
	}
	if p.InputJSON != `{"cmd":"rm -rf /"}` {
		t.Errorf("input = %q", p.InputJSON)
	}
	if p.RequestedAt.IsZero() {
		t.Error("RequestedAt not set")
	}
	if got := eventTypes(events(t, st, "hp-perm")); !equalStrings(got,
		[]string{EventStatusChange, evPermissionRequest, evDone, EventStatusChange}) {
		t.Errorf("timeline = %v", got)
	}
}

func TestRunTaskRefusesSandboxToken(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-sbx", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-sbx"),
		&harnessv1.HarnessEvent{Type: evSandboxTokenRequest, RequestId: "sbx-1", Audience: "https://haybale.internal"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "setup_failed"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	ctls := s.controls()
	if len(ctls) != 2 || ctls[1].GetType() != ctlSandboxTokenResponse {
		t.Fatalf("controls = %v, want [task_assignment sandbox_token_response]", s.types())
	}
	resp := ctls[1]
	if resp.GetRequestId() != "sbx-1" {
		t.Errorf("request_id = %q, want sbx-1", resp.GetRequestId())
	}
	if !resp.GetIsError().GetValue() {
		t.Error("is_error not set on refusal")
	}
	if resp.GetReason() != sandboxTokenRefusal {
		t.Errorf("reason = %q", resp.GetReason())
	}
	if resp.GetToken() != "" {
		t.Error("refusal carried a token")
	}
}

func TestRunTaskIssuesSandboxToken(t *testing.T) {
	issuer := &fakeIssuer{audience: "https://haybale.internal"}
	h, st, _ := testHandlerWithIssuer(t, issuer)
	seedJob(t, st, "hp-sbx-ok", job.StatusAwaitingHarness, func(j *job.Job) {
		j.RepoScope = []string{"github.com/rxbynerd/*"}
	})

	s := newFakeStream(
		ready("hp-sbx-ok"),
		&harnessv1.HarnessEvent{Type: evSandboxTokenRequest, RequestId: "sbx-1", Audience: "https://haybale.internal"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	ctls := s.controls()
	if len(ctls) != 2 || ctls[1].GetType() != ctlSandboxTokenResponse {
		t.Fatalf("controls = %v, want [task_assignment sandbox_token_response]", s.types())
	}
	resp := ctls[1]
	if resp.GetIsError().GetValue() {
		t.Errorf("is_error set on a successful mint: reason=%q", resp.GetReason())
	}
	if resp.GetRequestId() != "sbx-1" {
		t.Errorf("request_id = %q, want sbx-1", resp.GetRequestId())
	}
	if resp.GetToken() != "signed.hp-sbx-ok" {
		t.Errorf("token = %q", resp.GetToken())
	}
	if resp.GetExpiresAt() == 0 {
		t.Error("expires_at not set on a successful mint")
	}

	if len(issuer.minted) != 1 || issuer.minted[0].sub != "hp-sbx-ok" {
		t.Fatalf("Mint calls = %+v, want one call for hp-sbx-ok", issuer.minted)
	}
	if got := issuer.minted[0].scope; !equalStrings(got, []string{"github.com/rxbynerd/*"}) {
		t.Errorf("Mint repo scope = %v, want the job's RepoScope", got)
	}
}

func TestRunTaskSandboxTokenMintErrorIsGeneric(t *testing.T) {
	issuer := &fakeIssuer{audience: "aud", err: errors.New("kms unavailable: rate limited")}
	h, st, _ := testHandlerWithIssuer(t, issuer)
	seedJob(t, st, "hp-sbx-err", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-sbx-err"),
		&harnessv1.HarnessEvent{Type: evSandboxTokenRequest, RequestId: "sbx-2"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "setup_failed"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	resp := s.controls()[1]
	if !resp.GetIsError().GetValue() {
		t.Fatal("is_error not set on a mint failure")
	}
	if resp.GetReason() != sandboxTokenIssuanceFailure {
		t.Errorf("reason = %q, want the generic failure message", resp.GetReason())
	}
	if strings.Contains(resp.GetReason(), "kms") {
		t.Error("mint error internals leaked into the harness-facing reason")
	}
	if resp.GetToken() != "" {
		t.Error("error response carried a token")
	}
}

func TestRunTaskSandboxTokenAudienceMismatchStillIssues(t *testing.T) {
	issuer := &fakeIssuer{audience: "https://haybale.internal"}
	h, st, _ := testHandlerWithIssuer(t, issuer)
	seedJob(t, st, "hp-sbx-aud", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-sbx-aud"),
		&harnessv1.HarnessEvent{Type: evSandboxTokenRequest, RequestId: "sbx-3", Audience: "https://someone-else.example"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	resp := s.controls()[1]
	if resp.GetIsError().GetValue() {
		t.Fatal("a requested audience that differs from the configured one caused a refusal; want the configured audience to win")
	}
	if len(issuer.minted) != 1 {
		t.Fatalf("Mint calls = %d, want 1", len(issuer.minted))
	}
}

func TestRunTaskSandboxTokenNeverInTimeline(t *testing.T) {
	issuer := &fakeIssuer{audience: "aud"}
	h, st, _ := testHandlerWithIssuer(t, issuer)
	seedJob(t, st, "hp-sbx-secret", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-sbx-secret"),
		&harnessv1.HarnessEvent{Type: evSandboxTokenRequest, RequestId: "sbx-4"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	token := s.controls()[1].GetToken()
	if token == "" {
		t.Fatal("no token minted")
	}
	for _, ev := range events(t, st, "hp-sbx-secret") {
		if strings.Contains(ev.PayloadJSON, token) {
			t.Errorf("minted token leaked into timeline event %q: %s", ev.Type, ev.PayloadJSON)
		}
	}
}

func TestRunTaskUnsupportedRequestsAreRecorded(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-unsup", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-unsup"),
		&harnessv1.HarnessEvent{Type: evBatchSubmission, RequestId: "b-1"},
		ready("hp-unsup"),
		&harnessv1.HarnessEvent{Type: "future_event", Message: "hello from stirrup 2"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if got := s.types(); !equalStrings(got, []string{ctlTaskAssignment}) {
		t.Fatalf("controls = %v, want only the assignment", got)
	}
	want := []string{EventStatusChange, evBatchSubmission, "future_event", evDone, EventStatusChange}
	if got := eventTypes(events(t, st, "hp-unsup")); !equalStrings(got, want) {
		t.Errorf("timeline = %v, want %v", got, want)
	}
}

func TestRunTaskStreamClosedWithoutDone(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-crash", job.StatusAwaitingHarness, nil)

	s := newFakeStream(ready("hp-crash"), delta("partial"))
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	j := getJob(t, st, "hp-crash")
	if j.Status != job.StatusFailed {
		t.Fatalf("status = %q, want failed", j.Status)
	}
	if j.Error != msgStreamClosed {
		t.Errorf("error = %q, want %q", j.Error, msgStreamClosed)
	}
	if j.FinalText != "partial" {
		t.Errorf("final text = %q, want partial", j.FinalText)
	}
	want := []string{EventStatusChange, evTextDelta, EventStatusChange}
	if got := eventTypes(events(t, st, "hp-crash")); !equalStrings(got, want) {
		t.Errorf("timeline = %v, want %v", got, want)
	}
}

func TestRunTaskStreamClosedAfterErrorEvent(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-err", job.StatusAwaitingHarness, nil)

	s := newFakeStream(ready("hp-err"), &harnessv1.HarnessEvent{Type: evError, Message: "provider exploded"})
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	j := getJob(t, st, "hp-err")
	if j.Status != job.StatusFailed {
		t.Fatalf("status = %q, want failed", j.Status)
	}
	if !strings.Contains(j.Error, msgStreamClosed) || !strings.Contains(j.Error, "provider exploded") {
		t.Errorf("error = %q, want both the crash note and the harness message", j.Error)
	}
}

func TestRunTaskCancelledStreamClosureStaysCancelled(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-late", job.StatusAwaitingHarness, nil)

	s := newFakeStream(ready("hp-late"))
	s.onReceive = func(n int) {
		if n != 1 { // cancel lands after the task was assigned
			return
		}
		if _, err := st.UpdateJob(context.Background(), "hp-late", func(j *job.Job) error {
			j.CancelRequested = true
			return nil
		}); err != nil {
			t.Errorf("UpdateJob: %v", err)
		}
	}

	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}
	j := getJob(t, st, "hp-late")
	if j.Status != job.StatusCancelled {
		t.Errorf("status = %q, want cancelled", j.Status)
	}
	if j.Error != "" {
		t.Errorf("error = %q, want empty for a cancelled run", j.Error)
	}
}

func TestRunTaskErrorEventFeedsDone(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-fail", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-fail"),
		&harnessv1.HarnessEvent{Type: evError, Message: "tool budget exhausted"},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "budget_exceeded"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	j := getJob(t, st, "hp-fail")
	if j.Status != job.StatusFailed {
		t.Errorf("status = %q, want failed", j.Status)
	}
	if j.StopReason != "budget_exceeded" {
		t.Errorf("stop reason = %q, want budget_exceeded preserved", j.StopReason)
	}
	if j.Error != "tool budget exhausted" {
		t.Errorf("error = %q", j.Error)
	}
}

func TestRunTaskUnknownStopReasonPreserved(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-new", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-new"),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "quantum_collapse"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	j := getJob(t, st, "hp-new")
	if j.Status != job.StatusFailed {
		t.Errorf("status = %q, want failed", j.Status)
	}
	if j.StopReason != "quantum_collapse" {
		t.Errorf("stop reason = %q, want verbatim", j.StopReason)
	}
}

func TestRunTaskCancelledStopReason(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-stopc", job.StatusAwaitingHarness, nil)

	s := newFakeStream(ready("hp-stopc"), &harnessv1.HarnessEvent{Type: evDone, StopReason: "cancelled"})
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if j := getJob(t, st, "hp-stopc"); j.Status != job.StatusCancelled {
		t.Errorf("status = %q, want cancelled", j.Status)
	}
}

func TestTextDeltaCoalescingSplitsOnByteCap(t *testing.T) {
	h, st, _ := testHandler(t)
	h.deltaFlushBytes = 8
	h.deltaFlushEvery = time.Hour
	seedJob(t, st, "hp-coal", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-coal"),
		delta("aaaa"), delta("bbbb"), // hits the 8-byte cap
		delta("cc"),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	evs := events(t, st, "hp-coal")
	want := []string{EventStatusChange, evTextDelta, evTextDelta, evDone, EventStatusChange}
	if got := eventTypes(evs); !equalStrings(got, want) {
		t.Fatalf("timeline = %v, want %v", got, want)
	}
	if got := decode(t, evs[1].PayloadJSON).GetText(); got != "aaaabbbb" {
		t.Errorf("first flush = %q, want aaaabbbb", got)
	}
	if got := decode(t, evs[2].PayloadJSON).GetText(); got != "cc" {
		t.Errorf("terminal flush = %q, want cc", got)
	}
}

func TestTextDeltaCoalescingSplitsOnInterval(t *testing.T) {
	h, st, _ := testHandler(t)
	h.deltaFlushEvery = 500 * time.Millisecond
	h.deltaFlushBytes = 1 << 20
	clock := newTestClock()
	h.now = clock.now
	seedJob(t, st, "hp-time", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-time"),
		delta("a"), delta("b"), delta("c"),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	s.onReceive = func(n int) {
		if n == 2 { // "a" is buffered; age it past the flush interval.
			clock.advance(time.Second)
		}
	}

	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}
	evs := events(t, st, "hp-time")
	want := []string{EventStatusChange, evTextDelta, evTextDelta, evDone, EventStatusChange}
	if got := eventTypes(evs); !equalStrings(got, want) {
		t.Fatalf("timeline = %v, want %v", got, want)
	}
	if got := decode(t, evs[1].PayloadJSON).GetText(); got != "ab" {
		t.Errorf("aged flush = %q, want ab", got)
	}
	if got := decode(t, evs[2].PayloadJSON).GetText(); got != "c" {
		t.Errorf("terminal flush = %q, want c", got)
	}
}

func TestFinalTextCapped(t *testing.T) {
	h, st, _ := testHandler(t)
	h.maxFinalTextByte = 6
	seedJob(t, st, "hp-cap", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-cap"),
		delta("abcd"), delta("efgh"), delta("ijkl"),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	if j := getJob(t, st, "hp-cap"); j.FinalText != "abcdef" {
		t.Errorf("final text = %q, want abcdef (capped)", j.FinalText)
	}
	// Truncation must not affect the timeline: every fragment is kept.
	evs := events(t, st, "hp-cap")
	if got := decode(t, evs[1].PayloadJSON).GetText(); got != "abcdefghijkl" {
		t.Errorf("timeline delta = %q, want the full text", got)
	}
}

func TestLastEventAtFlushedOnInterval(t *testing.T) {
	h, st, _ := testHandler(t)
	clock := newTestClock()
	h.now = clock.now
	h.lastEventFlush = 10 * time.Second
	seedJob(t, st, "hp-live", job.StatusAwaitingHarness, nil)

	assigned := clock.now()
	s := newFakeStream(
		ready("hp-live"),
		&harnessv1.HarnessEvent{Type: evHeartbeat},
		&harnessv1.HarnessEvent{Type: evHeartbeat},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	s.onReceive = func(n int) {
		switch n {
		case 1:
			clock.advance(5 * time.Second)
		case 2:
			if got := getJob(t, st, "hp-live").LastEventAt; !got.Equal(assigned) {
				t.Errorf("LastEventAt flushed before the interval elapsed: %v", got)
			}
			clock.advance(10 * time.Second)
		case 3:
			if got := getJob(t, st, "hp-live").LastEventAt; !got.Equal(clock.now()) {
				t.Errorf("LastEventAt = %v, want the flushed %v", got, clock.now())
			}
		}
	}

	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}
}

func TestSessionSendIsRoutedThroughRegistry(t *testing.T) {
	h, st, reg := testHandler(t)
	seedJob(t, st, "hp-reg", job.StatusAwaitingHarness, nil)

	s := newFakeStream(ready("hp-reg"), &harnessv1.HarnessEvent{Type: evDone, StopReason: "cancelled"})
	s.onReceive = func(n int) {
		if n != 1 { // the run is assigned and registered by now
			return
		}
		if err := reg.Send("hp-reg", &harnessv1.ControlEvent{Type: ctlCancel}); err != nil {
			t.Errorf("registry Send: %v", err)
		}
	}

	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if got := s.types(); !equalStrings(got, []string{ctlTaskAssignment, ctlCancel}) {
		t.Fatalf("controls = %v, want [task_assignment cancel]", got)
	}
	if err := reg.Send("hp-reg", &harnessv1.ControlEvent{Type: ctlCancel}); !errors.Is(err, registry.ErrNotConnected) {
		t.Errorf("Send after the stream ended = %v, want ErrNotConnected", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
