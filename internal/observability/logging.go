package observability

import (
	"context"
	"io"
	"log/slog"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
)

// NewJSONLogger returns a [*slog.Logger] that writes structured JSON records to
// w. Pass os.Stdout in production; pass a [*bytes.Buffer] in tests.
func NewJSONLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// WithTraceContext returns a child logger pre-populated with "trace_id" and
// "span_id" fields extracted from the active OpenTelemetry span in ctx. If ctx
// carries no valid span the original logger is returned unchanged.
func WithTraceContext(ctx context.Context, logger *slog.Logger) *slog.Logger {
	span := trace.SpanFromContext(ctx)
	sc := span.SpanContext()
	if !sc.IsValid() {
		return logger
	}
	return logger.With(
		"trace_id", sc.TraceID().String(),
		"span_id", sc.SpanID().String(),
	)
}

// NewLogr wraps a [*slog.Logger] as a [logr.Logger] so it can be passed to
// controller-runtime's [ctrl.SetLogger]. All controller-runtime log output
// (including reconcile errors) then flows through the slog JSON handler.
func NewLogr(logger *slog.Logger) logr.Logger {
	return logr.FromSlogHandler(logger.Handler())
}
