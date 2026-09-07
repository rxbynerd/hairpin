package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
)

// ToolSearch and ToolSave are the only control-plane tool names hairpin
// fulfils. A profile declares them in its RunConfig; hairpin matches on
// the name alone.
const (
	ToolSearch = "search_memory"
	ToolSave   = "save_memory"
)

// Billet's own request limits, mirrored here so hairpin does not depend
// on an unauthenticated service for its resource bounds and so an
// oversized save is named as the caller's mistake rather than surfacing
// Billet's transport limit.
const (
	maxSearchLimit  = 100
	maxContentBytes = 256 << 10
)

// GenericFailureMessage is all a model learns about a Billet failure it
// did not cause. The detail reaches hairpin's log instead, because the
// model has no use for it and it may describe hairpin's own deployment.
const GenericFailureMessage = "memory is unavailable"

// UnsupportedToolMessage is the refusal for a tool hairpin will not
// answer. Callers use the same wording whether the tool is unknown or
// merely undeclared by the run, so a harness learns nothing about the
// deployment from the difference.
func UnsupportedToolMessage(name string) string {
	return fmt.Sprintf("hairpin does not fulfil the tool %q", name)
}

// IsMemoryTool reports whether name is a tool hairpin fulfils.
func IsMemoryTool(name string) bool {
	return name == ToolSearch || name == ToolSave
}

// Fulfil answers one memory tool call. content and isError are what the
// harness is told; detail is non-nil only when the model was given
// GenericFailureMessage and carries the withheld cause for the caller's
// log.
func Fulfil(ctx context.Context, c Client, toolName string, input []byte) (content string, isError bool, detail error) {
	switch toolName {
	case ToolSearch:
		return fulfilSearch(ctx, c, input)
	case ToolSave:
		return fulfilSave(ctx, c, input)
	default:
		return UnsupportedToolMessage(toolName), true, nil
	}
}

type searchInput struct {
	Query string `json:"query"`
	Limit int32  `json:"limit"`
}

type recordOutput struct {
	MemoryID  string  `json:"memory_id"`
	Content   string  `json:"content"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

type searchOutput struct {
	Records []recordOutput `json:"records"`
}

func fulfilSearch(ctx context.Context, c Client, input []byte) (string, bool, error) {
	var in searchInput
	if err := decodeInput(ToolSearch, input, &in); err != nil {
		return err.Error(), true, nil
	}
	if strings.TrimSpace(in.Query) == "" {
		return ToolSearch + `: "query" is required`, true, nil
	}
	// Zero and below stay Billet's to default; only the ceiling is
	// hairpin's, because an unbounded limit inflates the response, the
	// control stream frame, and the timeline entry at once.
	if in.Limit > maxSearchLimit {
		in.Limit = maxSearchLimit
	}

	records, err := c.Search(ctx, in.Query, in.Limit)
	if err != nil {
		return callFailure(err)
	}

	out := searchOutput{Records: make([]recordOutput, 0, len(records))}
	for _, r := range records {
		out.Records = append(out.Records, recordOutput(r))
	}
	return encodeOutput(out)
}

type saveInput struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

type saveOutput struct {
	MemoryID string `json:"memory_id"`
	Accepted bool   `json:"accepted"`
}

func fulfilSave(ctx context.Context, c Client, input []byte) (string, bool, error) {
	var in saveInput
	if err := decodeInput(ToolSave, input, &in); err != nil {
		return err.Error(), true, nil
	}
	if strings.TrimSpace(in.Content) == "" {
		return ToolSave + `: "content" is required`, true, nil
	}
	if len(in.Content) > maxContentBytes {
		return fmt.Sprintf("%s: %q exceeds the %d byte limit", ToolSave, "content", maxContentBytes), true, nil
	}
	kind := Kind(in.Kind)
	switch kind {
	case KindUnspecified, KindEvent, KindFact:
	default:
		return fmt.Sprintf("%s: %q must be %q or %q", ToolSave, "kind", KindEvent, KindFact), true, nil
	}

	res, err := c.Save(ctx, in.Content, kind)
	if err != nil {
		return callFailure(err)
	}
	return encodeOutput(saveOutput(res))
}

// decodeInput rejects anything that is not a JSON object shaped like
// dst, naming the offending field so the model can correct its call.
func decodeInput(tool string, input []byte, dst any) error {
	err := json.Unmarshal(input, dst)
	if err == nil {
		return nil
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fmt.Errorf("%s: %q has the wrong type (got %s)", tool, typeErr.Field, typeErr.Value)
	}
	if errors.As(err, &typeErr) {
		return fmt.Errorf("%s: input must be a JSON object", tool)
	}
	return fmt.Errorf("%s: input is not valid JSON", tool)
}

func encodeOutput(v any) (string, bool, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return GenericFailureMessage, true, err
	}
	return string(out), false, nil
}

// callFailure applies Billet's error policy: a rejection the caller can
// act on is passed through verbatim, anything else is withheld.
func callFailure(err error) (string, bool, error) {
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeResourceExhausted:
		var connectErr *connect.Error
		if errors.As(err, &connectErr) {
			return connectErr.Message(), true, nil
		}
		return err.Error(), true, nil
	default:
		return GenericFailureMessage, true, err
	}
}
