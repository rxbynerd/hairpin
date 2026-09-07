package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
)

func writeProfile(t *testing.T, dir, name string, cfg *harnessv1.RunConfig) {
	t.Helper()
	raw, err := protojson.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

func TestLoadProfiles(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "review", testTemplate())
	writeProfile(t, dir, "execution", testTemplate())
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p, err := LoadProfiles(dir, "review")
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if got := strings.Join(p.Names(), ","); got != "execution,review" {
		t.Errorf("names = %q", got)
	}
	if p.Default() != "review" {
		t.Errorf("default = %q", p.Default())
	}
	cfg, ok := p.Get("review")
	if !ok {
		t.Fatal("review profile missing")
	}
	if cfg.GetMode() != "planning" {
		t.Errorf("mode = %q", cfg.GetMode())
	}
	if _, ok := p.Get("absent"); ok {
		t.Error("Get reported an absent profile")
	}
}

func TestLoadProfilesRejectsMalformedTemplate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"mode":`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadProfiles(dir, "")
	if err == nil {
		t.Fatal("LoadProfiles succeeded on a malformed template")
	}
	if !strings.Contains(err.Error(), "broken.json") {
		t.Errorf("error %q does not name the offending file", err)
	}
}

func TestLoadProfilesRejectsMissingDefault(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "review", testTemplate())
	if _, err := LoadProfiles(dir, "execution"); err == nil {
		t.Fatal("LoadProfiles accepted a default that is not loaded")
	}
}

func TestLoadProfilesEmptyDir(t *testing.T) {
	p, err := LoadProfiles("", "")
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if len(p.Names()) != 0 || p.Default() != "" {
		t.Errorf("unset profiles dir yielded %v / %q", p.Names(), p.Default())
	}
}

func TestProfilesGetReturnsCopy(t *testing.T) {
	p, err := NewProfiles(map[string]*harnessv1.RunConfig{"default": testTemplate()}, "default")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	cfg, _ := p.Get("default")
	cfg.Mode = "execution"
	again, _ := p.Get("default")
	if again.GetMode() != "planning" {
		t.Errorf("template mutated through Get: mode = %q", again.GetMode())
	}
}

func TestLoadProfilesParsesControlPlaneTools(t *testing.T) {
	dir := t.TempDir()
	raw := `{
  "mode": "execution",
  "provider": {"type": "anthropic"},
  "maxTurns": 20,
  "timeout": 600,
  "executor": {"type": "local"},
  "tools": {
    "builtIn": ["read_file", "run_command"],
    "controlPlane": [
      {
        "name": "search_memory",
        "description": "Search knowledge saved by earlier sessions.",
        "inputSchema": {
          "type": "object",
          "properties": {
            "query": {"type": "string"},
            "limit": {"type": "integer"}
          },
          "required": ["query"]
        },
        "timeoutSeconds": 30
      }
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p, err := LoadProfiles(dir, "memory")
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	cfg, ok := p.Get("memory")
	if !ok {
		t.Fatal("memory profile missing")
	}
	tools := cfg.GetTools().GetControlPlane()
	if len(tools) != 1 {
		t.Fatalf("control-plane tools = %v, want one entry", tools)
	}
	if got := tools[0].GetName(); got != "search_memory" {
		t.Errorf("name = %q", got)
	}
	if got := tools[0].GetTimeoutSeconds(); got != 30 {
		t.Errorf("timeout seconds = %d, want 30", got)
	}
	schema := tools[0].GetInputSchema().GetFields()
	if got := schema["type"].GetStringValue(); got != "object" {
		t.Errorf("input schema type = %q, want object", got)
	}
	props := schema["properties"].GetStructValue().GetFields()
	if _, ok := props["query"]; !ok {
		t.Errorf("input schema properties = %v, want a query property", props)
	}
}
