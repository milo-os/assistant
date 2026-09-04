package tracing

import (
	"context"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestSetupNoopWhenUnconfigured proves the core safety property: with no
// OTLP endpoint env var set, Setup must not dial anything, must not error,
// and must return promptly. This is what makes it safe to leave tracing "on"
// by default in every environment, including dev and CI, where no collector
// is listening.
func TestSetupNoopWhenUnconfigured(t *testing.T) {
	t.Setenv(otlpEndpointEnv, "")
	t.Setenv(tracesEndpointEnv, "")
	os.Unsetenv(otlpEndpointEnv)
	os.Unsetenv(tracesEndpointEnv)

	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	done := make(chan struct{})
	var shutdown Shutdown
	var err error
	go func() {
		shutdown, err = Setup(context.Background(), "assistant-test")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Setup blocked with no OTLP endpoint configured — it must return immediately")
	}
	if err != nil {
		t.Fatalf("Setup returned an error with no endpoint configured: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Setup returned a nil Shutdown")
	}

	// The installed provider must be the no-op implementation — real spans
	// created against it must never reach an exporter or attempt a network
	// call.
	got := otel.GetTracerProvider()
	if _, ok := got.(noop.TracerProvider); !ok {
		t.Fatalf("tracer provider = %T, want noop.TracerProvider", got)
	}

	// Shutdown must also be a safe, immediate no-op.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("no-op shutdown returned an error: %v", err)
	}

	// Creating and ending a span through the installed provider must not
	// panic or block, confirming the whole path is inert.
	_, span := got.Tracer("tracing_test").Start(context.Background(), "smoke")
	span.End()
}
