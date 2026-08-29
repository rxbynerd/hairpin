package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/rxbynerd/hairpin/internal/config"
)

func run(args []string) error {
	if len(args) < 1 || args[0] != "serve" {
		return fmt.Errorf("usage: hairpin serve [flags]")
	}

	cfg, err := parseServeFlags(args[1:])
	if err != nil {
		return err
	}
	return serve(cfg)
}

func parseServeFlags(args []string) (*config.Config, error) {
	cfg := &config.Config{}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", ":8130", "h2c listen address (API, control plane, web UI)")
	fs.StringVar(&cfg.AdvertiseAddr, "advertise", "", "address harnesses dial back into (CONTROL_PLANE_ADDR)")
	fs.StringVar(&cfg.RedisAddr, "redis", "", "Redis address host:port (empty: in-memory store, dev only)")
	fs.StringVar(&cfg.RedisPassword, "redis-password", "", "Redis password")
	fs.IntVar(&cfg.RedisDB, "redis-db", 0, "Redis database number")
	fs.StringVar(&cfg.Launcher, "launcher", "none", "harness launcher: k8s, process, or none")
	fs.StringVar(&cfg.ProfilesDir, "profiles", "", "directory of RunConfig profile templates (<name>.json)")
	fs.StringVar(&cfg.DefaultProfile, "default-profile", "default", "profile used when a submit names none")
	fs.StringVar(&cfg.Process.StirrupBin, "stirrup-bin", "", "process launcher: path to the stirrup binary")
	fs.StringVar(&cfg.Process.WorkDir, "stirrup-workdir", "", "process launcher: harness working directory (empty: per-job temp dir)")
	fs.BoolVar(&cfg.Process.InheritEnv, "stirrup-inherit-env", true, "process launcher: pass hairpin's environment to the harness")
	fs.StringVar(&cfg.K8s.Namespace, "k8s-namespace", "", "k8s launcher: namespace for harness Jobs")
	fs.StringVar(&cfg.K8s.Image, "k8s-image", "", "k8s launcher: stirrup container image")
	fs.StringVar(&cfg.K8s.Kubeconfig, "k8s-kubeconfig", "", "k8s launcher: kubeconfig path (empty: in-cluster, then $KUBECONFIG)")
	fs.StringVar(&cfg.K8s.ServiceAccount, "k8s-service-account", "", "k8s launcher: ServiceAccount for harness Pods")
	envFromSecrets := fs.String("k8s-env-from-secrets", "", "k8s launcher: comma-separated Secret names exposed to harness Pods via envFrom")
	ttl := fs.Int("k8s-job-ttl", 3600, "k8s launcher: Job ttlSecondsAfterFinished")
	slack := fs.Duration("k8s-deadline-slack", 10*time.Minute, "k8s launcher: slack added to RunConfig timeout for Job activeDeadlineSeconds")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *envFromSecrets != "" {
		cfg.K8s.EnvFromSecrets = splitComma(*envFromSecrets)
	}
	cfg.K8s.TTLSecondsAfterFinished = int32(*ttl)
	cfg.K8s.ActiveDeadlineSlack = *slack
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func splitComma(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// serve wires the store, registry, launcher, control plane, API, and
// web UI onto one h2c listener. Implemented during integration.
func serve(cfg *config.Config) error {
	return fmt.Errorf("serve: not wired yet (listen=%s)", cfg.ListenAddr)
}
