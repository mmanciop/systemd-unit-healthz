package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/mmanciop/systemd-unit-healthz/internal/unitprobe"
)

func handlerFor(statuses ...unitprobe.Status) *Handler {
	names := make([]string, 0, len(statuses))
	for _, s := range statuses {
		names = append(names, s.Name)
	}
	return New(names, func(context.Context) []unitprobe.Status { return statuses }, noop.NewTracerProvider().Tracer("test"))
}

func active(name string) unitprobe.Status {
	return unitprobe.Status{
		Name:        name,
		ActiveState: "active",
		SubState:    "running",
		ActiveSince: time.Date(2026, 8, 26, 9, 12, 41, 0, time.UTC),
	}
}

func get(t *testing.T, h *Handler, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/healthz", nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) Response {
	t.Helper()
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestAllUnitsHealthy(t *testing.T) {
	rec := get(t, handlerFor(active("minecraft.service")), http.MethodGet)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	resp := decode(t, rec)
	if !resp.Healthy {
		t.Error("Healthy = false for an active/running unit")
	}
	if len(resp.Units) != 1 || !resp.Units[0].Healthy {
		t.Fatalf("units = %+v, want one healthy entry", resp.Units)
	}
	if resp.Units[0].ActiveSince == nil || *resp.Units[0].ActiveSince != "2026-08-26T09:12:41Z" {
		t.Errorf("ActiveSince = %v, want RFC 3339 2026-08-26T09:12:41Z", resp.Units[0].ActiveSince)
	}
}

// active/running is two conditions, not one: a unit can be "active" with a
// sub-state that is not running.
func TestActiveButNotRunningIsUnhealthy(t *testing.T) {
	st := active("minecraft.service")
	st.SubState = "start-pre"

	rec := get(t, handlerFor(st), http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if decode(t, rec).Healthy {
		t.Error("Healthy = true for active/start-pre")
	}
}

func TestOneUnhealthyUnitFailsTheWhole(t *testing.T) {
	down := unitprobe.Status{Name: "nginx.service", ActiveState: "failed", SubState: "failed"}

	rec := get(t, handlerFor(active("minecraft.service"), down), http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	resp := decode(t, rec)
	if resp.Healthy {
		t.Error("Healthy = true while one unit is failed")
	}
	if !resp.Units[0].Healthy {
		t.Error("the healthy unit was reported unhealthy")
	}
	if resp.Units[1].Healthy {
		t.Error("the failed unit was reported healthy")
	}
	// A unit that was never active must not claim a timestamp.
	if resp.Units[1].ActiveSince != nil {
		t.Errorf("ActiveSince = %v for a never-active unit, want null", *resp.Units[1].ActiveSince)
	}
}

// A unit that cannot be read is not the same as a unit that is down, and the
// body has to let a human tell them apart.
func TestProbeErrorIsReportedDistinctly(t *testing.T) {
	broken := unitprobe.Status{Name: "minecraft.service", Err: errors.New("connect to the D-Bus system bus: no such file")}

	rec := get(t, handlerFor(broken), http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	unit := decode(t, rec).Units[0]
	if unit.Healthy {
		t.Error("Healthy = true for a unit that could not be read")
	}
	if unit.Error == "" {
		t.Error("Error is empty; the reason a unit could not be read must reach the body")
	}
	if unit.ActiveState != "" {
		t.Errorf("ActiveState = %q, want empty when the probe failed", unit.ActiveState)
	}
}

// Configuration order is part of the contract: JSONPath assertions index into
// the array.
func TestUnitOrderFollowsConfiguration(t *testing.T) {
	resp := decode(t, get(t, handlerFor(
		active("first.service"), active("second.service"), active("third.service"),
	), http.MethodGet))

	for i, want := range []string{"first.service", "second.service", "third.service"} {
		if resp.Units[i].Name != want {
			t.Errorf("units[%d].name = %q, want %q", i, resp.Units[i].Name, want)
		}
	}
}

func TestHeadReturnsStatusWithoutABody(t *testing.T) {
	rec := get(t, handlerFor(active("minecraft.service")), http.MethodHead)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty for HEAD", rec.Body.String())
	}
}

func TestWriteMethodsAreRejected(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := get(t, handlerFor(active("minecraft.service")), method)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("%s: no Allow header on a 405", method)
		}
	}
}

func TestBuildWithNoUnitsIsNotHealthy(t *testing.T) {
	if Build(nil).Healthy {
		t.Error("Build(nil).Healthy = true; nothing measured cannot be reported as healthy")
	}
}

// Span attributes must use unique keys, so per-unit values are parallel arrays
// rather than one attribute per unit.
func TestUnitAttributesAreParallelArrays(t *testing.T) {
	attrs := UnitAttributes([]unitprobe.Status{
		active("a.service"),
		{Name: "b.service"},
	})

	if len(attrs) != 3 {
		t.Fatalf("got %d attributes, want 3 parallel arrays", len(attrs))
	}
	seen := map[string]bool{}
	for _, a := range attrs {
		key := string(a.Key)
		if seen[key] {
			t.Errorf("attribute key %q appears more than once", key)
		}
		seen[key] = true
		if got := len(a.Value.AsStringSlice()); got != 2 {
			t.Errorf("attribute %q has %d values, want one per unit (2)", key, got)
		}
	}
	// An unread unit still needs a value in every array, or the arrays stop
	// lining up by index.
	states := attrs[1].Value.AsStringSlice()
	if states[1] != "unknown" {
		t.Errorf("state for the unread unit = %q, want %q", states[1], "unknown")
	}
}
