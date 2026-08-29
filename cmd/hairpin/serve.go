package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rxbynerd/hairpin/gen/hairpin/v1/hairpinv1connect"
	"github.com/rxbynerd/hairpin/internal/api"
	"github.com/rxbynerd/hairpin/internal/config"
	"github.com/rxbynerd/hairpin/internal/controlplane"
	"github.com/rxbynerd/hairpin/internal/launcher"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/service"
	"github.com/rxbynerd/hairpin/internal/store"
	"github.com/rxbynerd/hairpin/internal/store/redisstore"
	"github.com/rxbynerd/hairpin/internal/web"
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
// web UI onto one h2c listener.
func serve(cfg *config.Config) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	st, err := buildStore(cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	profiles, err := service.LoadProfiles(cfg.ProfilesDir, cfg.DefaultProfile)
	if err != nil {
		return err
	}
	if cfg.ProfilesDir == "" {
		logger.Warn("no profiles directory configured; submits must carry run_config_json")
	}

	l, err := buildLauncher(cfg, logger)
	if err != nil {
		return err
	}

	reg := registry.New()
	svc := service.New(st, reg, l, profiles, logger)

	mux := http.NewServeMux()
	cpPath, cpHandler := controlplane.New(st, reg, controlplane.WithLogger(logger)).NewHTTPHandler()
	mux.Handle(cpPath, cpHandler)
	apiPath, apiHandler := hairpinv1connect.NewJobServiceHandler(api.New(svc))
	mux.Handle(apiPath, apiHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/", web.New(svc, logger))

	// The harness dials with plaintext gRPC (HTTP/2 prior knowledge), so
	// the listener must speak unencrypted HTTP/2 alongside HTTP/1 for
	// the web UI. Trusted-network posture: see docs/design.md.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Addr:      cfg.ListenAddr,
		Handler:   mux,
		Protocols: protocols,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("hairpin serving", "listen", cfg.ListenAddr, "advertise", cfg.AdvertiseAddr, "launcher", cfg.Launcher)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("forced shutdown", "error", err)
	}
	svc.WaitForLaunches()
	return nil
}

func buildStore(cfg *config.Config, logger *slog.Logger) (store.Store, error) {
	if cfg.RedisAddr == "" {
		logger.Warn("using in-memory store; all state is lost on restart")
		return store.NewMemStore(0), nil
	}
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("redis at %s: %w", cfg.RedisAddr, err)
	}
	return redisstore.New(client, redisstore.Options{}), nil
}

func buildLauncher(cfg *config.Config, logger *slog.Logger) (launcher.Launcher, error) {
	switch cfg.Launcher {
	case "k8s":
		return launcher.NewK8s(cfg.K8s, cfg.AdvertiseAddr, logger)
	case "process":
		return launcher.NewProcess(cfg.Process, cfg.AdvertiseAddr, logger), nil
	default:
		logger.Warn("launcher disabled; harnesses must be started out-of-band with CONTROL_PLANE_SESSION_ID set to the job ID")
		return launcher.None{}, nil
	}
}
