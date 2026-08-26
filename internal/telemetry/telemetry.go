// Package telemetry wires up OpenTelemetry from a declarative configuration
// file and owns the instruments this service records.
//
// Telemetry is contingent on that file: with no file, Setup installs nothing,
// the global providers stay the API's no-op defaults, and every Record call
// below is a cheap no-op. That keeps the service usable on a host with no
// collector without a second code path to maintain.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/otelconf"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/mmanciop/systemd-unit-healthz/internal/unitprobe"
)

// scopeName identifies this service's instrumentation scope.
const scopeName = "github.com/mmanciop/systemd-unit-healthz"

// Attribute keys.
//
// systemd has no OpenTelemetry semantic conventions, so these are ours. They
// are namespaced and self-describing, and deliberately avoid the bare "name"
// label the Prometheus systemd exporter uses, which is a collision waiting to
// happen in a shared metric store.
const (
	AttrUnitName    = attribute.Key("systemd.unit.name")
	AttrUnitState   = attribute.Key("systemd.unit.state")
	AttrSubState    = attribute.Key("systemd.unit.sub_state")
	AttrErrorType   = attribute.Key("error.type")
	AttrHealthy     = attribute.Key("healthz.healthy")
	AttrUnhealthyNo = attribute.Key("healthz.units.unhealthy")
)

// Telemetry holds the instruments and the SDK shutdown hook.
type Telemetry struct {
	Tracer trace.Tracer

	unitState  metric.Int64Gauge
	unitActive metric.Int64Gauge
	unitUptime metric.Float64Gauge
	authFails  metric.Int64Counter

	shutdown func(context.Context) error
}

// Setup installs the SDK described by configFile and returns the instruments.
//
// An empty configFile is not an error: it means telemetry is switched off.
func Setup(ctx context.Context, configFile string, logger *slog.Logger) (*Telemetry, error) {
	var shutdown func(context.Context) error
	if configFile == "" {
		logger.Info("telemetry.disabled", "reason", "no telemetry.configFile in the configuration")
	} else {
		var err error
		if shutdown, err = installSDK(ctx, configFile); err != nil {
			return nil, err
		}
		logger.Info("telemetry.enabled", "config_file", configFile)
	}

	t := &Telemetry{Tracer: otel.Tracer(scopeName), shutdown: shutdown}
	if err := t.initInstruments(otel.Meter(scopeName)); err != nil {
		return nil, err
	}

	// The propagator is part of the configuration file, which makes it possible
	// to run fully instrumented and still drop the incoming traceparent -- every
	// span becomes a disconnected root and nothing about the service looks
	// wrong. Fields() is empty for the API's no-op propagator, so this catches
	// both "telemetry off" and "file forgot the propagator block".
	if configFile != "" && len(otel.GetTextMapPropagator().Fields()) == 0 {
		logger.Warn("telemetry.propagator_missing",
			"detail", "no text map propagator is configured, so incoming trace context is ignored and every span will be a root",
			"fix", "add propagator.composite_list: \"tracecontext,baggage\" to the telemetry configuration file")
	}

	return t, nil
}

func installSDK(ctx context.Context, configFile string) (func(context.Context) error, error) {
	raw, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("read telemetry configuration: %w", err)
	}
	cfg, err := otelconf.ParseYAML(raw)
	if err != nil {
		return nil, fmt.Errorf("parse telemetry configuration %s: %w", configFile, err)
	}
	sdk, err := otelconf.NewSDK(
		otelconf.WithContext(ctx),
		otelconf.WithOpenTelemetryConfiguration(*cfg),
	)
	if err != nil {
		return nil, fmt.Errorf("build the telemetry SDK from %s: %w", configFile, err)
	}

	otel.SetTracerProvider(sdk.TracerProvider())
	otel.SetMeterProvider(sdk.MeterProvider())
	otel.SetTextMapPropagator(sdk.Propagator())

	return sdk.Shutdown, nil
}

func (t *Telemetry) initInstruments(m metric.Meter) error {
	var err error
	var errs []error
	record := func(e error) {
		if e != nil {
			errs = append(errs, e)
		}
	}

	// A state set rather than an enum: 1 for the current state and 0 for the
	// others. An explicit 0 is what lets a query for state="active" alert during
	// an outage instead of going absent, and naming states in words beats
	// remembering what integer 4 meant.
	t.unitState, err = m.Int64Gauge("systemd.unit.state",
		metric.WithDescription("1 for the unit's current ActiveState, 0 for every other state"),
		metric.WithUnit("1"))
	record(err)

	// Derivable from the state set, kept because it makes the common alert and
	// the common dashboard tile a one-liner.
	t.unitActive, err = m.Int64Gauge("systemd.unit.active",
		metric.WithDescription("1 when the unit's ActiveState is active, 0 otherwise"),
		metric.WithUnit("1"))
	record(err)

	// A drop in this proves a restart happened even when no sample ever caught
	// the unit down, which is the blind spot a 30s scrape interval leaves.
	t.unitUptime, err = m.Float64Gauge("systemd.unit.uptime",
		metric.WithDescription("Seconds since the unit last entered the active state, 0 when it is not active"),
		metric.WithUnit("s"))
	record(err)

	// Not derivable from http.server.request.duration's 403 bucket once
	// scanners start knocking, and a half-finished token rotation is otherwise
	// invisible.
	t.authFails, err = m.Int64Counter("systemd_unit_healthz.auth.failures",
		metric.WithDescription("Requests rejected because they carried no valid credential"),
		metric.WithUnit("{failure}"))
	record(err)

	if len(errs) > 0 {
		return fmt.Errorf("create instruments: %w", errs[0])
	}
	return nil
}

// RecordUnits records the metrics derived from one probe cycle.
func (t *Telemetry) RecordUnits(ctx context.Context, statuses []unitprobe.Status) {
	now := time.Now()
	for _, st := range statuses {
		unit := AttrUnitName.String(st.Name)

		for _, state := range unitprobe.ActiveStates {
			value := int64(0)
			if st.ActiveState == state {
				value = 1
			}
			t.unitState.Record(ctx, value, metric.WithAttributes(unit, AttrUnitState.String(state)))
		}

		active := int64(0)
		if st.ActiveState == "active" {
			active = 1
		}
		t.unitActive.Record(ctx, active, metric.WithAttributes(unit))

		uptime := 0.0
		if active == 1 && !st.ActiveSince.IsZero() {
			uptime = now.Sub(st.ActiveSince).Seconds()
		}
		t.unitUptime.Record(ctx, uptime, metric.WithAttributes(unit))
	}
}

// RecordAuthFailure counts one rejected request. reason is a bounded enum, so
// it is safe as an attribute; the presented credential never is.
func (t *Telemetry) RecordAuthFailure(ctx context.Context, reason string) {
	t.authFails.Add(ctx, 1, metric.WithAttributes(AttrErrorType.String(reason)))
}

// Shutdown flushes and stops the SDK. It must run after the HTTP server has
// finished draining, or the last request's span never leaves the process.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t.shutdown == nil {
		return nil
	}
	return t.shutdown(ctx)
}
