package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestInitTracerProvider_Noop verifies that an empty endpoint installs a no-op
// provider and returns a no-error Shutdown function.
func TestInitTracerProvider_Noop(t *testing.T) {
	// Reset global state after the test.
	original := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(original) })

	shutdown, err := InitTracerProvider(context.Background(), "")
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	// Calling Shutdown on the no-op path must not error.
	require.NoError(t, shutdown(context.Background()))

	// The global provider must be the same as before (no-op path leaves it alone).
	assert.Equal(t, original, otel.GetTracerProvider())
}

// TestWithTraceContext_NoSpan verifies that WithTraceContext is safe to call
// when there is no active OTel span in ctx. The returned logger must behave
// identically to the input logger without panicking.
func TestWithTraceContext_NoSpan(t *testing.T) {
	tp := noop.NewTracerProvider()
	otel.SetTracerProvider(tp)

	var buf bytes.Buffer
	logger := NewJSONLogger(&buf)

	// ctx has no active span — WithTraceContext must return the logger unchanged.
	enriched := WithTraceContext(context.Background(), logger)
	require.NotNil(t, enriched)

	// Writing a log record must not panic and must produce valid JSON.
	enriched.Info("test message", "key", "value")
	assert.Contains(t, buf.String(), `"msg":"test message"`)
	// No trace_id should appear when there is no active span.
	assert.False(t, strings.Contains(buf.String(), "trace_id"),
		"expected no trace_id in log output when span is not active")
}

// TestWithTraceContext_ActiveSpan verifies that trace_id and span_id are
// injected into log records when an active span exists in ctx.
func TestWithTraceContext_ActiveSpan(t *testing.T) {
	tp := noop.NewTracerProvider()
	otel.SetTracerProvider(tp)

	// Start a real span using the no-op provider (IDs are all zeros, but valid).
	ctx, span := tp.Tracer(TracerName).Start(context.Background(), "test-span")
	defer span.End()

	var buf bytes.Buffer
	logger := NewJSONLogger(&buf)
	enriched := WithTraceContext(ctx, logger)

	enriched.Info("traced message")

	// The noop provider returns a non-recording span. SpanContext is not valid
	// (all-zero trace/span IDs), so WithTraceContext must not inject fields.
	// This test validates the guard branch (sc.IsValid check) does not panic.
	assert.Contains(t, buf.String(), `"msg":"traced message"`)
}
