package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmanciop/systemd-unit-healthz/internal/unitprobe"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return NewLogger(buf, slog.LevelDebug)
}

// Telemetry off is a supported configuration, not a degraded one: everything
// has to work, and no Record call may panic on a nil instrument.
func TestSetupWithoutAConfigFile(t *testing.T) {
	var buf bytes.Buffer
	tel, err := Setup(context.Background(), "", testLogger(&buf))
	if err != nil {
		t.Fatalf("Setup with no config file: %v", err)
	}

	tel.RecordUnits(context.Background(), []unitprobe.Status{{
		Name: "minecraft.service", ActiveState: "active", SubState: "running",
		ActiveSince: time.Now().Add(-time.Minute),
	}})
	tel.RecordAuthFailure(context.Background(), "bad_token")

	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown with no SDK installed: %v", err)
	}

	if !strings.Contains(buf.String(), "telemetry.disabled") {
		t.Errorf("expected a telemetry.disabled log line, got %q", buf.String())
	}
	// The propagator warning is about a file that forgot the block. With no file
	// at all it would be noise.
	if strings.Contains(buf.String(), "telemetry.propagator_missing") {
		t.Error("warned about a missing propagator while telemetry is switched off")
	}
}

func TestSetupFailsOnAnUnreadableOrInvalidConfigFile(t *testing.T) {
	var buf bytes.Buffer

	if _, err := Setup(context.Background(), filepath.Join(t.TempDir(), "absent.yaml"), testLogger(&buf)); err == nil {
		t.Error("Setup accepted a missing telemetry config file")
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("file_format: \"1.0-rc.2\"\ntracer_provider: [this is not a map]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(context.Background(), bad, testLogger(&buf)); err == nil {
		t.Error("Setup accepted an invalid telemetry config file")
	}
}

// The failure this guards against: fully instrumented, exporting happily, and
// silently ignoring the incoming traceparent because the file has no propagator.
func TestSetupWarnsWhenTheConfigDeclaresNoPropagator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "otel.yaml")
	if err := os.WriteFile(path, []byte("file_format: \"1.0-rc.2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Setup(context.Background(), path, testLogger(&buf)); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if !strings.Contains(buf.String(), "telemetry.propagator_missing") {
		t.Errorf("no propagator warning for a config with no propagator block; log was %q", buf.String())
	}
}

func TestNoWarningWhenThePropagatorIsConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "otel.yaml")
	contents := "file_format: \"1.0-rc.2\"\npropagator:\n  composite_list: \"tracecontext,baggage\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if _, err := Setup(context.Background(), path, testLogger(&buf)); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if strings.Contains(buf.String(), "telemetry.propagator_missing") {
		t.Errorf("warned about a missing propagator when one is configured; log was %q", buf.String())
	}
}

func TestLoggerEmitsJSONLines(t *testing.T) {
	var buf bytes.Buffer
	testLogger(&buf).Info("health.evaluated", "healthy", true)

	line := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
		t.Errorf("log line is not a JSON object: %q", line)
	}
	if strings.Count(line, "\n") != 0 {
		t.Errorf("log record spans multiple lines, which the journal parser cannot handle: %q", line)
	}
	if !strings.Contains(line, `"msg":"health.evaluated"`) {
		t.Errorf("message missing from %q", line)
	}
}
