package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// stubClient answers without a Billet, recording what Fulfil asked for.
type stubClient struct {
	records   []Record
	searchErr error
	result    SaveResult
	saveErr   error

	query   string
	limit   int32
	content string
	kind    Kind
	calls   int
}

func (s *stubClient) Search(_ context.Context, query string, limit int32) ([]Record, error) {
	s.calls++
	s.query, s.limit = query, limit
	return s.records, s.searchErr
}

func (s *stubClient) Save(_ context.Context, content string, kind Kind) (SaveResult, error) {
	s.calls++
	s.content, s.kind = content, kind
	return s.result, s.saveErr
}

func TestIsMemoryTool(t *testing.T) {
	for name, want := range map[string]bool{
		ToolSearch:      true,
		ToolSave:        true,
		"read_file":     false,
		"":              false,
		"SEARCH_MEMORY": false,
	} {
		if got := IsMemoryTool(name); got != want {
			t.Errorf("IsMemoryTool(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestFulfilSearchOutput(t *testing.T) {
	c := &stubClient{records: []Record{
		{MemoryID: "m-1", Content: "hairpin proxies memory", Score: 0.5, CreatedAt: "2026-09-01T10:00:00Z"},
	}}

	content, isError, detail := Fulfil(context.Background(), c, ToolSearch, []byte(`{"query":"memory","limit":3}`))
	if isError || detail != nil {
		t.Fatalf("content = %q, isError = %v, detail = %v", content, isError, detail)
	}
	want := `{"records":[{"memory_id":"m-1","content":"hairpin proxies memory","score":0.5,"created_at":"2026-09-01T10:00:00Z"}]}`
	if content != want {
		t.Errorf("content = %s, want %s", content, want)
	}
	if c.query != "memory" || c.limit != 3 {
		t.Errorf("client called with query %q limit %d", c.query, c.limit)
	}
}

func TestFulfilSearchNoMatches(t *testing.T) {
	content, isError, detail := Fulfil(context.Background(), &stubClient{}, ToolSearch, []byte(`{"query":"nothing"}`))
	if isError || detail != nil {
		t.Fatalf("content = %q, isError = %v, detail = %v", content, isError, detail)
	}
	if content != `{"records":[]}` {
		t.Errorf("content = %s, want an empty records array", content)
	}
}

func TestFulfilSearchAbsentLimitLeftToBillet(t *testing.T) {
	c := &stubClient{}
	if _, isError, _ := Fulfil(context.Background(), c, ToolSearch, []byte(`{"query":"x","limit":-4}`)); isError {
		t.Fatal("a negative limit was rejected instead of being passed on")
	}
	if c.limit != -4 {
		t.Errorf("limit = %d, want the value passed through for Billet to default", c.limit)
	}
}

func TestFulfilSaveOutput(t *testing.T) {
	c := &stubClient{result: SaveResult{MemoryID: "m-7", Accepted: true}}

	content, isError, detail := Fulfil(context.Background(), c, ToolSave, []byte(`{"content":"a fact","kind":"fact"}`))
	if isError || detail != nil {
		t.Fatalf("content = %q, isError = %v, detail = %v", content, isError, detail)
	}
	if content != `{"memory_id":"m-7","accepted":true}` {
		t.Errorf("content = %s", content)
	}
	if c.content != "a fact" || c.kind != KindFact {
		t.Errorf("client called with content %q kind %q", c.content, c.kind)
	}
}

func TestFulfilSearchClampsLimit(t *testing.T) {
	for name, tc := range map[string]struct{ in, want int32 }{
		"above the ceiling": {in: 5000, want: maxSearchLimit},
		"at the ceiling":    {in: maxSearchLimit, want: maxSearchLimit},
		"below the ceiling": {in: 7, want: 7},
		"absent":            {in: 0, want: 0},
		"negative":          {in: -4, want: -4},
	} {
		t.Run(name, func(t *testing.T) {
			c := &stubClient{}
			if _, isError, _ := Fulfil(context.Background(), c,
				ToolSearch, fmt.Appendf(nil, `{"query":"x","limit":%d}`, tc.in)); isError {
				t.Fatal("a well-formed search was rejected")
			}
			if c.limit != tc.want {
				t.Errorf("limit = %d, want %d", c.limit, tc.want)
			}
		})
	}
}

func TestFulfilSaveOmittedKind(t *testing.T) {
	c := &stubClient{}
	if _, isError, _ := Fulfil(context.Background(), c, ToolSave, []byte(`{"content":"a turn"}`)); isError {
		t.Fatal("save without a kind was rejected")
	}
	if c.kind != KindUnspecified {
		t.Errorf("kind = %q, want the unspecified kind", c.kind)
	}
}

func TestFulfilMalformedInput(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"search not an object", ToolSearch, `["memory"]`, `search_memory: input must be a JSON object`},
		{"search not json", ToolSearch, `{"query":`, `search_memory: input is not valid JSON`},
		{"search empty input", ToolSearch, ``, `search_memory: input is not valid JSON`},
		{"search missing query", ToolSearch, `{"limit":3}`, `search_memory: "query" is required`},
		{"search blank query", ToolSearch, `{"query":"  "}`, `search_memory: "query" is required`},
		{"search query wrong type", ToolSearch, `{"query":5}`, `search_memory: "query" has the wrong type (got number)`},
		{"search limit wrong type", ToolSearch, `{"query":"x","limit":"3"}`, `search_memory: "limit" has the wrong type (got string)`},
		{"save not an object", ToolSave, `"a fact"`, `save_memory: input must be a JSON object`},
		{"save missing content", ToolSave, `{"kind":"fact"}`, `save_memory: "content" is required`},
		{"save unknown kind", ToolSave, `{"content":"x","kind":"opinion"}`, `save_memory: "kind" must be "event" or "fact"`},
		{
			"save oversized content",
			ToolSave,
			`{"content":"` + strings.Repeat("a", maxContentBytes+1) + `"}`,
			`save_memory: "content" exceeds the 262144 byte limit`,
		},
		{"save kind wrong type", ToolSave, `{"content":"x","kind":7}`, `save_memory: "kind" has the wrong type (got number)`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &stubClient{}
			content, isError, detail := Fulfil(context.Background(), c, tc.tool, []byte(tc.input))
			if !isError {
				t.Fatalf("malformed input accepted: %s", content)
			}
			if detail != nil {
				t.Errorf("detail = %v, want none for a caller error", detail)
			}
			if content != tc.want {
				t.Errorf("content = %q, want %q", content, tc.want)
			}
			if c.calls != 0 {
				t.Error("malformed input reached Billet")
			}
		})
	}
}

