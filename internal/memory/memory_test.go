package memory

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	billetv1 "github.com/rxbynerd/billet/gen/billet/v1"
	"github.com/rxbynerd/billet/gen/billet/v1/billetv1connect"
)

// fakeService is an in-process billet.v1.MemoryService, served over
// cleartext HTTP/2 so tests exercise the same wire path as a deployment.
type fakeService struct {
	searchErr error
	saveErr   error

	records []*billetv1.MemoryRecord
	saved   *billetv1.SaveMemoryResponse

	searchReqs []*billetv1.SearchMemoryRequest
	saveReqs   []*billetv1.SaveMemoryRequest
}

func (f *fakeService) SearchMemory(_ context.Context, req *connect.Request[billetv1.SearchMemoryRequest]) (*connect.Response[billetv1.SearchMemoryResponse], error) {
	f.searchReqs = append(f.searchReqs, req.Msg)
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return connect.NewResponse(&billetv1.SearchMemoryResponse{Records: f.records}), nil
}

func (f *fakeService) SaveMemory(_ context.Context, req *connect.Request[billetv1.SaveMemoryRequest]) (*connect.Response[billetv1.SaveMemoryResponse], error) {
	f.saveReqs = append(f.saveReqs, req.Msg)
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	if f.saved == nil {
		return connect.NewResponse(&billetv1.SaveMemoryResponse{}), nil
	}
	return connect.NewResponse(f.saved), nil
}

// serveFake starts the fake on a cleartext HTTP/2 listener and returns a
// client dialling it, mirroring Billet's own rpcserver.Protocols().
func serveFake(t *testing.T, f *fakeService) Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.Handle(billetv1connect.NewMemoryServiceHandler(f))

	srv := httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)

	return NewBilletClient(srv.Listener.Addr().String())
}

func TestBilletClientSearch(t *testing.T) {
	f := &fakeService{records: []*billetv1.MemoryRecord{
		{MemoryId: "m-1", Content: "the deploy needs sudo", Score: 0.9, CreatedAt: "2026-09-01T10:00:00Z"},
		{MemoryId: "m-2", Content: "kind cluster is named hairpin", Score: 0.4, CreatedAt: "2026-09-01T11:00:00Z"},
	}}
	c := serveFake(t, f)

	got, err := c.Search(context.Background(), "deploy", 7)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []Record{
		{MemoryID: "m-1", Content: "the deploy needs sudo", Score: 0.9, CreatedAt: "2026-09-01T10:00:00Z"},
		{MemoryID: "m-2", Content: "kind cluster is named hairpin", Score: 0.4, CreatedAt: "2026-09-01T11:00:00Z"},
	}
	if len(got) != len(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(f.searchReqs) != 1 {
		t.Fatalf("server saw %d searches, want 1", len(f.searchReqs))
	}
	if q := f.searchReqs[0].GetQuery(); q != "deploy" {
		t.Errorf("query = %q, want deploy", q)
	}
	if l := f.searchReqs[0].GetLimit(); l != 7 {
		t.Errorf("limit = %d, want 7", l)
	}
}

func TestBilletClientSearchEmptyResult(t *testing.T) {
	c := serveFake(t, &fakeService{})

	got, err := c.Search(context.Background(), "anything", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("records = %v, want none", got)
	}
}

func TestBilletClientSave(t *testing.T) {
	f := &fakeService{saved: &billetv1.SaveMemoryResponse{MemoryId: "m-9", Accepted: true}}
	c := serveFake(t, f)

	res, err := c.Save(context.Background(), "remember this", KindFact)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if res != (SaveResult{MemoryID: "m-9", Accepted: true}) {
		t.Errorf("result = %+v", res)
	}
	if len(f.saveReqs) != 1 {
		t.Fatalf("server saw %d saves, want 1", len(f.saveReqs))
	}
	if got := f.saveReqs[0].GetKind(); got != billetv1.Kind_KIND_FACT {
		t.Errorf("kind = %v, want KIND_FACT", got)
	}
	if got := f.saveReqs[0].GetContent(); got != "remember this" {
		t.Errorf("content = %q", got)
	}
}

func TestBilletClientSaveKindMapping(t *testing.T) {
	for kind, want := range map[Kind]billetv1.Kind{
		KindUnspecified: billetv1.Kind_KIND_UNSPECIFIED,
		KindEvent:       billetv1.Kind_KIND_EVENT,
		KindFact:        billetv1.Kind_KIND_FACT,
	} {
		f := &fakeService{}
		c := serveFake(t, f)
		if _, err := c.Save(context.Background(), "content", kind); err != nil {
			t.Fatalf("Save(%q): %v", kind, err)
		}
		if got := f.saveReqs[0].GetKind(); got != want {
			t.Errorf("kind %q mapped to %v, want %v", kind, got, want)
		}
	}
}

func TestBilletClientSaveRejectsUnknownKind(t *testing.T) {
	f := &fakeService{}
	c := serveFake(t, f)

	_, err := c.Save(context.Background(), "content", Kind("opinion"))
	if err == nil {
		t.Fatal("Save accepted an unknown kind")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want invalid_argument", got)
	}
	if len(f.saveReqs) != 0 {
		t.Error("unknown kind reached the server")
	}
}

func TestBilletClientPreservesConnectCode(t *testing.T) {
	f := &fakeService{searchErr: connect.NewError(connect.CodeResourceExhausted, errors.New("budget exceeded"))}
	c := serveFake(t, f)

	_, err := c.Search(context.Background(), "anything", 0)
	if err == nil {
		t.Fatal("Search succeeded against a failing server")
	}
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Fatalf("code = %v, want resource_exhausted", got)
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("error %v is not a *connect.Error", err)
	}
	if connectErr.Message() != "budget exceeded" {
		t.Errorf("message = %q, want the server's verbatim message", connectErr.Message())
	}
}

