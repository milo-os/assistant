// Package tracing wires the OpenTelemetry Go SDK for the assistant's
// binaries. It follows the codebase's established "nil/unset disables the
// feature" convention (see e.g. Deps.Memory, Deps.GapReports in
// internal/agent): with no OTLP endpoint configured, [Setup] installs a no-op
// tracer provider and does not attempt any network connection, so a service
// with tracing left "on" but no collector present behaves exactly as it did
// before tracing existed — no dial attempts, no export errors, no added
// startup latency.
//
// When configured, spans flow to any OTLP-compatible collector (the Otel
// Collector, Tempo, Jaeger, etc.) over gRPC.
package tracing

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace/noop"
)

// otlpEndpointEnv is the standard OTel env var that gates whether tracing
// exports anywhere. Its presence is the single on/off switch for this
// package: unset ⇒ no-op, set ⇒ real OTLP/gRPC export. The exporter itself
// also reads OTEL_EXPORTER_OTLP_ENDPOINT (and the traces-specific override,
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) via the SDK's own standard env-var
// resolution — see [otlptracegrpc.New] — so this package does not hand-roll
// endpoint/header/compression parsing.
const otlpEndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// tracesEndpointEnv is the traces-specific override of otlpEndpointEnv. Either
// being set is enough to opt into real export.
const tracesEndpointEnv = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"

// Shutdown drains and closes the tracer provider. Callers should invoke it
// (with a bounded context) during graceful shutdown so buffered spans are not
// dropped when the process exits. It is always non-nil and always safe to
// call, including in the no-op case.
type Shutdown func(context.Context) error

// Setup installs the global OpenTelemetry tracer provider for serviceName
// ("assistant" or "assistant-apiserver") and returns a [Shutdown] func the
// caller must invoke on process exit.
//
// Configuration is entirely via the standard OTEL_EXPORTER_OTLP_ENDPOINT (or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) env var — there is no assistant-specific
// tracing config. When neither is set, Setup installs
// [go.opentelemetry.io/otel/trace/noop]'s TracerProvider: every span created
// through it is a genuine no-op (no allocation of span state beyond the
// interface, no batching goroutine, no exporter, no dial). This is the
// default posture in dev and in any environment without a collector, and it
// is safe to leave on unconditionally.
//
// The W3C tracecontext + baggage propagators are always installed as the
// global propagator, independent of whether export is enabled, so
// trace-context headers (traceparent/tracestate) still flow through
// otelhttp-instrumented hops even when this particular process is not itself
// exporting spans — a downstream hop may have a collector configured even if
// this one doesn't.
func Setup(ctx context.Context, serviceName string) (Shutdown, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if os.Getenv(otlpEndpointEnv) == "" && os.Getenv(tracesEndpointEnv) == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	// otlptracegrpc.New with no options reads OTEL_EXPORTER_OTLP_ENDPOINT (and
	// the traces-specific override, plus headers/compression/timeout env vars)
	// via the SDK's standard env resolution. The gRPC client this creates does
	// not dial eagerly, so failures surface at export time, not here.
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
		resource.WithFromEnv(),      // OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME
		resource.WithTelemetrySDK(), // telemetry.sdk.{name,language,version}
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}
