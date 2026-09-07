// Package integration exercises the full hairpin loop over real wire
// protocols: a JobService client submits work, a fake harness speaks
// the stirrup RunTask stream (gRPC over unencrypted HTTP/2, exactly as
// `stirrup job` dials), and the caller observes status, events, and
// permissions end to end.
package integration

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	hairpinv1 "github.com/rxbynerd/hairpin/gen/hairpin/v1"
	"github.com/rxbynerd/hairpin/gen/hairpin/v1/hairpinv1connect"
	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/gen/harness/v1/harnessv1connect"
	"github.com/rxbynerd/hairpin/internal/api"
	"github.com/rxbynerd/hairpin/internal/controlplane"
	"github.com/rxbynerd/hairpin/internal/launcher"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/service"
	"github.com/rxbynerd/hairpin/internal/store"
	"github.com/rxbynerd/hairpin/internal/telemetry"
	"github.com/rxbynerd/hairpin/internal/telemetry/telemetrytest"
	"github.com/rxbynerd/hairpin/internal/web"
)

const testRunConfig = `{
	"mode": "review",
	"prompt": "Review the diff.",
	"provider": {"type": "anthropic", "apiKeyRef": "secret://ANTHROPIC_API_KEY"},
	"permissionPolicy": {"type": "ask-upstream"},
	"tools": {"builtIn": ["read_file"]},
	"executor": {"type": "local"},
	"maxTurns": 5,
	"timeout": 300
}`

// startServer wires the same stack cmd/hairpin serves, on an ephemeral
// h2c port.
func startServer(t *testing.T) (baseURL string, httpClient *http.Client) {
	t.Helper()
	return startServerWith(t, nil, nil)
}

// startInstrumentedServer is startServer with telemetry wired exactly
// as cmd/hairpin wires it, plus the collector that reads back what the
// loop measured.
func startInstrumentedServer(t *testing.T) (string, *http.Client, *telemetrytest.Collector) {
	t.Helper()
	rec, collector := telemetrytest.New(t)
	baseURL, client := startServerWith(t, rec, collector)
	return baseURL, client, collector
}

// startServerWith builds the stack around an optional recorder. The
// collector supplies the providers cmd/hairpin installs globally.
func startServerWith(t *testing.T, rec *telemetry.Recorder, collector *telemetrytest.Collector) (string, *http.Client) {
	t.Helper()
	st := store.NewMemStore(0)
	reg := registry.New()
	profiles, err := service.NewProfiles(nil, "default")
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, reg, launcher.None{}, profiles, nil, service.WithTelemetry(rec))

	var rpcOpts []connect.HandlerOption
	if rec != nil {
		interceptor, err := otelconnect.NewInterceptor(
			otelconnect.WithoutTraceEvents(),
			otelconnect.WithTracerProvider(collector.TracerProvider()),
			otelconnect.WithMeterProvider(collector.MeterProvider()),
		)
		if err != nil {
			t.Fatalf("otelconnect.NewInterceptor: %v", err)
		}
		rpcOpts = append(rpcOpts, connect.WithInterceptors(interceptor))
	}

	mux := http.NewServeMux()
	cpPath, cpHandler := controlplane.New(st, reg, controlplane.WithTelemetry(rec)).NewHTTPHandler(rpcOpts...)
	mux.Handle(cpPath, cpHandler)
	apiPath, apiHandler := hairpinv1connect.NewJobServiceHandler(api.New(svc), rpcOpts...)
	mux.Handle(apiPath, apiHandler)
	mux.Handle("/", web.New(svc, nil))

	server := httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server.Config.Protocols = protocols
	server.Start()
	t.Cleanup(server.Close)

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetUnencryptedHTTP2(true)
	return server.URL, &http.Client{Transport: transport}
}

func waitForStatus(t *testing.T, jobs hairpinv1connect.JobServiceClient, id string, want hairpinv1.JobStatus) *hairpinv1.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last *hairpinv1.Job
	for time.Now().Before(deadline) {
		resp, err := jobs.GetJob(context.Background(), connect.NewRequest(&hairpinv1.GetJobRequest{Id: id}))
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		last = resp.Msg.Job
		if last.Status == want {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %v; last status %v (stop_reason %q, error %q)",
		id, want, last.GetStatus(), last.GetStopReason(), last.GetError())
	return nil
}

