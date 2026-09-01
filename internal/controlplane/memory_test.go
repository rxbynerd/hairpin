package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/memory"
	"github.com/rxbynerd/hairpin/internal/store"
)

// fakeMemory answers memory calls without a Billet. release, when set,
// holds every call until the test closes it.
type fakeMemory struct {
	records []memory.Record
	result  memory.SaveResult
	release chan struct{}

	mu      sync.Mutex
	queries []string
	saves   []string
}

func (f *fakeMemory) Search(_ context.Context, query string, _ int32) ([]memory.Record, error) {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	f.wait()
	return f.records, nil
}

func (f *fakeMemory) Save(_ context.Context, content string, _ memory.Kind) (memory.SaveResult, error) {
	f.mu.Lock()
	f.saves = append(f.saves, content)
	f.mu.Unlock()
	f.wait()
	return f.result, nil
}

func (f *fakeMemory) wait() {
	if f.release != nil {
		<-f.release
	}
}

func (f *fakeMemory) calls() (queries, saves []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...), append([]string(nil), f.saves...)
}

func toolRequest(requestID, tool, input string) *harnessv1.HarnessEvent {
	return &harnessv1.HarnessEvent{
		Type:      evToolResultRequest,
		RequestId: requestID,
		ToolUseId: "tu-" + requestID,
		ToolName:  tool,
		Input:     []byte(input),
	}
}

func decodeControl(t *testing.T, payload string) *harnessv1.ControlEvent {
	t.Helper()
	var ev harnessv1.ControlEvent
	if err := protojson.Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("decode payload %q: %v", payload, err)
	}
	return &ev
}

// waitFor polls until cond holds, covering the window in which a memory
// call is still in flight on its own goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func toolResponses(s *fakeStream) []*harnessv1.ControlEvent {
	var out []*harnessv1.ControlEvent
	for _, ev := range s.controls() {
		if ev.GetType() == ctlToolResultResponse {
			out = append(out, ev)
		}
	}
	return out
}

