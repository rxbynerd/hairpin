// Package telemetrytest provides a telemetry.Recorder backed by
// in-memory readers, so tests can assert what hairpin measured.
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/hairpin/internal/telemetry"
)

// Collector reads back the spans and metrics a Recorder produced.
type Collector struct {
	reader *sdkmetric.ManualReader
	spans  *tracetest.InMemoryExporter
	tp     *sdktrace.TracerProvider
	mp     *sdkmetric.MeterProvider
}

// TracerProvider and MeterProvider expose the pipeline behind the
// Recorder, for instrumentation that takes providers of its own rather
// than reading the globals.
func (c *Collector) TracerProvider() trace.TracerProvider { return c.tp }

func (c *Collector) MeterProvider() metric.MeterProvider { return c.mp }

// New returns a Recorder writing into an in-memory pipeline, along with
// the Collector that reads it.
func New(t *testing.T) (*telemetry.Recorder, *Collector) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	rec, err := telemetry.New(tp, mp)
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	return rec, &Collector{reader: reader, spans: spans, tp: tp, mp: mp}
}

// Sum totals the int64 series recorded for a counter or up-down
// counter, counting only series carrying every attribute in match. A
// metric that was never recorded sums to zero.
func (c *Collector) Sum(t *testing.T, name string, match ...attribute.KeyValue) int64 {
	t.Helper()
	var total int64
	for _, m := range c.metrics(t, name) {
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("metric %s is %T, want an int64 sum", name, m.Data)
		}
		for _, dp := range sum.DataPoints {
			if matches(dp.Attributes, match) {
				total += dp.Value
			}
		}
	}
	return total
}

// Count returns how many values a histogram recorded on series
// carrying every attribute in match.
func (c *Collector) Count(t *testing.T, name string, match ...attribute.KeyValue) uint64 {
	t.Helper()
	var total uint64
	for _, m := range c.metrics(t, name) {
		hist, ok := m.Data.(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("metric %s is %T, want a float64 histogram", name, m.Data)
		}
		for _, dp := range hist.DataPoints {
			if matches(dp.Attributes, match) {
				total += dp.Count
			}
		}
	}
	return total
}

// Attrs returns the attribute sets recorded for name, letting a test
// assert that an attribute is absent.
func (c *Collector) Attrs(t *testing.T, name string) []attribute.Set {
	t.Helper()
	var out []attribute.Set
	for _, m := range c.metrics(t, name) {
		switch data := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, dp := range data.DataPoints {
				out = append(out, dp.Attributes)
			}
		case metricdata.Histogram[float64]:
			for _, dp := range data.DataPoints {
				out = append(out, dp.Attributes)
			}
		default:
			t.Fatalf("metric %s has unexpected data %T", name, m.Data)
		}
	}
	return out
}

// Spans returns the spans finished so far.
func (c *Collector) Spans(t *testing.T) tracetest.SpanStubs {
	t.Helper()
	if err := c.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush spans: %v", err)
	}
	return c.spans.GetSpans()
}

// Span returns the one finished span with the given name.
func (c *Collector) Span(t *testing.T, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range c.Spans(t) {
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d spans named %q, want exactly one", len(found), name)
	}
	return found[0]
}

func (c *Collector) metrics(t *testing.T, name string) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := c.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var out []metricdata.Metrics
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				out = append(out, m)
			}
		}
	}
	return out
}

func matches(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		got, ok := set.Value(kv.Key)
		if !ok || got != kv.Value {
			return false
		}
	}
	return true
}
