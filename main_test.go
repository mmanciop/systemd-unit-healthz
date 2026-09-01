package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/mmanciop/systemd-unit-healthz/internal/config"
	"github.com/mmanciop/systemd-unit-healthz/internal/telemetry"
	"github.com/mmanciop/systemd-unit-healthz/internal/unitprobe"
)

type fakeProber struct{ calls int }

func (f *fakeProber) Probe(_ context.Context, units []string) []unitprobe.Status {
	f.calls++
	out := make([]unitprobe.Status, 0, len(units))
	for _, u := range units {
		out = append(out, unitprobe.Status{
			Name:        u,
			ActiveState: "active",
			SubState:    "running",
			ActiveSince: time.Now().Add(-time.Hour),
		})
	}
	return out
}

func (f *fakeProber) Close() error { return nil }

// probeHarness installs SDK providers that record in memory, so a test can ask
// what telemetry a probe actually produced.
func probeHarness(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder, *sdkmetric.ManualReader, *telemetry.Telemetry, *config.Config, *fakeProber) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	// telemetry.Setup builds its instruments from the global providers, so
	// installing them first is what makes the recorders see anything.
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		_ = tp.Shutdown(context.Background())
	})

	tel, err := telemetry.Setup(context.Background(), "", slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatalf("telemetry.Setup: %v", err)
	}

	cfg := &config.Config{
		Units:        []string{"minecraft.service"},
		ProbeTimeout: config.Duration(time.Second),
	}
	return tp, recorder, reader, tel, cfg, &fakeProber{}
}

func metricNames(t *testing.T, reader *sdkmetric.ManualReader) map[string]bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	return names
}

// The background sampler has no caller, so a span for it would be a CLIENT
// span at the root of its own trace, once per tick. It still has to record
// metrics -- that is the entire reason the sampler exists.
func TestUntracedProbeRecordsMetricsWithoutSpans(t *testing.T) {
	_, recorder, reader, tel, cfg, prober := probeHarness(t)

	probe := instrumentedProbe(cfg, prober, tel, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), false)
	probe(context.Background())

	if prober.calls != 1 {
		t.Fatalf("prober called %d times, want 1", prober.calls)
	}
	if spans := recorder.Ended(); len(spans) != 0 {
		t.Errorf("untraced probe produced %d span(s), want none: %v", len(spans), spans[0].Name())
	}

	names := metricNames(t, reader)
	for _, want := range []string{"systemd.unit.active", "systemd.unit.state", "systemd.unit.uptime"} {
		if !names[want] {
			t.Errorf("untraced probe did not record %s; recorded: %v", want, names)
		}
	}
}

// A request-driven probe belongs in the trace of the request that caused it.
func TestTracedProbeEmitsOneClientSpan(t *testing.T) {
	_, recorder, _, tel, cfg, prober := probeHarness(t)

	probe := instrumentedProbe(cfg, prober, tel, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), true)
	probe(context.Background())

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("traced probe produced %d spans, want 1", len(spans))
	}

	span := spans[0]
	if got, want := span.Name(), "org.freedesktop.systemd1.Manager/GetUnitProperties"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
	if got := span.SpanKind(); got != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", got)
	}

	// The rpc.* attributes are what classify this as a call to something,
	// rather than an unlabelled internal span.
	attrs := map[string]string{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["rpc.system"] != "dbus" {
		t.Errorf("rpc.system = %q, want dbus", attrs["rpc.system"])
	}
	if attrs["systemd.unit.name"] == "" {
		t.Error("probe results did not reach the span attributes")
	}
}

// The span, when there is one, has to be a child of whatever called in --
// otherwise the health request and the probe it triggers land in two
// unconnected traces.
func TestTracedProbeNestsUnderTheCaller(t *testing.T) {
	tp, recorder, _, tel, cfg, prober := probeHarness(t)

	ctx, parent := tp.Tracer("test").Start(context.Background(), "GET /healthz",
		trace.WithSpanKind(trace.SpanKindServer))
	probe := instrumentedProbe(cfg, prober, tel, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), true)
	probe(ctx)
	parent.End()

	var child, root string
	for _, s := range recorder.Ended() {
		if s.Name() == "GET /healthz" {
			root = s.SpanContext().SpanID().String()
		} else {
			child = s.Parent().SpanID().String()
		}
	}
	if root == "" || child == "" {
		t.Fatalf("expected both spans to end, got %d", len(recorder.Ended()))
	}
	if child != root {
		t.Errorf("probe span's parent = %s, want the caller's span %s", child, root)
	}
}