func TestRunTaskFulfilsMemoryTools(t *testing.T) {
	h, st, _ := testHandler(t)
	mem := &fakeMemory{
		records: []memory.Record{{MemoryID: "m-1", Content: "hairpin proxies memory", Score: 0.5, CreatedAt: "2026-09-01T10:00:00Z"}},
		result:  memory.SaveResult{MemoryID: "m-2", Accepted: true},
	}
	h.memory = mem
	seedJob(t, st, "hp-mem", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-mem"),
		toolRequest("t-1", memory.ToolSearch, `{"query":"proxy","limit":2}`),
		toolRequest("t-2", memory.ToolSave, `{"content":"a fact","kind":"fact"}`),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	// The harness blocks on an async tool result before finishing its
	// turn, so done waits for both answers here too.
	s.onReceive = func(n int) {
		if n == 3 {
			waitFor(t, "both tool results", func() bool { return len(toolResponses(s)) == 2 })
		}
	}
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	queries, saves := mem.calls()
	if len(queries) != 1 || queries[0] != "proxy" {
		t.Errorf("searches = %v, want [proxy]", queries)
	}
	if len(saves) != 1 || saves[0] != "a fact" {
		t.Errorf("saves = %v, want [a fact]", saves)
	}

	want := map[string]string{
		"t-1": `{"records":[{"memory_id":"m-1","content":"hairpin proxies memory","score":0.5,"created_at":"2026-09-01T10:00:00Z"}]}`,
		"t-2": `{"memory_id":"m-2","accepted":true}`,
	}
	for _, resp := range toolResponses(s) {
		if resp.GetIsError().GetValue() {
			t.Errorf("request %q answered with an error: %s", resp.GetRequestId(), resp.GetContent())
		}
		if got := resp.GetContent(); got != want[resp.GetRequestId()] {
			t.Errorf("request %q content = %s, want %s", resp.GetRequestId(), got, want[resp.GetRequestId()])
		}
	}

	waitFor(t, "both responses on the timeline", func() bool {
		return len(eventsOfType(t, st, "hp-mem", ctlToolResultResponse)) == 2
	})
	if got := len(eventsOfType(t, st, "hp-mem", evToolResultRequest)); got != 2 {
		t.Errorf("recorded %d tool_result_request events, want 2", got)
	}
	recorded := decodeControl(t, eventsOfType(t, st, "hp-mem", ctlToolResultResponse)[0].PayloadJSON)
	if recorded.GetRequestId() == "" || recorded.GetContent() == "" {
		t.Errorf("recorded response lost its payload: %+v", recorded)
	}
}

func TestRunTaskRefusesUnknownControlPlaneTool(t *testing.T) {
	h, st, _ := testHandler(t)
	h.memory = &fakeMemory{}
	seedJob(t, st, "hp-unk", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-unk"),
		toolRequest("t-1", "ask_the_operator", `{}`),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	resps := toolResponses(s)
	if len(resps) != 1 {
		t.Fatalf("controls = %v, want a refusal", s.types())
	}
	if !resps[0].GetIsError().GetValue() {
		t.Error("refusal not marked as an error")
	}
	if got := resps[0].GetContent(); got != `hairpin does not fulfil the tool "ask_the_operator"` {
		t.Errorf("content = %q", got)
	}
	if got := len(eventsOfType(t, st, "hp-unk", ctlToolResultResponse)); got != 1 {
		t.Errorf("recorded %d responses, want 1", got)
	}
}

func TestRunTaskRefusesMemoryToolWhenDisabled(t *testing.T) {
	h, st, _ := testHandler(t)
	seedJob(t, st, "hp-off", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-off"),
		toolRequest("t-1", memory.ToolSearch, `{"query":"anything"}`),
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}

	resps := toolResponses(s)
	if len(resps) != 1 {
		t.Fatalf("controls = %v, want a refusal", s.types())
	}
	if !resps[0].GetIsError().GetValue() {
		t.Error("refusal not marked as an error")
	}
	if got := resps[0].GetContent(); got != memoryDisabledRefusal {
		t.Errorf("content = %q, want %q", got, memoryDisabledRefusal)
	}
}

func TestRunTaskKeepsPumpingWhileMemoryCallIsInFlight(t *testing.T) {
	h, st, _ := testHandler(t)
	mem := &fakeMemory{release: make(chan struct{})}
	h.memory = mem
	seedJob(t, st, "hp-slow", job.StatusAwaitingHarness, nil)

	s := newFakeStream(
		ready("hp-slow"),
		toolRequest("t-1", memory.ToolSearch, `{"query":"slow"}`),
		&harnessv1.HarnessEvent{Type: evHeartbeat},
		&harnessv1.HarnessEvent{Type: evDone, StopReason: "success"},
	)
	s.onReceive = func(n int) {
		if n != 3 { // the heartbeat has been handled; the search has not returned
			return
		}
		if len(eventsOfType(t, st, "hp-slow", evHeartbeat)) != 1 {
			t.Error("the heartbeat was not recorded while the memory call was in flight")
		}
		if len(toolResponses(s)) != 0 {
			t.Error("a tool result was sent before the memory call returned")
		}
		close(mem.release)
		waitFor(t, "the released tool result", func() bool { return len(toolResponses(s)) == 1 })
	}

	if err := h.runTask(context.Background(), s); err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if got := toolResponses(s); len(got) != 1 || got[0].GetRequestId() != "t-1" {
		t.Fatalf("controls = %v, want one answer to t-1", s.types())
	}
}

func eventsOfType(t *testing.T, st store.Store, jobID, evType string) []store.Event {
	t.Helper()
	var out []store.Event
	for _, ev := range events(t, st, jobID) {
		if ev.Type == evType {
			out = append(out, ev)
		}
	}
	return out
}
