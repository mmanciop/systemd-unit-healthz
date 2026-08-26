package telemetry

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// NewLogger returns a JSON logger that writes one line per record.
//
// Logs go to stdout for the journal rather than over OTLP, on purpose: a crash
// before the SDK is installed, or a collector that is unreachable, still leaves
// a record on disk. It also means there is no log provider to flush on the way
// out.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(&traceHandler{
		Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}),
	})
}

// traceHandler adds the active span's identifiers to every record, so a log
// line can be joined to the trace that produced it.
type traceHandler struct{ slog.Handler }

func (h *traceHandler) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		// These key names are what Dash0 and the OpenTelemetry log data model
		// expect for correlation.
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, rec)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{Handler: h.Handler.WithGroup(name)}
}
