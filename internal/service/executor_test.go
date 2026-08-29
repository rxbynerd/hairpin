package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/config"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/store"
)

func sandboxDefaults() config.ExecutorDefaults {
	return config.ExecutorDefaults{
		Image:          "ghcr.io/rxbynerd/stirrup-sandbox:latest",
		Namespace:      "hairpin-sandboxes",
		ServiceAccount: "stirrup-sandbox",
		Runtime:        "gvisor",
	}
}

// sandboxTemplate is a caller-shaped profile: it names the isolation it
// wants and leaves the cluster coordinates to the server.
func sandboxTemplate() *harnessv1.RunConfig {
	tmpl := testTemplate()
	tmpl.Executor = &harnessv1.ExecutorConfig{
		Type:    "k8s",
		Network: &harnessv1.NetworkConfig{Mode: "none"},
	}
	return tmpl
}

// newSandboxService builds a Service whose default profile carries cfg
// as its executor, with the sandbox defaults applied.
func newSandboxService(t *testing.T, tmpl *harnessv1.RunConfig) *Service {
	t.Helper()
	profiles, err := NewProfiles(map[string]*harnessv1.RunConfig{"default": tmpl}, "default")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	return New(st, registry.New(), &fakeLauncher{}, profiles, slog.New(slog.DiscardHandler),
		WithExecutorDefaults(sandboxDefaults()))
}

// submittedExecutor submits against svc and returns the executor as it
// was persisted.
func submittedExecutor(t *testing.T, svc *Service) *harnessv1.ExecutorConfig {
	t.Helper()
	j, err := svc.Submit(context.Background(), SubmitParams{Prompt: "do the thing"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	svc.WaitForLaunches()
	var cfg harnessv1.RunConfig
	if err := protojson.Unmarshal([]byte(j.RunConfigJSON), &cfg); err != nil {
		t.Fatalf("unmarshal persisted run config: %v", err)
	}
	return cfg.GetExecutor()
}

func TestSubmitFillsSandboxCoordinates(t *testing.T) {
	ex := submittedExecutor(t, newSandboxService(t, sandboxTemplate()))

	d := sandboxDefaults()
	if ex.GetImage() != d.Image {
		t.Errorf("image = %q, want %q", ex.GetImage(), d.Image)
	}
	if ex.GetK8SNamespace() != d.Namespace {
		t.Errorf("k8sNamespace = %q, want %q", ex.GetK8SNamespace(), d.Namespace)
	}
	if ex.GetK8SServiceAccount() != d.ServiceAccount {
		t.Errorf("k8sServiceAccount = %q, want %q", ex.GetK8SServiceAccount(), d.ServiceAccount)
	}
	if ex.GetRuntime() != d.Runtime {
		t.Errorf("runtime = %q, want %q", ex.GetRuntime(), d.Runtime)
	}
}

func TestSubmitKeepsExplicitSandboxCoordinates(t *testing.T) {
	tmpl := sandboxTemplate()
	tmpl.Executor.Image = "example.com/custom:v1"
	tmpl.Executor.K8SNamespace = "tenant-a"

	ex := submittedExecutor(t, newSandboxService(t, tmpl))

	if ex.GetImage() != "example.com/custom:v1" {
		t.Errorf("image = %q, want the profile's own", ex.GetImage())
	}
	if ex.GetK8SNamespace() != "tenant-a" {
		t.Errorf("k8sNamespace = %q, want the profile's own", ex.GetK8SNamespace())
	}
	if ex.GetK8SServiceAccount() != sandboxDefaults().ServiceAccount {
		t.Errorf("k8sServiceAccount = %q, want the unset field to be defaulted", ex.GetK8SServiceAccount())
	}
}

func TestSubmitLeavesNonSandboxExecutorsAlone(t *testing.T) {
	ex := submittedExecutor(t, newSandboxService(t, testTemplate()))

	if ex.GetType() != "local" {
		t.Fatalf("type = %q, want local", ex.GetType())
	}
	if ex.GetImage() != "" || ex.GetK8SNamespace() != "" || ex.GetRuntime() != "" {
		t.Errorf("sandbox defaults leaked onto a local executor: %+v", ex)
	}
}

func TestSubmitRejectsUnrunnableSandboxConfigs(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*harnessv1.ExecutorConfig)
		want string
	}{
		{
			name: "no network mode",
			mut:  func(ex *harnessv1.ExecutorConfig) { ex.Network = nil },
			want: "executor.network is required",
		},
		{
			name: "workspace on a pod-backed executor",
			mut:  func(ex *harnessv1.ExecutorConfig) { ex.Workspace = "/src" },
			want: "executor.workspace is not valid",
		},
		{
			name: "allowlist without an egress proxy",
			mut: func(ex *harnessv1.ExecutorConfig) {
				ex.Network = &harnessv1.NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.anthropic.com"}}
			},
			want: "executor.k8sEgressProxyUrl is required",
		},
		{
			name: "egress proxy without allowlist mode",
			mut:  func(ex *harnessv1.ExecutorConfig) { ex.K8SEgressProxyUrl = "http://egress:3128" },
			want: "executor.k8sEgressProxyUrl is only valid",
		},
		{
			name: "non-gvisor runtime on the CRD-provisioned sandbox",
			mut: func(ex *harnessv1.ExecutorConfig) {
				ex.Type = "k8s-sandbox"
				ex.Runtime = "kata-qemu"
			},
			want: "executor.runtime must be",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := sandboxTemplate()
			tc.mut(tmpl.Executor)
			svc := newSandboxService(t, tmpl)

			_, err := svc.Submit(context.Background(), SubmitParams{Prompt: "do the thing"})
			if err == nil {
				t.Fatal("Submit succeeded, want an error")
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("error %v is not ErrInvalidArgument", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A server with no sandbox image configured must refuse a Pod-backed
// run rather than launch a harness that fails at executor construction.
func TestSubmitRejectsSandboxRunWithoutDefaults(t *testing.T) {
	profiles, err := NewProfiles(map[string]*harnessv1.RunConfig{"default": sandboxTemplate()}, "default")
	if err != nil {
		t.Fatalf("NewProfiles: %v", err)
	}
	st := store.NewMemStore(0)
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st, registry.New(), &fakeLauncher{}, profiles, slog.New(slog.DiscardHandler))

	_, err = svc.Submit(context.Background(), SubmitParams{Prompt: "x"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "executor.image is required") {
		t.Errorf("error %q does not name the missing image", err)
	}
}

// The k8s-sandbox executor draws on the same defaults as k8s.
func TestSubmitFillsCRDProvisionedSandbox(t *testing.T) {
	tmpl := sandboxTemplate()
	tmpl.Executor.Type = "k8s-sandbox"
	tmpl.Timeout = proto.Int32(600)

	ex := submittedExecutor(t, newSandboxService(t, tmpl))

	if ex.GetK8SNamespace() != sandboxDefaults().Namespace {
		t.Errorf("k8sNamespace = %q, want the default", ex.GetK8SNamespace())
	}
	if ex.GetRuntime() != "gvisor" {
		t.Errorf("runtime = %q, want gvisor", ex.GetRuntime())
	}
}
