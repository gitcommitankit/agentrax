// Package observability provides OpenTelemetry tracing initialization for the Agentrax operator.
//
// Use [InitTracerProvider] in main() to configure the global OTel tracer. When
// endpoint is empty the function installs a no-op provider so callers need not
// guard on whether tracing is enabled. The returned Shutdown function must be
// deferred in main() to flush buffered spans before process exit.
package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName is the instrumentation library name used for all Agentrax OTel spans.
const TracerName = "agentrax.io/controller"

// Tracer is the package-level tracer used by controller code. It is set by
// [InitTracerProvider] and defaults to the global no-op tracer so it is always
// safe to call, even when tracing is disabled.
var Tracer trace.Tracer = noop.NewTracerProvider().Tracer(TracerName)

// InitTracerProvider configures the global OpenTelemetry TracerProvider and
// installs a W3C TraceContext propagator. When endpoint is empty it installs a
// no-op provider (tracing disabled). The returned Shutdown function flushes and
// stops the exporter; it must be deferred in main().
func InitTracerProvider(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	if endpoint == "" {
		// No-op: tracing disabled. Global tracer stays as the default no-op.
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP gRPC exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName("agentrax-operator"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating OTel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// Sample all traces by default. Operators may reduce this with env-based
		// sampler configuration via OTEL_TRACES_SAMPLER.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tp)
	// W3C TraceContext + Baggage propagation — required for cross-service correlation.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Update the package-level tracer to use the real provider.
	Tracer = tp.Tracer(TracerName)

	return tp.Shutdown, nil
}
