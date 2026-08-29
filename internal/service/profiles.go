package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
)

// Profiles is a set of named RunConfig templates a submit can be
// resolved against. Templates are parsed at load time so a malformed
// profile fails the server's start-up rather than a caller's submit.
type Profiles struct {
	templates map[string]*harnessv1.RunConfig
	def       string
}

// LoadProfiles reads <name>.json protobuf-JSON RunConfig templates from
// dir. An empty dir yields an empty set, in which case every submit
// must carry its own run_config_json. defaultProfile names the template
// used by submits that name none, and must exist unless empty.
func LoadProfiles(dir, defaultProfile string) (*Profiles, error) {
	if dir == "" {
		return NewProfiles(nil, defaultProfile)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read profiles dir %s: %w", dir, err)
	}
	templates := make(map[string]*harnessv1.RunConfig)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read profile %s: %w", path, err)
		}
		var cfg harnessv1.RunConfig
		if err := protojson.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parse profile %s: %w", path, err)
		}
		templates[strings.TrimSuffix(e.Name(), ".json")] = &cfg
	}
	return NewProfiles(templates, defaultProfile)
}

// NewProfiles builds a profile set from in-memory templates.
func NewProfiles(templates map[string]*harnessv1.RunConfig, defaultProfile string) (*Profiles, error) {
	p := &Profiles{templates: make(map[string]*harnessv1.RunConfig, len(templates)), def: defaultProfile}
	for name, cfg := range templates {
		p.templates[name] = cfg
	}
	if defaultProfile != "" {
		if _, ok := p.templates[defaultProfile]; !ok {
			return nil, fmt.Errorf("default profile %q is not among the %d loaded profiles", defaultProfile, len(p.templates))
		}
	}
	return p, nil
}

// Default is the profile name used when a submit names none; empty when
// no default is configured.
func (p *Profiles) Default() string { return p.def }

// Names returns the profile names in sorted order.
func (p *Profiles) Names() []string {
	names := make([]string, 0, len(p.templates))
	for name := range p.templates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Get returns a deep copy of the named template, so callers may mutate
// it freely.
func (p *Profiles) Get(name string) (*harnessv1.RunConfig, bool) {
	cfg, ok := p.templates[name]
	if !ok {
		return nil, false
	}
	return proto.Clone(cfg).(*harnessv1.RunConfig), true
}
