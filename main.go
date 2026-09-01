// Command systemd-unit-healthz serves the state of systemd units as JSON over
// HTTPS, for an external health check to poll.
//
// It reads unit state over the D-Bus system bus, terminates TLS itself, and
// checks a shared secret itself. The response is the instantaneous truth: there
// is no debouncing and no grace period, on the grounds that smoothing belongs
// in whatever alerts on this, not in the thing being measured.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/mmanciop/systemd-unit-healthz/internal/config"
	"github.com/mmanciop/systemd-unit-healthz/internal/health"
	"github.com/mmanciop/systemd-unit-healthz/internal/server"
	"github.com/mmanciop/systemd-unit-healthz/internal/telemetry"
	"github.com/mmanciop/systemd-unit-healthz/internal/unitprobe"
)

// version is set at build time with -X main.version=...
var version = "dev"

func main() {
	configPath := flag.String("config", os.Getenv("SYSTEMD_UNIT_HEALTHZ_CONFIG"),
		"path to the configuration file (or set SYSTEMD_UNIT_HEALTHZ_CONFIG)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	logLevel := flag.String("log-level", "info", "one of debug, info, warn, error")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	logger := telemetry.NewLogger(os.Stdout, parseLevel(*logLevel))

	if err := run(logger, *configPath); err != nil {
		logger.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger, configPath string) error {
	if configPath == "" {
		return fmt.Errorf("no configuration file given; pass --config or set SYSTEMD_UNIT_HEALTHZ_CONFIG")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logger.Info("config.loaded",
		"path", configPath, "version", version,
		"listen", cfg.Listen, "route", cfg.Path, "units", cfg.Units)

	// SIGTERM is what systemd sends on stop and restart; SIGINT is for running
	// this by hand.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	tel, err := telemetry.Setup(ctx, cfg.Telemetry.ConfigFile, logger)
	if err != nil {
		return err
	}

	prober := unitprobe.New()
	defer prober.Close()

	// Traced: each of these is caused by an inbound request, so its span
	// belongs in that request's trace.
	probe := instrumentedProbe(cfg, prober, tel, logger, true)

	handler := health.New(cfg.Units, probe, tel.Tracer)

	srv, err := server.New(cfg, handler, tel, logger)
	if err != nil {
		return err
	}

	// Sampling on a timer as well as per request: the metrics have to exist
	// when nobody is polling, or an absence-based alert has nothing to go on and
	// a restart between two polls leaves no trace at all.
	//
	// Untraced, deliberately -- see instrumentedProbe.
	if interval := cfg.SampleInterval.Duration(); interval > 0 {
		go sampleForever(ctx, interval, instrumentedProbe(cfg, prober, tel, logger, false))
	}

	serveErr := srv.Serve(ctx)

	// Flush telemetry after the server has drained, or the final request's span
	// never leaves the process.
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.Shutdown(flushCtx); err != nil {
		logger.Warn("telemetry.shutdown_failed", "error", err.Error())
	}

	return serveErr
}

// instrumentedProbe wraps the prober in a timeout, the metric recording both
// callers share, and -- only when traced -- a client span.
//
// traced is a parameter rather than something inferred inside, because the two
// callers want different things and the difference should be visible at the
// call site. A request-driven probe belongs in the trace of the request that
// caused it. The background sampler has no caller: a span for it would be a
// CLIENT span at the root of its own trace, once per tick, carrying nothing
// the metrics do not already carry.
func instrumentedProbe(
	cfg *config.Config,
	prober unitprobe.Prober,
	tel *telemetry.Telemetry,
	logger *slog.Logger,
	traced bool,
) func(context.Context) []unitprobe.Status {
	return func(ctx context.Context) []unitprobe.Status {
		ctx, cancel := context.WithTimeout(ctx, cfg.ProbeTimeout.Duration())
		defer cancel()

		var span trace.Span
		if traced {
			// The rpc.* attributes are what get this classified as a client
			// call rather than an unlabelled internal span. rpc.system is an
			// open enum, so "dbus" is a legitimate value.
			ctx, span = tel.Tracer.Start(ctx, "org.freedesktop.systemd1.Manager/GetUnitProperties",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(
					attribute.String("rpc.system", "dbus"),
					attribute.String("rpc.service", "org.freedesktop.systemd1.Manager"),
					attribute.String("rpc.method", "GetUnitProperties"),
					attribute.String("network.transport", "unix"),
					attribute.String("network.peer.address", "/run/dbus/system_bus_socket"),
				))
			defer span.End()
		}

		statuses := prober.Probe(ctx, cfg.Units)
		if span != nil {
			span.SetAttributes(health.UnitAttributes(statuses)...)
		}

		for _, st := range statuses {
			if st.Err != nil {
				logger.WarnContext(ctx, "probe.dbus.failed", "unit", st.Name, "error", st.Err.Error())
			}
		}

		tel.RecordUnits(ctx, statuses)
		return statuses
	}
}

func sampleForever(ctx context.Context, interval time.Duration, probe func(context.Context) []unitprobe.Status) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe(ctx)
		}
	}
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
