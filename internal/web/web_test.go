package web

import (
	"bufio"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestJob(id string, status job.Status) *job.Job {
	return &job.Job{
		ID:        id,
		Status:    status,
		Prompt:    "do the thing",
		Profile:   "default",
		CreatedAt: time.Now().Add(-time.Minute),
	}
}

func TestIndexRendersJobs(t *testing.T) {
	svc := newFakeService()
	svc.addJob(newTestJob("hp-00000000000000000000000001", job.StatusRunning))
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "hp-00000000000000000000000001") {
		t.Errorf("body missing job ID:\n%s", body)
	}
	if !strings.Contains(body, "do the thing") {
		t.Errorf("body missing prompt:\n%s", body)
	}
	if !strings.Contains(body, "badge-running") {
		t.Errorf("body missing running badge class:\n%s", body)
	}
}

func TestSubmitRedirectsOnSuccess(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	form := url.Values{"prompt": {"hello there"}, "profile": {"default"}}
	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/jobs/") {
		t.Errorf("Location = %q, want prefix /jobs/", loc)
	}
}

func TestSubmitErrorRerendersWithMessage(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	// No prompt, no run_config_json: rejected before the service is called.
	form := url.Values{"profile": {"default"}}
	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "prompt or run config JSON is required") {
		t.Errorf("body missing validation message:\n%s", w.Body.String())
	}
}

func TestSubmitServiceErrorRerendersWithMessage(t *testing.T) {
	svc := newFakeService()
	svc.submitErr = errStub("profile not found")
	h := New(svc, testLogger())

	form := url.Values{"prompt": {"hello"}}
	req := httptest.NewRequest(http.MethodPost, "/jobs", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "profile not found") {
		t.Errorf("body missing service error message:\n%s", w.Body.String())
	}
	// The submitted prompt is preserved in the re-rendered form.
	if !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("body missing preserved prompt:\n%s", w.Body.String())
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

func TestDetailShowsPermissions(t *testing.T) {
	svc := newFakeService()
	id := "hp-00000000000000000000000002"
	svc.addJob(newTestJob(id, job.StatusRunning))
	svc.setPermissions(id, []store.PermissionRequest{
		{RequestID: "req-1", ToolName: "shell", InputJSON: `{"cmd":"ls"}`, State: store.PermissionPending, RequestedAt: time.Now()},
		{RequestID: "req-2", ToolName: "write_file", State: store.PermissionAllowed, RequestedAt: time.Now(), AnsweredAt: time.Now()},
	})
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/jobs/"+id, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"shell", "write_file", "req-1", `{&#34;cmd&#34;:&#34;ls&#34;}`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestDetailUnknownJobReturns404(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/jobs/hp-00000000000000000000000099", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDetailInvalidIDReturns400(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/jobs/not-a-valid-id", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestPermissionPostCallsService(t *testing.T) {
	svc := newFakeService()
	id := "hp-00000000000000000000000003"
	svc.addJob(newTestJob(id, job.StatusRunning))
	h := New(svc, testLogger())

	form := url.Values{"allow": {"false"}, "reason": {"looks risky"}}
	req := httptest.NewRequest(http.MethodPost, "/jobs/"+id+"/permissions/req-9", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", w.Code, w.Body.String())
	}
	if len(svc.answered) != 1 {
		t.Fatalf("answered count = %d, want 1", len(svc.answered))
	}
	got := svc.answered[0]
	if got.jobID != id || got.requestID != "req-9" || got.allow != false || got.reason != "looks risky" {
		t.Errorf("answered = %+v, unexpected", got)
	}
}

func TestCancelPost(t *testing.T) {
	svc := newFakeService()
	id := "hp-00000000000000000000000004"
	svc.addJob(newTestJob(id, job.StatusRunning))
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodPost, "/jobs/"+id+"/cancel", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", w.Code, w.Body.String())
	}
	if svc.jobs[id].Status != job.StatusCancelled {
		t.Errorf("job status = %s, want cancelled", svc.jobs[id].Status)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/does/not/exist", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	for _, path := range []string{"/static/style.css", "/static/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, w.Code)
		}
	}
}

func TestSSEStreamsEventsAndTerminatesOnClose(t *testing.T) {
	svc := newFakeService()
	id := "hp-00000000000000000000000005"
	svc.addJob(newTestJob(id, job.StatusRunning))
	svc.events[id] = []store.Event{
		{ID: "1-1", Type: "text_delta", PayloadJSON: `{"text":"hi"}`, At: time.Now()},
	}
	h := New(svc, testLogger())

	srv := httptest.NewServer(h)
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL + "/jobs/" + id + "/events")
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Push a live event, then close the watcher to simulate the job
	// reaching a terminal status.
	go func() {
		svc.pushEvent(id, store.Event{ID: "1-2", Type: "done", PayloadJSON: `{"stop_reason":"success"}`, At: time.Now()})
		svc.closeWatchers(id)
	}()

	scanner := bufio.NewScanner(resp.Body)
	var sawHistory, sawLive, sawEOF bool
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.Contains(line, "text_delta"):
			sawHistory = true
		case strings.Contains(line, "event: done"):
			sawLive = true
		case strings.Contains(line, "event: eof"):
			sawEOF = true
		}
		if sawEOF {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning SSE stream: %v", err)
	}
	if !sawHistory {
		t.Error("did not see history event (text_delta)")
	}
	if !sawLive {
		t.Error("did not see live event (done)")
	}
	if !sawEOF {
		t.Error("did not see terminating eof event")
	}
}

func TestSSEUnknownJobReturns404(t *testing.T) {
	svc := newFakeService()
	h := New(svc, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/jobs/hp-00000000000000000000000099/events", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}