func TestBilletClientUnreachable(t *testing.T) {
	// Port 1 on the loopback address refuses connections, so this
	// exercises the transport failure path without a live server.
	c := NewBilletClient("127.0.0.1:1")

	if _, err := c.Search(context.Background(), "anything", 0); err == nil {
		t.Fatal("Search succeeded with no server listening")
	}
	if _, err := c.Save(context.Background(), "content", KindEvent); err == nil {
		t.Fatal("Save succeeded with no server listening")
	}
}

func TestBilletClientRequiresCleartextHTTP2(t *testing.T) {
	// A server offering only HTTP/1.1 must not be usable: the client's
	// transport speaks cleartext HTTP/2 alone, which is what makes the
	// no-TLS gRPC path work at all.
	mux := http.NewServeMux()
	mux.Handle(billetv1connect.NewMemoryServiceHandler(&fakeService{}))

	srv := httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)

	c := NewBilletClient(srv.Listener.Addr().String())
	if _, err := c.Search(context.Background(), "anything", 0); err == nil {
		t.Error("Search succeeded against an HTTP/1.1-only server")
	}
	if _, err := c.Save(context.Background(), "content", KindEvent); err == nil {
		t.Error("Save succeeded against an HTTP/1.1-only server")
	}
}

func TestBilletTransportKeepsConnectionsChecked(t *testing.T) {
	tr := unencryptedHTTP2Transport()
	if tr.HTTP2 == nil {
		t.Fatal("transport has no HTTP/2 configuration")
	}
	if tr.HTTP2.PingTimeout == 0 || tr.HTTP2.SendPingTimeout == 0 || tr.HTTP2.WriteByteTimeout == 0 {
		t.Errorf("keepalive timeouts unset: %+v", tr.HTTP2)
	}
	if tr.IdleConnTimeout == 0 {
		t.Error("IdleConnTimeout unset: a blackholed connection would be pooled indefinitely")
	}
}

func TestWithCallTimeoutOverridesDefault(t *testing.T) {
	c := NewBilletClient("127.0.0.1:1", WithCallTimeout(time.Millisecond)).(*billetClient)
	if c.timeout != time.Millisecond {
		t.Errorf("timeout = %v, want the override", c.timeout)
	}
	if got := NewBilletClient("127.0.0.1:1", WithCallTimeout(0)).(*billetClient).timeout; got != CallTimeout {
		t.Errorf("timeout = %v, want the default kept for a non-positive override", got)
	}
}
