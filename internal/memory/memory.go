// Package memory proxies the harness's control-plane memory tools to
// Billet's billet.v1.MemoryService. The harness never reaches Billet
// itself: a tool_result_request arrives on the RunTask stream, hairpin
// calls Billet over cleartext gRPC, and the answer travels back down the
// same stream (docs/memory.md).
package memory

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"

	billetv1 "github.com/rxbynerd/billet/gen/billet/v1"
	"github.com/rxbynerd/billet/gen/billet/v1/billetv1connect"
)

// CallTimeout bounds one Billet call, well inside stirrup's per-call
// async tool timeout so the harness hears a refusal rather than timing
// out on a wedged Billet.
const CallTimeout = 10 * time.Second

// Kind hints how Billet should treat a saved memory. The empty kind
// lets Billet apply its own default.
type Kind string

const (
	KindUnspecified Kind = ""
	KindEvent       Kind = "event"
	KindFact        Kind = "fact"
)

// Record is one memory returned by a search, best matches first.
type Record struct {
	MemoryID  string
	Content   string
	Score     float64
	CreatedAt string // RFC 3339, as Billet reports it.
}

// SaveResult is the outcome of saving one memory. Acceptance does not
// imply the memory is searchable yet: Billet may index asynchronously.
type SaveResult struct {
	MemoryID string
	Accepted bool
}

// Client is the subset of Billet's MemoryService hairpin proxies.
// Errors are returned unwrapped so callers can inspect them with
// connect.CodeOf. Implementations must respect the context's deadline:
// callers bound each call with CallTimeout, and a call that outlives it
// holds a fulfilment goroutine open past the run it belongs to.
type Client interface {
	// Search returns memories matching query. A limit of zero or less
	// leaves Billet's default in force.
	Search(ctx context.Context, query string, limit int32) ([]Record, error)
	// Save persists one memory.
	Save(ctx context.Context, content string, kind Kind) (SaveResult, error)
}

// Option configures a Billet client.
type Option func(*billetClient)

// WithCallTimeout overrides the per-call timeout.
func WithCallTimeout(d time.Duration) Option {
	return func(c *billetClient) {
		if d > 0 {
			c.timeout = d
		}
	}
}

type billetClient struct {
	svc     billetv1connect.MemoryServiceClient
	timeout time.Duration
}

// NewBilletClient returns a Client for the Billet RPC listener at addr
// ("host:port"), dialled with plaintext gRPC over HTTP/2 prior
// knowledge. Nothing is dialled until the first call.
func NewBilletClient(addr string, opts ...Option) Client {
	c := &billetClient{timeout: CallTimeout}
	for _, o := range opts {
		o(c)
	}
	c.svc = billetv1connect.NewMemoryServiceClient(
		&http.Client{Transport: unencryptedHTTP2Transport()},
		"http://"+addr,
		connect.WithGRPC(),
	)
	return c
}

// unencryptedHTTP2Transport speaks HTTP/2 without TLS, which is what
// Billet's RPC listener serves for gRPC clients.
func unencryptedHTTP2Transport() *http.Transport {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	t := &http.Transport{}
	t.Protocols = protocols
	return t
}

func (c *billetClient) Search(ctx context.Context, query string, limit int32) ([]Record, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.svc.SearchMemory(ctx, connect.NewRequest(&billetv1.SearchMemoryRequest{
		Query: query,
		Limit: limit,
	}))
	if err != nil {
		return nil, err
	}

	msg := resp.Msg.GetRecords()
	records := make([]Record, 0, len(msg))
	for _, r := range msg {
		records = append(records, Record{
			MemoryID:  r.GetMemoryId(),
			Content:   r.GetContent(),
			Score:     r.GetScore(),
			CreatedAt: r.GetCreatedAt(),
		})
	}
	return records, nil
}

func (c *billetClient) Save(ctx context.Context, content string, kind Kind) (SaveResult, error) {
	k, err := protoKind(kind)
	if err != nil {
		return SaveResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.svc.SaveMemory(ctx, connect.NewRequest(&billetv1.SaveMemoryRequest{
		Content: content,
		Kind:    k,
	}))
	if err != nil {
		return SaveResult{}, err
	}
	return SaveResult{MemoryID: resp.Msg.GetMemoryId(), Accepted: resp.Msg.GetAccepted()}, nil
}

func protoKind(kind Kind) (billetv1.Kind, error) {
	switch kind {
	case KindUnspecified:
		return billetv1.Kind_KIND_UNSPECIFIED, nil
	case KindEvent:
		return billetv1.Kind_KIND_EVENT, nil
	case KindFact:
		return billetv1.Kind_KIND_FACT, nil
	default:
		return billetv1.Kind_KIND_UNSPECIFIED,
			connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("save_memory: kind %q is not %q or %q", kind, KindEvent, KindFact))
	}
}
