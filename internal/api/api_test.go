package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	hairpinv1 "github.com/rxbynerd/hairpin/gen/hairpin/v1"
	"github.com/rxbynerd/hairpin/gen/hairpin/v1/hairpinv1connect"
	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/service"
	"github.com/rxbynerd/hairpin/internal/store"
)

// nopLauncher accepts every launch without starting anything.
type nopLauncher struct{}

func (nopLauncher) Launch(context.Context, *job.Job) error { return nil }

// recordingSession stands in for a live harness stream.
type recordingSession struct {
	mu     sync.Mutex
	events []*harnessv1.ControlEvent
}

func (r *recordingSession) Send(ev *harnessv1.ControlEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

type harness struct {
	client   hairpinv1connect.JobServiceClient
	svc      *service.Service
	store    store.Store
	registry *registry.Registry
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	template := &harnessv1.RunConfig{
		Mode:     "planning",
		Provider: &harnessv1.ProviderConfig{Type: "anthropic"},
		MaxTurns: 20,
		Timeout:  proto.Int32(600),
	}
	profiles, err := service.NewProfiles(map[string]*harnessv1.RunConfig{"default": template}, "default")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	reg := registry.New()
	svc := service.New(st, reg, nopLauncher{}, profiles, slog.New(slog.DiscardHandler))

	mux := http.NewServeMux()
	mux.Handle(hairpinv1connect.NewJobServiceHandler(New(svc)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &harness{
		client:   hairpinv1connect.NewJobServiceClient(srv.Client(), srv.URL),
		svc:      svc,
		store:    st,
		registry: reg,
	}
}

func (h *harness) submit(t *testing.T, prompt string) *hairpinv1.Job {
	t.Helper()
	resp, err := h.client.SubmitJob(context.Background(), connect.NewRequest(&hairpinv1.SubmitJobRequest{Prompt: prompt}))
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	h.svc.WaitForLaunches()
	return resp.Msg.GetJob()
}

func TestSubmitAndGetJob(t *testing.T) {
	h := newHarness(t)
	submitted := h.submit(t, "check the logs")

	if submitted.GetStatus() != hairpinv1.JobStatus_JOB_STATUS_QUEUED {
		t.Errorf("submit status = %s, want QUEUED", submitted.GetStatus())
	}
	if submitted.GetCreatedAt() == nil {
		t.Error("created_at missing")
	}
	if submitted.GetFinishedAt() != nil {
		t.Error("finished_at set on a fresh job")
	}

	got, err := h.client.GetJob(context.Background(), connect.NewRequest(&hairpinv1.GetJobRequest{Id: submitted.GetId()}))
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Msg.GetJob().GetStatus() != hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS {
		t.Errorf("status = %s, want AWAITING_HARNESS", got.Msg.GetJob().GetStatus())
	}
	if got.Msg.GetJob().GetProfile() != "default" {
		t.Errorf("profile = %q", got.Msg.GetJob().GetProfile())
	}
}

func TestSubmitJobInvalidArgument(t *testing.T) {
	h := newHarness(t)
	_, err := h.client.SubmitJob(context.Background(), connect.NewRequest(&hairpinv1.SubmitJobRequest{Profile: "nope", Prompt: "x"}))
	assertCode(t, err, connect.CodeInvalidArgument)
}

func TestGetJobNotFound(t *testing.T) {
	h := newHarness(t)
	_, err := h.client.GetJob(context.Background(), connect.NewRequest(&hairpinv1.GetJobRequest{Id: "hp-missing"}))
	assertCode(t, err, connect.CodeNotFound)
}

func TestAnswerPermissionWithoutHarnessIsFailedPrecondition(t *testing.T) {
	h := newHarness(t)
	j := h.submit(t, "x")
	putPending(t, h.store, j.GetId(), "req-1")

	_, err := h.client.AnswerPermission(context.Background(), connect.NewRequest(&hairpinv1.AnswerPermissionRequest{
		JobId:     j.GetId(),
		RequestId: "req-1",
		Allow:     true,
	}))
	assertCode(t, err, connect.CodeFailedPrecondition)
}

func TestAnswerPermissionAndList(t *testing.T) {
	h := newHarness(t)
	j := h.submit(t, "x")
	if err := h.registry.Register(j.GetId(), &recordingSession{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	putPending(t, h.store, j.GetId(), "req-1")

	answered, err := h.client.AnswerPermission(context.Background(), connect.NewRequest(&hairpinv1.AnswerPermissionRequest{
		JobId:     j.GetId(),
		RequestId: "req-1",
		Allow:     false,
		Reason:    "not on prod",
	}))
	if err != nil {
		t.Fatalf("AnswerPermission: %v", err)
	}
	p := answered.Msg.GetPermissionRequest()
	if p.GetState() != string(store.PermissionDenied) || p.GetReason() != "not on prod" {
		t.Errorf("permission = %+v, want denied with a reason", p)
	}
	if p.GetAnsweredAt() == nil {
		t.Error("answered_at missing")
	}

	listed, err := h.client.ListPermissionRequests(context.Background(), connect.NewRequest(&hairpinv1.ListPermissionRequestsRequest{JobId: j.GetId()}))
	if err != nil {
		t.Fatalf("ListPermissionRequests: %v", err)
	}
	if got := listed.Msg.GetPermissionRequests(); len(got) != 1 || got[0].GetToolName() != "bash" {
		t.Errorf("list = %+v", got)
	}
}

func TestCancelJob(t *testing.T) {
	h := newHarness(t)
	j := h.submit(t, "x")

	resp, err := h.client.CancelJob(context.Background(), connect.NewRequest(&hairpinv1.CancelJobRequest{Id: j.GetId()}))
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if resp.Msg.GetJob().GetStatus() != hairpinv1.JobStatus_JOB_STATUS_CANCELLED {
		t.Errorf("status = %s, want CANCELLED", resp.Msg.GetJob().GetStatus())
	}
	if resp.Msg.GetJob().GetFinishedAt() == nil {
		t.Error("finished_at missing on a cancelled job")
	}
}

func TestListJobsCapsLimit(t *testing.T) {
	h := newHarness(t)
	first := h.submit(t, "one")
	second := h.submit(t, "two")

	resp, err := h.client.ListJobs(context.Background(), connect.NewRequest(&hairpinv1.ListJobsRequest{Limit: 5000}))
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	jobs := resp.Msg.GetJobs()
	if len(jobs) != 2 || jobs[0].GetId() != second.GetId() || jobs[1].GetId() != first.GetId() {
		t.Fatalf("jobs = %+v, want newest first", jobs)
	}

	page, err := h.client.ListJobs(context.Background(), connect.NewRequest(&hairpinv1.ListJobsRequest{Limit: 1}))
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(page.Msg.GetJobs()) != 1 || page.Msg.GetNextPageToken() == "" {
		t.Errorf("first page = %+v, token = %q", page.Msg.GetJobs(), page.Msg.GetNextPageToken())
	}
}

func TestWatchJobStreamsUntilTerminal(t *testing.T) {
	h := newHarness(t)
	j := h.submit(t, "x")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := h.client.WatchJob(ctx, connect.NewRequest(&hairpinv1.WatchJobRequest{Id: j.GetId()}))
	if err != nil {
		t.Fatalf("WatchJob: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	cancelled := make(chan error, 1)
	go func() {
		_, err := h.svc.Cancel(context.Background(), j.GetId())
		cancelled <- err
	}()

	var last *hairpinv1.JobEvent
	for stream.Receive() {
		last = stream.Msg().GetEvent()
	}
	if err := <-cancelled; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream ended with %v", err)
	}
	if last == nil {
		t.Fatal("no events received")
	}
	if last.GetType() != service.EventTypeStatusChange || !strings.Contains(last.GetPayloadJson(), "cancelled") {
		t.Errorf("last event = %+v, want the cancelled status change", last)
	}
	if last.GetAt() == nil || last.GetId() == "" {
		t.Errorf("event lacks id/at: %+v", last)
	}
}

func TestWatchJobNotFound(t *testing.T) {
	h := newHarness(t)
	stream, err := h.client.WatchJob(context.Background(), connect.NewRequest(&hairpinv1.WatchJobRequest{Id: "hp-missing"}))
	if err != nil {
		assertCode(t, err, connect.CodeNotFound)
		return
	}
	t.Cleanup(func() { _ = stream.Close() })
	for stream.Receive() {
	}
	assertCode(t, stream.Err(), connect.CodeNotFound)
}

func TestSubmitJobExplicitRunConfig(t *testing.T) {
	h := newHarness(t)
	raw, err := protojson.Marshal(&harnessv1.RunConfig{
		Mode:     "execution",
		Provider: &harnessv1.ProviderConfig{Type: "anthropic"},
		MaxTurns: 3,
		Timeout:  proto.Int32(30),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := h.client.SubmitJob(context.Background(), connect.NewRequest(&hairpinv1.SubmitJobRequest{
		Prompt:        "explicit",
		RunConfigJson: string(raw),
	}))
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	if resp.Msg.GetJob().GetProfile() != "" {
		t.Errorf("profile = %q, want empty", resp.Msg.GetJob().GetProfile())
	}
	h.svc.WaitForLaunches()
}

func TestConnectErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not found", store.ErrNotFound, connect.CodeNotFound},
		{"invalid", service.ErrInvalidArgument, connect.CodeInvalidArgument},
		{"not connected", service.ErrNotConnected, connect.CodeFailedPrecondition},
		{"other", errors.New("boom"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := connect.CodeOf(connectError(tc.err)); got != tc.want {
				t.Errorf("code = %s, want %s", got, tc.want)
			}
		})
	}
}

func assertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("call succeeded, want %s", want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("code = %s, want %s (err: %v)", got, want, err)
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
