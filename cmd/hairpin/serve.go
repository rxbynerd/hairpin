package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/redis/go-redis/v9"

	"github.com/rxbynerd/hairpin/gen/hairpin/v1/hairpinv1connect"
	harnessv1 "github.com/rxbynerd/hairpin/gen/harness/v1"
	"github.com/rxbynerd/hairpin/internal/api"
	"github.com/rxbynerd/hairpin/internal/config"
	"github.com/rxbynerd/hairpin/internal/controlplane"
	"github.com/rxbynerd/hairpin/internal/launcher"
	"github.com/rxbynerd/hairpin/internal/memory"
	"github.com/rxbynerd/hairpin/internal/reaper"
	"github.com/rxbynerd/hairpin/internal/registry"
	"github.com/rxbynerd/hairpin/internal/service"
	"github.com/rxbynerd/hairpin/internal/store"
	"github.com/rxbynerd/hairpin/internal/store/redisstore"
	"github.com/rxbynerd/hairpin/internal/telemetry"
	"github.com/rxbynerd/hairpin/internal/tokenissuer"
	"github.com/rxbynerd/hairpin/internal/web"
)

func run(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: hairpin <serve|keygen> [flags]")
	}
	switch args[0] {
	case "serve":
		cfg, err := parseServeFlags(args[1:])
		if err != nil {
			return err
		}
		return serve(cfg)
	case "keygen":
		return runKeygen(args[1:])
	default:
		return fmt.Errorf("usage: hairpin <serve|keygen> [flags]")
	}
}

func parseServeFlags(args []string) (*config.Config, error) {
	cfg := &config.Config{}
	namespace := config.DetectNamespace()

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", ":8130", "h2c listen address (API, control plane, web UI)")
	fs.StringVar(&cfg.AdvertiseAddr, "advertise", "", "address harnesses dial back into (CONTROL_PLANE_ADDR); defaults to hairpin.<namespace>.svc:8130 in-cluster")
	fs.StringVar(&cfg.RedisAddr, "redis", "", "Redis address host:port (empty: in-memory store, dev only)")
	fs.StringVar(&cfg.RedisPassword, "redis-password", "", "Redis password")
	fs.IntVar(&cfg.RedisDB, "redis-db", 0, "Redis database number")
	fs.StringVar(&cfg.BilletAddr, "billet-addr", "", "host:port of Billet's RPC listener, backing the memory tools (empty: memory disabled)")
	fs.StringVar(&cfg.Launcher, "launcher", "kubernetes", "harness launcher: kubernetes or none")
	fs.StringVar(&cfg.ProfilesDir, "profiles", "", "directory of RunConfig profile templates (<name>.json)")
	fs.StringVar(&cfg.DefaultProfile, "default-profile", "default", "profile used when a submit names none")
	fs.DurationVar(&cfg.Retention, "retention", 0, "delete a terminal job's record, timeline, and permissions this long after it finishes (0: keep forever, the current default)")

	fs.StringVar(&cfg.Harness.Namespace, "namespace", namespace, "namespace harness Jobs are created in")
	fs.StringVar(&cfg.Harness.Image, "harness-image", config.DefaultHarnessImage, "stirrup harness image run as the Job")
	fs.StringVar(&cfg.Harness.ServiceAccount, "harness-service-account", "", "ServiceAccount for harness Pods; its token is mounted so the sandbox executor can reach the API")
	fs.StringVar(&cfg.Harness.Kubeconfig, "kubeconfig", "", "kubeconfig path (empty: in-cluster, then $KUBECONFIG)")
	secrets := fs.String("harness-secrets", "", "comma-separated Secret names exposed to harness Pods via envFrom")
	ttl := fs.Int("job-ttl", 3600, "ttlSecondsAfterFinished on created harness Jobs")
	slack := fs.Duration("deadline-slack", 10*time.Minute, "slack added to the RunConfig timeout for Job activeDeadlineSeconds")

	fs.StringVar(&cfg.Sandbox.Image, "sandbox-image", config.DefaultSandboxImage, "image sandbox Pods run; must ship a shell, tar, ls, and mkdir")
	fs.StringVar(&cfg.Sandbox.Namespace, "sandbox-namespace", "", "namespace sandbox Pods are created in (empty: --namespace)")
	fs.StringVar(&cfg.Sandbox.ServiceAccount, "sandbox-service-account", "", "ServiceAccount for sandbox Pods; its token is never mounted")
	fs.StringVar(&cfg.Sandbox.Runtime, "sandbox-runtime", "", "RuntimeClassName for sandbox Pods: runc, gvisor, kata-qemu, kata-fc, kata-clh (empty: cluster default)")

	fs.StringVar(&cfg.HarnessTelemetryEndpoint, "harness-telemetry-endpoint", "", "OTLP/gRPC host:port injected as a submitted RunConfig's trace_emitter when it names none (empty: leave it alone); the harness exports the run's own trace there")

	fs.StringVar(&cfg.Telemetry.Exporter, "telemetry", telemetry.ExporterNone, "OpenTelemetry exporter: none, otlp, or stdout")
	fs.StringVar(&cfg.Telemetry.Protocol, "telemetry-protocol", envOr("OTEL_EXPORTER_OTLP_PROTOCOL", telemetry.ProtocolGRPC), "OTLP transport: grpc or http/protobuf")
	fs.StringVar(&cfg.Telemetry.Endpoint, "telemetry-endpoint", "", "OTLP endpoint URL (empty: OTEL_EXPORTER_OTLP_ENDPOINT, then localhost)")
	fs.Float64Var(&cfg.Telemetry.SampleRatio, "telemetry-sample-ratio", 1, "head-sampling probability for traces hairpin starts, 0 to 1")
	fs.StringVar(&cfg.Telemetry.ServiceName, "telemetry-service-name", "", "service.name reported to the collector (empty: OTEL_SERVICE_NAME, then hairpin)")
	fs.DurationVar(&cfg.Telemetry.MetricInterval, "telemetry-metric-interval", telemetry.DefaultMetricInterval, "how often metrics are exported")

	fs.StringVar(&cfg.SandboxToken.KeyPath, "sandbox-token-key", "", "path to an ES256 (P-256) private key PEM for signing sandbox identity tokens; empty disables issuance (sandbox_token_request is refused)")
	fs.StringVar(&cfg.SandboxToken.Issuer, "sandbox-token-issuer", "", "iss claim on minted sandbox identity tokens; required when -sandbox-token-key is set")
	fs.StringVar(&cfg.SandboxToken.Audience, "sandbox-token-audience", "", "aud claim on minted sandbox identity tokens; required when -sandbox-token-key is set. Always wins over the harness's requested audience.")
	tokenTTL := fs.Duration("sandbox-token-ttl", 15*time.Minute, "TTL of minted sandbox identity tokens")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *secrets != "" {
		cfg.Harness.EnvFromSecrets = splitComma(*secrets)
	}
	if *ttl < 0 || int64(*ttl) > math.MaxInt32 {
		return nil, fmt.Errorf("job TTL must be between 0 and %d seconds", int64(math.MaxInt32))
	}
	cfg.Harness.TTLSecondsAfterFinished = int32(*ttl)
	cfg.Harness.ActiveDeadlineSlack = *slack
	cfg.SandboxToken.TTL = *tokenTTL
	if cfg.AdvertiseAddr == "" && cfg.Harness.Namespace != "" {
		cfg.AdvertiseAddr = defaultAdvertiseAddr(cfg.Harness.Namespace, cfg.ListenAddr)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultAdvertiseAddr names the in-cluster Service the reference
// manifests create, on the port hairpin listens on.
func defaultAdvertiseAddr(namespace, listenAddr string) string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return ""
	}
	return fmt.Sprintf("hairpin.%s.svc:%s", namespace, port)
}

