package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/memory"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

func objectSchema(t *testing.T) *structpb.Struct {
	t.Helper()
	schema, err := structpb.NewStruct(map[string]any{
		"type":       "object",
		"properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required":   []any{"query"},
	})
	if err != nil {
		t.Fatalf("build input schema: %v", err)
	}
	return schema
}

func controlPlaneTool(t *testing.T, name string) *harnessv1.ControlPlaneToolConfig {
	t.Helper()
	return &harnessv1.ControlPlaneToolConfig{
		Name:        name,
		Description: "a tool the control plane answers",
		InputSchema: objectSchema(t),
	}
}

// toolsTemplate is a runnable profile declaring tools as its
// control-plane surface.
func toolsTemplate(t *testing.T, tools ...*harnessv1.ControlPlaneToolConfig) *harnessv1.RunConfig {
	t.Helper()
	cfg := testTemplate()
	if len(tools) > 0 {
		cfg.Tools = &harnessv1.ToolsConfig{
			BuiltIn:      []string{"read_file"},
			ControlPlane: tools,
		}
	}
	return cfg
}

func newToolsService(t *testing.T, memoryEnabled bool, tmpl *harnessv1.RunConfig) *Service {
	t.Helper()
	profiles, err := NewProfiles(map[string]*harnessv1.RunConfig{"default": tmpl}, "default")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	return New(st, registry.New(), &fakeLauncher{}, profiles, slog.New(slog.DiscardHandler),
		WithMemoryTools(memoryEnabled))
}

func TestSubmitControlPlaneToolPreflight(t *testing.T) {
	cases := []struct {
		name          string
		tools         []*harnessv1.ControlPlaneToolConfig
		memoryEnabled bool
		wantErr       string
	}{
		{
			name:          "no control-plane tools",
			memoryEnabled: false,
		},
		{
			name:          "memory tools with memory enabled",
			tools:         []*harnessv1.ControlPlaneToolConfig{controlPlaneTool(t, memory.ToolSearch), controlPlaneTool(t, memory.ToolSave)},
			memoryEnabled: true,
		},
		{
			name:          "memory tool with memory disabled",
			tools:         []*harnessv1.ControlPlaneToolConfig{controlPlaneTool(t, memory.ToolSearch)},
			memoryEnabled: false,
			wantErr:       "billet-addr",
		},
		{
			name:          "foreign tool name",
			tools:         []*harnessv1.ControlPlaneToolConfig{controlPlaneTool(t, "ask_the_operator")},
			memoryEnabled: true,
			wantErr:       `"ask_the_operator"`,
		},
		{
			name:          "foreign tool alongside a memory tool",
			tools:         []*harnessv1.ControlPlaneToolConfig{controlPlaneTool(t, memory.ToolSave), controlPlaneTool(t, "ask_the_operator")},
			memoryEnabled: true,
			wantErr:       `"ask_the_operator"`,
		},
	}

	// The same RunConfig reaches Submit from a profile or straight from
	// the caller; the preflight must see both.
	sources := map[string]func(t *testing.T, tmpl *harnessv1.RunConfig) (*harnessv1.RunConfig, SubmitParams){
		"profile": func(_ *testing.T, tmpl *harnessv1.RunConfig) (*harnessv1.RunConfig, SubmitParams) {
			return tmpl, SubmitParams{Prompt: "do the thing"}
		},
		"run_config_json": func(t *testing.T, tmpl *harnessv1.RunConfig) (*harnessv1.RunConfig, SubmitParams) {
			t.Helper()
			cfg := tmpl
			cfg.Prompt = "do the thing"
			raw, err := protojson.Marshal(cfg)
			if err != nil {
				t.Fatalf("marshal run config: %v", err)
			}
			// The service's own profile carries no tools, so only the
			// submitted config can trip the preflight.
			return testTemplate(), SubmitParams{RunConfigJSON: string(raw)}
		},
	}

	for _, tc := range cases {
		for source, build := range sources {
			t.Run(tc.name+"/"+source, func(t *testing.T) {
				tmpl, params := build(t, toolsTemplate(t, tc.tools...))
				svc := newToolsService(t, tc.memoryEnabled, tmpl)

				j, err := svc.Submit(context.Background(), params)
				svc.WaitForLaunches()

				if tc.wantErr == "" {
					if err != nil {
						t.Fatalf("Submit: %v", err)
					}
					if j == nil {
						t.Fatal("Submit returned no job")
					}
					return
				}
				if err == nil {
					t.Fatal("Submit accepted an unfulfillable control-plane tool")
				}
				if !errors.Is(err, ErrInvalidArgument) {
					t.Errorf("error %v is not ErrInvalidArgument", err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
			})
		}
	}
}

func TestSubmitRejectedToolsPersistNoJob(t *testing.T) {
	svc := newToolsService(t, false, toolsTemplate(t, controlPlaneTool(t, memory.ToolSearch)))

	if _, err := svc.Submit(context.Background(), SubmitParams{Prompt: "do the thing"}); err == nil {
		t.Fatal("Submit accepted a memory tool with memory disabled")
	}
	jobs, _, err := svc.store.ListJobs(context.Background(), 10, "")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("rejected submit persisted %d jobs", len(jobs))
	}
}

func TestSubmitKeepsControlPlaneToolDeclarations(t *testing.T) {
	svc := newToolsService(t, true, toolsTemplate(t, controlPlaneTool(t, memory.ToolSearch)))

	j, err := svc.Submit(context.Background(), SubmitParams{Prompt: "do the thing"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	svc.WaitForLaunches()

	var cfg harnessv1.RunConfig
	if err := protojson.Unmarshal([]byte(j.RunConfigJSON), &cfg); err != nil {
		t.Fatalf("unmarshal persisted run config: %v", err)
	}
	tools := cfg.GetTools().GetControlPlane()
	if len(tools) != 1 || tools[0].GetName() != memory.ToolSearch {
		t.Fatalf("persisted control-plane tools = %v", tools)
	}
	if got := tools[0].GetInputSchema().GetFields()["type"].GetStringValue(); got != "object" {
		t.Errorf("persisted input schema type = %q, want object", got)
	}
}