// TestFullLoop drives submit → harness dial-in → assignment → deltas →
// permission round-trip → done, asserting the caller-visible record at
// each step.
func TestFullLoop(t *testing.T) {
	baseURL, client := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs := hairpinv1connect.NewJobServiceClient(client, baseURL)

	// Submit with an explicit RunConfig (no profiles configured).
	sub, err := jobs.SubmitJob(ctx, connect.NewRequest(&hairpinv1.SubmitJobRequest{
		RunConfigJson: testRunConfig,
	}))
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	jobID := sub.Msg.Job.Id
	session := sub.Msg.HarnessSession
	if !strings.HasPrefix(jobID, "hp-") {
		t.Fatalf("unexpected job id %q", jobID)
	}

	// Launcher "none": the job parks in awaiting_harness for an
	// out-of-band harness — which this test now plays, dialling exactly
	// as `stirrup job` does with CONTROL_PLANE_SESSION_ID echoed in
	// ready.id.
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS)

	harness := harnessv1connect.NewHarnessServiceClient(client, baseURL, connect.WithGRPC())
	stream := harness.RunTask(ctx)
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready", Id: session, HarnessVersion: "test"}); err != nil {
		t.Fatalf("send ready: %v", err)
	}

	assignment, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive assignment: %v", err)
	}
	if assignment.Type != "task_assignment" {
		t.Fatalf("first control event = %q, want task_assignment", assignment.Type)
	}
	if got := assignment.Task.GetRunId(); got != jobID {
		t.Errorf("assigned run_id = %q, want %q", got, jobID)
	}
	if got := assignment.Task.GetPrompt(); got != "Review the diff." {
		t.Errorf("assigned prompt = %q", got)
	}
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_RUNNING)

	// Stream some output, then ask permission.
	for _, text := range []string{"Looking at ", "the diff now."} {
		if err := stream.Send(&harnessv1.HarnessEvent{Type: "text_delta", Text: text}); err != nil {
			t.Fatalf("send delta: %v", err)
		}
	}
	if err := stream.Send(&harnessv1.HarnessEvent{
		Type: "permission_request", RequestId: "perm-1",
		ToolName: "web_fetch", Input: []byte(`{"url":"https://example.com"}`),
	}); err != nil {
		t.Fatalf("send permission_request: %v", err)
	}

	// The permission surfaces on the API, gets answered, and the
	// decision arrives on the stream.
	var pending *hairpinv1.PermissionRequest
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := jobs.ListPermissionRequests(ctx, connect.NewRequest(&hairpinv1.ListPermissionRequestsRequest{JobId: jobID}))
		if err != nil {
			t.Fatalf("ListPermissionRequests: %v", err)
		}
		if prs := resp.Msg.PermissionRequests; len(prs) > 0 {
			pending = prs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pending == nil || pending.State != "pending" {
		t.Fatalf("pending permission not visible: %+v", pending)
	}
	if _, err := jobs.AnswerPermission(ctx, connect.NewRequest(&hairpinv1.AnswerPermissionRequest{
		JobId: jobID, RequestId: "perm-1", Allow: false, Reason: "not needed for this review",
	})); err != nil {
		t.Fatalf("AnswerPermission: %v", err)
	}
	decision, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive permission_response: %v", err)
	}
	if decision.Type != "permission_response" || decision.RequestId != "perm-1" {
		t.Fatalf("got %q/%q, want permission_response/perm-1", decision.Type, decision.RequestId)
	}
	if decision.Allowed.GetValue() {
		t.Error("permission was denied via API but stream says allowed")
	}
	if decision.Reason != "not needed for this review" {
		t.Errorf("reason = %q", decision.Reason)
	}

	// Finish the run and half-close immediately, as a real `stirrup job`
	// does on exit. (Not CloseResponse: that is an RST_STREAM cancel,
	// which may discard the in-flight done frame — a teardown no real
	// harness performs. The write-vs-teardown race is covered
	// deterministically in the controlplane package.)
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "done", StopReason: "success"}); err != nil {
		t.Fatalf("send done: %v", err)
	}
	_ = stream.CloseRequest()
	final := waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_SUCCEEDED)
	if final.StopReason != "success" {
		t.Errorf("stop_reason = %q", final.StopReason)
	}
	if final.FinalText != "Looking at the diff now." {
		t.Errorf("final_text = %q", final.FinalText)
	}

	// WatchJob replays the full timeline and terminates on its own.
	watch, err := jobs.WatchJob(ctx, connect.NewRequest(&hairpinv1.WatchJobRequest{Id: jobID}))
	if err != nil {
		t.Fatalf("WatchJob: %v", err)
	}
	var types []string
	for watch.Receive() {
		types = append(types, watch.Msg().Event.Type)
	}
	if err := watch.Err(); err != nil {
		t.Fatalf("watch ended with error: %v", err)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"status_change", "text_delta", "permission_request", "done"} {
		if !strings.Contains(joined, want) {
			t.Errorf("watch timeline missing %q: %s", want, joined)
		}
	}
}