func TestFulfilPassesBilletRejectionsThrough(t *testing.T) {
	for name, code := range map[string]connect.Code{
		"invalid argument":   connect.CodeInvalidArgument,
		"resource exhausted": connect.CodeResourceExhausted,
	} {
		t.Run(name, func(t *testing.T) {
			c := &stubClient{searchErr: connect.NewError(code, errors.New("search_memory: query must not be empty"))}
			content, isError, detail := Fulfil(context.Background(), c, ToolSearch, []byte(`{"query":"x"}`))
			if !isError {
				t.Fatal("a rejected call was reported as success")
			}
			if detail != nil {
				t.Errorf("detail = %v, want none when the message is passed through", detail)
			}
			if content != "search_memory: query must not be empty" {
				t.Errorf("content = %q, want Billet's message verbatim", content)
			}
		})
	}
}

func TestFulfilWithholdsOtherFailures(t *testing.T) {
	for name, err := range map[string]error{
		"unavailable": connect.NewError(connect.CodeUnavailable, errors.New("billet.hairpin.svc:8141: connection refused")),
		"internal":    connect.NewError(connect.CodeInternal, errors.New("bolt: database is locked")),
		"transport":   errors.New("dial tcp: no route to host"),
	} {
		t.Run(name, func(t *testing.T) {
			c := &stubClient{saveErr: err}
			content, isError, detail := Fulfil(context.Background(), c, ToolSave, []byte(`{"content":"x"}`))
			if !isError {
				t.Fatal("a failed call was reported as success")
			}
			if content != GenericFailureMessage {
				t.Errorf("content = %q, want the generic message", content)
			}
			if !errors.Is(detail, err) {
				t.Errorf("detail = %v, want the underlying cause for the log", detail)
			}
		})
	}
}

func TestFulfilUnknownTool(t *testing.T) {
	c := &stubClient{}
	content, isError, detail := Fulfil(context.Background(), c, "read_file", []byte(`{}`))
	if !isError || detail != nil {
		t.Fatalf("content = %q, isError = %v, detail = %v", content, isError, detail)
	}
	if content != `hairpin does not fulfil the tool "read_file"` {
		t.Errorf("content = %q", content)
	}
	if c.calls != 0 {
		t.Error("an unknown tool reached Billet")
	}
}
