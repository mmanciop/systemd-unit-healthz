// Package health turns unit probe results into an HTTP response.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/mmanciop/systemd-unit-healthz/internal/telemetry"
	"github.com/mmanciop/systemd-unit-healthz/internal/unitprobe"
)

// Response is the JSON body. Field order here is the order in the output, and
// the units array follows configuration order -- documented, because JSONPath
// assertions index into it.
type Response struct {
	Healthy bool   `json:"healthy"`
	Units   []Unit `json:"units"`
}

// Unit is one unit's state.
type Unit struct {
	Name        string `json:"name"`
	Healthy     bool   `json:"healthy"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
	// ActiveSince is null for a unit that has never been active, rather than
	// the empty string the shell predecessor emitted.
	ActiveSince *string `json:"active_since"`
	// Error is set only when the unit could not be read at all, which is a
	// different condition from a unit that is legitimately down.
	Error string `json:"error,omitempty"`
}

// Handler answers health requests.
type Handler struct {
	units  []string
	probe  func(context.Context) []unitprobe.Status
	tracer trace.Tracer
}

// New returns a Handler. probe is injected so tests do not need a system bus.
func New(units []string, probe func(context.Context) []unitprobe.Status, tracer trace.Tracer) *Handler {
	return &Handler{units: units, probe: probe, tracer: tracer}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed\n", http.StatusMethodNotAllowed)
		return
	}

	resp := Build(h.probe(r.Context()))

	// Annotate the server span otelhttp created, so a trace search can find red
	// evaluations without opening the body.
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(
		telemetry.AttrHealthy.Bool(resp.Healthy),
		telemetry.AttrUnhealthyNo.Int(resp.unhealthyCount()),
	)
	if !resp.Healthy {
		// ERROR only here: a 403 or a 404 is the server working correctly, and
		// marking those as errors makes the error rate meaningless.
		span.SetStatus(codes.Error, resp.summary())
	}

	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "internal error\n", http.StatusInternalServerError)
		return
	}

	status := http.StatusOK
	if !resp.Healthy {
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// Build converts probe results into a Response.
func Build(statuses []unitprobe.Status) Response {
	resp := Response{Healthy: true, Units: make([]Unit, 0, len(statuses))}
	for _, st := range statuses {
		u := Unit{
			Name:        st.Name,
			Healthy:     st.Healthy(),
			ActiveState: st.ActiveState,
			SubState:    st.SubState,
		}
		if !st.ActiveSince.IsZero() {
			s := st.ActiveSince.Format(time.RFC3339)
			u.ActiveSince = &s
		}
		if st.Err != nil {
			u.Error = st.Err.Error()
		}
		if !u.Healthy {
			resp.Healthy = false
		}
		resp.Units = append(resp.Units, u)
	}
	// No units means nothing was asked for, which cannot honestly be reported
	// as healthy. Config validation rejects this, so it is a guard, not a path.
	if len(statuses) == 0 {
		resp.Healthy = false
	}
	return resp
}

func (r Response) unhealthyCount() int {
	n := 0
	for _, u := range r.Units {
		if !u.Healthy {
			n++
		}
	}
	return n
}

// summary is the span status message: which units are unhealthy and how.
func (r Response) summary() string {
	out := ""
	for _, u := range r.Units {
		if u.Healthy {
			continue
		}
		if out != "" {
			out += "; "
		}
		switch {
		case u.Error != "":
			out += u.Name + ": " + u.Error
		default:
			out += u.Name + " is " + orUnknown(u.ActiveState) + "/" + orUnknown(u.SubState)
		}
	}
	if out == "" {
		out = "no units configured"
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// UnitAttributes returns the probe results as span attributes.
//
// Three parallel arrays rather than three attributes per unit: span attribute
// keys are unique, so repeating systemd.unit.name once per unit would leave
// only the last one. Index i of each array describes the same unit.
func UnitAttributes(statuses []unitprobe.Status) []attribute.KeyValue {
	names := make([]string, 0, len(statuses))
	states := make([]string, 0, len(statuses))
	subStates := make([]string, 0, len(statuses))
	for _, st := range statuses {
		names = append(names, st.Name)
		states = append(states, orUnknown(st.ActiveState))
		subStates = append(subStates, orUnknown(st.SubState))
	}
	return []attribute.KeyValue{
		telemetry.AttrUnitName.StringSlice(names),
		telemetry.AttrUnitState.StringSlice(states),
		telemetry.AttrSubState.StringSlice(subStates),
	}
}