// envOr returns the environment variable named by key, or fallback when
// it is unset or empty.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
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

	tel, shutdownTelemetry, err := telemetry.Setup(context.Background(), cfg.Telemetry, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownTelemetry(context.Background()); err != nil {
			logger.Warn("telemetry shutdown", "error", err)
		}
	}()

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

	// One client decides both halves of the feature: what a submit may
	// declare, and what the control plane will answer. Deriving them
	// separately would let a run pass preflight and then be refused.
	var memoryClient memory.Client
	if cfg.BilletAddr != "" {
		memoryClient = memory.NewBilletClient(cfg.BilletAddr)
		logger.Info("memory tools enabled", "billet_addr", cfg.BilletAddr)
	} else {
		logger.Info("memory tools disabled; submits declaring them are rejected")
	}

	issuer, err := buildTokenIssuer(cfg)
	if err != nil {
		return err
	}
	if issuer == nil {
		logger.Warn("no sandbox token key configured; sandbox_token_request will be refused")
	} else {
		logger.Info("sandbox identity token issuance enabled",
			"issuer", cfg.SandboxToken.Issuer, "audience", cfg.SandboxToken.Audience, "kid", issuer.KeyID())
		if cfg.SandboxToken.TTL > time.Hour {
			logger.Warn("sandbox identity token TTL is longer than an hour; a leaked token stays valid for the whole window",
				"ttl", cfg.SandboxToken.TTL)
		}
	}

	reg := registry.New()
	svc := service.New(st, reg, l, profiles, logger,
		service.WithExecutorDefaults(cfg.Sandbox),
		service.WithHarnessTelemetryEndpoint(cfg.HarnessTelemetryEndpoint),
		service.WithMemoryTools(memoryClient != nil),
		service.WithTelemetry(tel))

	cpOpts := []controlplane.Option{
		controlplane.WithLogger(logger),
		controlplane.WithMemory(memoryClient),
		controlplane.WithTelemetry(tel),
	}
	if issuer != nil {
		cpOpts = append(cpOpts, controlplane.WithSandboxTokenIssuer(issuer))
	}

	cp := controlplane.New(st, reg, cpOpts...)

	// RPC spans and metrics come from the interceptor; hairpin's own
	// spans hang off them. Without an exporter it is left out entirely
	// rather than relying on no-op instruments.
	var rpcOpts []connect.HandlerOption
	if tel != nil {
		// A harness stream carries thousands of text deltas, and one
		// span event each would swamp the trace.
		interceptor, err := otelconnect.NewInterceptor(otelconnect.WithoutTraceEvents())
		if err != nil {
			return fmt.Errorf("build telemetry interceptor: %w", err)
		}
		rpcOpts = append(rpcOpts, connect.WithInterceptors(interceptor))
	}

	// The reaper always runs its awaiting_harness sweep, catching a
	// harness that never dials back regardless of retention settings;
	// its job-deletion sweep is a no-op unless cfg.Retention > 0.
	rp := reaper.New(st, cfg.Retention, cfg.Harness.ActiveDeadlineSlack, logger,
		reaper.WithTelemetry(tel))
	reaperCtx, cancelReaper := context.WithCancel(context.Background())
	defer cancelReaper()
	var reaperWG sync.WaitGroup
	reaperWG.Add(1)
	go func() {
		defer reaperWG.Done()
		rp.Run(reaperCtx)
	}()

	mux := http.NewServeMux()
	cpPath, cpHandler := cp.NewHTTPHandler(rpcOpts...)
	mux.Handle(cpPath, cpHandler)
	// 4 MiB bounds a submit (RunConfigs are small; dynamic context is
	// capped harness-side at 50 KiB per entry) without letting one
	// request balloon memory.
	apiPath, apiHandler := hairpinv1connect.NewJobServiceHandler(api.New(svc),
		append([]connect.HandlerOption{connect.WithReadMaxBytes(4 << 20)}, rpcOpts...)...)
	mux.Handle(apiPath, apiHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	if issuer != nil {
		// haybale's jwksURL must be HTTPS or loopback; the reference
		// deployment ships this as a file instead (see
		// docs/deployment.md#sandbox-identity-tokens). Serving it here
		// still lets a caller inspect the current key over a trusted
		// network, and covers deployments that do front this in HTTPS.
		mux.Handle("/.well-known/jwks.json", issuer.JWKSHandler())
	}
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
		logger.Info("hairpin serving", "listen", cfg.ListenAddr, "advertise", cfg.AdvertiseAddr, "launcher", cfg.Launcher, "retention", cfg.Retention)
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

	// Cancel live runs first: the harness reacts within one turn and
	// emits done{cancelled}, letting the control-plane streams settle
	// their jobs before the listener drains. Without this, restart
	// leaves every in-flight job stuck "running" with no terminal
	// record.
	for _, id := range reg.JobIDs() {
		if err := reg.Send(id, &harnessv1.ControlEvent{Type: "cancel"}); err != nil {
			logger.Warn("failed to cancel live run for shutdown", "job_id", id, "error", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown grace expired with streams still open", "error", err)
	}
	cancelReaper()
	reaperWG.Wait()
	svc.WaitForLaunches()
	cp.WaitForMemoryCalls()
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

// buildTokenIssuer loads the configured signing key and returns a sandbox
// identity token Issuer, or nil when no key is configured — Config.Validate
// already checked issuer/audience/ttl accompany a set key.
func buildTokenIssuer(cfg *config.Config) (*tokenissuer.Issuer, error) {
	if cfg.SandboxToken.KeyPath == "" {
		return nil, nil
	}
	priv, err := tokenissuer.LoadKey(cfg.SandboxToken.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("sandbox token key: %w", err)
	}
	issuer, err := tokenissuer.New(priv, cfg.SandboxToken.Issuer, cfg.SandboxToken.Audience, cfg.SandboxToken.TTL)
	if err != nil {
		return nil, fmt.Errorf("sandbox token issuer: %w", err)
	}
	return issuer, nil
}

func buildLauncher(cfg *config.Config, logger *slog.Logger) (launcher.Launcher, error) {
	switch cfg.Launcher {
	case "kubernetes":
		return launcher.NewK8s(cfg.Harness, cfg.AdvertiseAddr, logger)
	default:
		logger.Warn("launcher disabled; harnesses must be started out-of-band with CONTROL_PLANE_SESSION_ID set to SubmitJob's harness_session")
		return launcher.None{}, nil
	}
}
