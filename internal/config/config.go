// Package config holds hairpin's runtime configuration, populated from
// flags and environment by cmd/hairpin.
package config

import (
	"fmt"
	"time"
)

// Config drives one hairpin server process.
type Config struct {
	// ListenAddr is the h2c listen address serving the harness gRPC
	// control plane, the JobService API, and the web UI (default
	// ":8130").
	ListenAddr string

	// AdvertiseAddr is the address launched harnesses dial back into —
	// the value placed in CONTROL_PLANE_ADDR. For the k8s launcher this
	// is typically "hairpin.<namespace>.svc:8130"; for the process
	// launcher "127.0.0.1:8130".
	AdvertiseAddr string

	// RedisAddr selects the Redis store ("host:port"). Empty selects the
	// in-memory store (dev only: state dies with the process).
	RedisAddr string
	// RedisPassword and RedisDB complete the Redis connection.
	RedisPassword string
	RedisDB       int

	// Launcher selects how harnesses start: "k8s", "process", or "none".
	Launcher string

	// ProfilesDir holds named RunConfig templates: <name>.json files in
	// protobuf-JSON form. A submit without an explicit run_config_json
	// resolves against one of these.
	ProfilesDir string
	// DefaultProfile is used when a submit names no profile.
	DefaultProfile string

	// Process launcher settings.
	Process ProcessConfig
	// K8s launcher settings.
	K8s K8sConfig
}

// ProcessConfig configures the local subprocess launcher.
type ProcessConfig struct {
	// StirrupBin is the path to the stirrup binary.
	StirrupBin string
	// WorkDir is the working directory harness processes run in; empty
	// uses a per-job temp dir.
	WorkDir string
	// InheritEnv passes the hairpin process environment through to the
	// harness (API keys etc.). Defaults to true for dev ergonomics.
	InheritEnv bool
}

// K8sConfig configures the Kubernetes Job launcher.
type K8sConfig struct {
	// Namespace the Jobs are created in.
	Namespace string
	// Image is the stirrup container image.
	Image string
	// Kubeconfig path; empty prefers in-cluster config, then $KUBECONFIG.
	Kubeconfig string
	// ServiceAccount for the harness Pod; empty uses the namespace default.
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

// Validate rejects configurations that cannot serve.
func (c *Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen address is required")
	}
	if c.AdvertiseAddr == "" {
		return fmt.Errorf("advertise address is required (harnesses must know where to dial back)")
	}
	switch c.Launcher {
	case "k8s":
		if c.K8s.Namespace == "" {
			return fmt.Errorf("k8s launcher requires a namespace")
		}
		if c.K8s.Image == "" {
			return fmt.Errorf("k8s launcher requires an image")
		}
	case "process":
		if c.Process.StirrupBin == "" {
			return fmt.Errorf("process launcher requires the stirrup binary path")
		}
	case "none":
	default:
		return fmt.Errorf("unknown launcher %q (want k8s, process, or none)", c.Launcher)
	}
	return nil
}