// TestCancelBeforeHarness cancels a parked job; a harness that dials in
// afterwards is turned away with a cancel control event.
func TestCancelBeforeHarness(t *testing.T) {
	baseURL, client := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs := hairpinv1connect.NewJobServiceClient(client, baseURL)
	sub, err := jobs.SubmitJob(ctx, connect.NewRequest(&hairpinv1.SubmitJobRequest{RunConfigJson: testRunConfig}))
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	jobID := sub.Msg.Job.Id
	session := sub.Msg.HarnessSession
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS)

	if _, err := jobs.CancelJob(ctx, connect.NewRequest(&hairpinv1.CancelJobRequest{Id: jobID})); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_CANCELLED)

	harness := harnessv1connect.NewHarnessServiceClient(client, baseURL, connect.WithGRPC())
	stream := harness.RunTask(ctx)
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready", Id: session, HarnessVersion: "test"}); err != nil {
		t.Fatalf("send ready: %v", err)
	}
	ev, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if ev.Type != "cancel" {
		t.Fatalf("late harness got %q, want cancel", ev.Type)
	}
}

// TestBareJobIDRejected proves the session token is load-bearing: a
// harness presenting only the (guessable) job ID is turned away.
func TestBareJobIDRejected(t *testing.T) {
	baseURL, client := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs := hairpinv1connect.NewJobServiceClient(client, baseURL)
	sub, err := jobs.SubmitJob(ctx, connect.NewRequest(&hairpinv1.SubmitJobRequest{RunConfigJson: testRunConfig}))
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	jobID := sub.Msg.Job.Id
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS)

	harness := harnessv1connect.NewHarnessServiceClient(client, baseURL, connect.WithGRPC())
	for _, id := range []string{jobID, jobID + ".wrong-token"} {
		stream := harness.RunTask(ctx)
		if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready", Id: id, HarnessVersion: "test"}); err != nil {
			t.Fatalf("send ready: %v", err)
		}
		ev, err := stream.Receive()
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		if ev.Type != "cancel" {
			t.Fatalf("harness with session %q got %q, want cancel", id, ev.Type)
		}
		_ = stream.CloseRequest()
	}

	// The job is untouched and still claimable by the real session.
	got, err := jobs.GetJob(ctx, connect.NewRequest(&hairpinv1.GetJobRequest{Id: jobID}))
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Msg.Job.Status != hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS {
		t.Fatalf("job status after rejected claims = %v", got.Msg.Job.Status)
	}
}

// TestCrashSettlement marks a job failed when the stream closes without
// a done event.
func TestCrashSettlement(t *testing.T) {
	baseURL, client := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs := hairpinv1connect.NewJobServiceClient(client, baseURL)
	sub, err := jobs.SubmitJob(ctx, connect.NewRequest(&hairpinv1.SubmitJobRequest{RunConfigJson: testRunConfig}))
	if err != nil {
		t.Fatalf("SubmitJob: %v", err)
	}
	jobID := sub.Msg.Job.Id
	session := sub.Msg.HarnessSession
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_AWAITING_HARNESS)

	harness := harnessv1connect.NewHarnessServiceClient(client, baseURL, connect.WithGRPC())
	stream := harness.RunTask(ctx)
	if err := stream.Send(&harnessv1.HarnessEvent{Type: "ready", Id: session, HarnessVersion: "test"}); err != nil {
		t.Fatalf("send ready: %v", err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatalf("receive assignment: %v", err)
	}
	waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_RUNNING)

	// Harness "crashes": close both directions without done.
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()

	final := waitForStatus(t, jobs, jobID, hairpinv1.JobStatus_JOB_STATUS_FAILED)
	if final.Error == "" {
		t.Error("crashed job carries no error detail")
	}
}
