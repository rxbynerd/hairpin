// Package config holds hairpin's runtime configuration, populated from
// flags and environment by cmd/hairpin.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// DefaultHarnessImage and DefaultSandboxImage are the published stirrup
// container images: the harness image runs `stirrup job` as the Job
// hairpin launches, and the sandbox image is what that harness runs
// agent commands in. They are distinct builds — the harness image is
// distroless and ships no shell, which the executors' exec contract
// requires.
const (
	DefaultHarnessImage = "ghcr.io/rxbynerd/stirrup:latest"
	DefaultSandboxImage = "ghcr.io/rxbynerd/stirrup-sandbox:latest"
)

// namespaceFile is the projected ServiceAccount namespace every Pod
// carries; reading it lets an in-cluster hairpin discover the namespace
// it is running in.
const namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Config drives one hairpin server process.
type Config struct {
	// ListenAddr is the h2c listen address serving the harness gRPC
	// control plane, the JobService API, and the web UI (default
	// ":8130").
	ListenAddr string

	// AdvertiseAddr is the address launched harnesses dial back into —
	// the value placed in CONTROL_PLANE_ADDR, typically
	// "hairpin.<namespace>.svc:8130".
	AdvertiseAddr string

	// RedisAddr selects the Redis store ("host:port"). Empty selects the
	// in-memory store (dev only: state dies with the process).
	RedisAddr string
	// RedisPassword and RedisDB complete the Redis connection.
	RedisPassword string
	RedisDB       int

	// Launcher selects how harnesses start: "kubernetes" (a batch/v1
	// Job per run) or "none" (started out-of-band).
	Launcher string

	// ProfilesDir holds named RunConfig templates: <name>.json files in
	// protobuf-JSON form. A submit without an explicit run_config_json
	// resolves against one of these.
	ProfilesDir string
	// DefaultProfile is used when a submit names no profile.
	DefaultProfile string

	// Harness configures the Jobs hairpin creates.
	Harness HarnessConfig
	// Sandbox supplies the cluster coordinates a submitted RunConfig's
	// Kubernetes executor inherits when it does not name its own.
	Sandbox ExecutorDefaults
}

// HarnessConfig configures the batch/v1 Job hairpin creates per run.
type HarnessConfig struct {
	// Namespace the harness Jobs are created in.
	Namespace string
	// Image is the stirrup harness container image.
	Image string
	// Kubeconfig path; empty prefers in-cluster config, then $KUBECONFIG.
	Kubeconfig string
	// ServiceAccount for the harness Pod. It needs the sandbox RBAC the
	// Kubernetes executor exercises (pods, pods/exec, networkpolicies),
	// and its token is mounted into the Pod so the executor can
	// authenticate; empty uses the namespace default and mounts no
	// token.
	ServiceAccount string
	// EnvFromSecrets lists Secret names exposed to the harness Pod via
	// envFrom, carrying provider API keys referenced as secret://NAME in
	// RunConfigs.
	EnvFromSecrets []string
	// TTLSecondsAfterFinished on the created Job (default 3600).
	TTLSecondsAfterFinished int32
	// ActiveDeadlineSlack is added to the RunConfig timeout to form the
	// Job's activeDeadlineSeconds, covering the pre-assignment wait (up
	// to 5 minutes), setup, and teardown (default 10m).
	ActiveDeadlineSlack time.Duration
	// Labels added to created Jobs and Pods, alongside hairpin's own.
	Labels map[string]string
}

// ExecutorDefaults fills in the cluster coordinates of a submitted
// RunConfig's "k8s" or "k8s-sandbox" executor. Callers name the
// isolation they want; the operator running hairpin owns where and as
// what it runs.
type ExecutorDefaults struct {
	// Image the sandbox Pod runs. Must ship a POSIX shell, tar, ls, and
	// mkdir — the harness drives file I/O through pods/exec.
	Image string
	// Namespace the sandbox Pod and its NetworkPolicy are created in.
	Namespace string
	// ServiceAccount for the sandbox Pod; empty uses the namespace
	// default. Its token is never automounted.
	ServiceAccount string
	// Runtime is the sandbox Pod's RuntimeClassName ("", "runc",
	// "gvisor", "kata-qemu", "kata-fc", "kata-clh"). Empty selects the
	// cluster default runtime.
	Runtime string
}

// DetectNamespace returns the namespace hairpin is running in, read
// from its projected ServiceAccount. It returns "" off-cluster.
func DetectNamespace() string {
	raw, err := os.ReadFile(namespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// Validate rejects configurations that cannot serve.
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen address is required")
	}
	if c.AdvertiseAddr == "" {
		return fmt.Errorf("advertise address is required (harnesses must know where to dial back)")
	}
	if c.RedisDB < 0 {
		return fmt.Errorf("redis database number must be non-negative")
	}
	if c.Harness.TTLSecondsAfterFinished < 0 {
		return fmt.Errorf("job TTL must be non-negative")
	}
	if c.Harness.ActiveDeadlineSlack < 0 {
		return fmt.Errorf("deadline slack must be non-negative")
	}
	switch c.Sandbox.Runtime {
	case "", "runc", "gvisor", "kata-qemu", "kata-fc", "kata-clh":
	default:
		return fmt.Errorf("unknown sandbox runtime %q", c.Sandbox.Runtime)
	}
	switch c.Launcher {
	case "kubernetes":
		if c.Harness.Namespace == "" {
			return fmt.Errorf("kubernetes launcher requires a namespace")
		}
		if c.Harness.Image == "" {
			return fmt.Errorf("kubernetes launcher requires a harness image")
		}
	case "none":
	default:
		return fmt.Errorf("unknown launcher %q (want kubernetes or none)", c.Launcher)
	}
	if c.Sandbox.Namespace == "" {
		c.Sandbox.Namespace = c.Harness.Namespace
	}
	return nil
}
