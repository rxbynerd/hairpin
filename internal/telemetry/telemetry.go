// Package telemetry configures hairpin's OpenTelemetry pipeline and
// records the service's own spans and metrics: submission, launch,
// harness stream lifecycle, outcomes, and latency.
//
// Export is opt-in. With no exporter configured Setup installs nothing
// and returns a nil *Recorder, which every recording method accepts, so
// a development server carries no telemetry cost and needs no collector.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Exporter selects where spans and metrics go.
const (
	// ExporterNone installs no SDK: the default, and what a local
	// development server wants.
	ExporterNone = "none"
	// ExporterOTLP exports over OTLP, configured by Options and the
	// standard OTEL_EXPORTER_OTLP_* environment variables.
	ExporterOTLP = "otlp"
	// ExporterStdout writes spans and metrics to stderr, for inspecting
	// instrumentation without a collector.
	ExporterStdout = "stdout"
)

// OTLP transport protocols.
const (
	ProtocolGRPC = "grpc"
	ProtocolHTTP = "http/protobuf"
)

// DefaultServiceName is the service.name reported when neither Options
// nor OTEL_SERVICE_NAME names one.
const DefaultServiceName = "hairpin"

// DefaultMetricInterval is how often the SDK exports metrics.
const DefaultMetricInterval = 60 * time.Second

// shutdownTimeout bounds each provider's flush on shutdown.
const shutdownTimeout = 5 * time.Second

// Options configures the telemetry pipeline.
type Options struct {
	// Exporter is "none", "otlp", or "stdout".
	Exporter string
	// Protocol is the OTLP transport: "grpc" or "http/protobuf".
	Protocol string
	// Endpoint overrides OTEL_EXPORTER_OTLP_ENDPOINT. It is a URL
	// ("http://collector:4317"); an https scheme selects TLS.
	Endpoint string
	// SampleRatio is the head-sampling probability for traces started
	// by hairpin, between 0 and 1. Sampling is parent-based: a sampled
	// caller's trace is always followed.
	SampleRatio float64
	// ServiceName overrides OTEL_SERVICE_NAME and DefaultServiceName.
	ServiceName string
	// MetricInterval is the metric export period.
	MetricInterval time.Duration
}

// Enabled reports whether o asks for any export.
func (o Options) Enabled() bool {
	return o.Exporter != "" && o.Exporter != ExporterNone
}

// Validate rejects options that cannot build a pipeline.
func (o Options) Validate() error {
	switch o.Exporter {
	case "", ExporterNone, ExporterOTLP, ExporterStdout:
	default:
		return fmt.Errorf("unknown telemetry exporter %q (want none, otlp, or stdout)", o.Exporter)
	}
	switch o.Protocol {
	case "", ProtocolGRPC, ProtocolHTTP:
	default:
		return fmt.Errorf("unknown telemetry protocol %q (want %s or %s)", o.Protocol, ProtocolGRPC, ProtocolHTTP)
	}
	if o.SampleRatio < 0 || o.SampleRatio > 1 {
		return fmt.Errorf("telemetry sample ratio must be between 0 and 1, got %v", o.SampleRatio)
	}
	if o.MetricInterval < 0 {
		return errors.New("telemetry metric interval must be non-negative")
	}
	return nil
}

// Shutdown flushes and stops the pipeline. It is safe to call once.
type Shutdown func(context.Context) error

// Setup installs the global tracer provider, meter provider, and
// propagator, and returns a Recorder bound to them. With export
// disabled it installs nothing and returns a nil Recorder and a
// no-op Shutdown.
func Setup(ctx context.Context, o Options, logger *slog.Logger) (*Recorder, Shutdown, error) {
	if err := o.Validate(); err != nil {
		return nil, noopShutdown, err
	}
	if !o.Enabled() {
		return nil, noopShutdown, nil
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	res, err := newResource(ctx, o.ServiceName)
	if err != nil {
		return nil, noopShutdown, err
	}

	spanExporter, err := newSpanExporter(ctx, o)
	if err != nil {
		return nil, noopShutdown, err
	}
	metricExporter, err := newMetricExporter(ctx, o)
	if err != nil {
		_ = spanExporter.Shutdown(ctx)
		return nil, noopShutdown, err
	}

	interval := o.MetricInterval
	if interval <= 0 {
		interval = DefaultMetricInterval
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(o.SampleRatio))),
	)
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(interval))),
	)

	// Export failures are an operational problem, not the run's: they
	// are logged rather than surfaced to the code being instrumented.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("telemetry export error", "error", err)
	}))
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	rec, err := New(tracerProvider, meterProvider)
	if err != nil {
		_ = shutdownProviders(ctx, tracerProvider, meterProvider)
		return nil, noopShutdown, err
	}

	logger.Info("telemetry enabled",
		"exporter", o.Exporter,
		"protocol", protocolOrDefault(o.Protocol),
		"sample_ratio", o.SampleRatio,
		"metric_interval", interval)

	return rec, func(ctx context.Context) error {
		return shutdownProviders(ctx, tracerProvider, meterProvider)
	}, nil
}

func noopShutdown(context.Context) error { return nil }

func shutdownProviders(ctx context.Context, tp *sdktrace.TracerProvider, mp *sdkmetric.MeterProvider) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
}

func protocolOrDefault(protocol string) string {
	if protocol == "" {
		return ProtocolGRPC
	}
	return protocol
}

// newResource describes this process. Explicit attributes come first so
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES, applied last, win.
func newResource(ctx context.Context, serviceName string) (*resource.Resource, error) {
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion()),
			semconv.ServiceInstanceID(instanceID()),
		),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
		resource.WithProcessPID(),
		resource.WithFromEnv(),
	)
	// A partial resource (an unresolvable hostname, a malformed
	// OTEL_RESOURCE_ATTRIBUTES entry) is still worth exporting with.
	if res != nil && errors.Is(err, resource.ErrPartialResource) {
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("build telemetry resource: %w", err)
	}
	return res, nil
}

// serviceVersion reports the module version this binary was built from,
// or "dev" for a build with no version stamp.
func serviceVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

// instanceID distinguishes replicas. In Kubernetes the Pod name is the
// natural identity; off-cluster the hostname is.
func instanceID() string {
	if pod := strings.TrimSpace(os.Getenv("POD_NAME")); pod != "" {
		return pod
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}

func newSpanExporter(ctx context.Context, o Options) (sdktrace.SpanExporter, error) {
	switch o.Exporter {
	case ExporterStdout:
		return stdouttrace.New(stdouttrace.WithWriter(os.Stderr))
	default:
		if o.Protocol == ProtocolHTTP {
			var opts []otlptracehttp.Option
			if o.Endpoint != "" {
				opts = append(opts, otlptracehttp.WithEndpointURL(o.Endpoint))
			}
			return otlptracehttp.New(ctx, opts...)
		}
		var opts []otlptracegrpc.Option
		if o.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(o.Endpoint))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
}

func newMetricExporter(ctx context.Context, o Options) (sdkmetric.Exporter, error) {
	switch o.Exporter {
	case ExporterStdout:
		return stdoutmetric.New(stdoutmetric.WithWriter(os.Stderr))
	default:
		if o.Protocol == ProtocolHTTP {
			var opts []otlpmetrichttp.Option
			if o.Endpoint != "" {
				opts = append(opts, otlpmetrichttp.WithEndpointURL(o.Endpoint))
			}
			return otlpmetrichttp.New(ctx, opts...)
		}
		var opts []otlpmetricgrpc.Option
		if o.Endpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(o.Endpoint))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
}
