package agent

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/milo-os/assistant/agentcore"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
)

// tracer is this package's OpenTelemetry tracer. It is safe to use
// unconditionally, including when tracing is entirely unconfigured: with no
// OTEL_EXPORTER_OTLP_ENDPOINT set, internal/tracing installs the global
// no-op TracerProvider, so every Start call below returns a genuine no-op
// span — no allocation of span state beyond the interface, no exporter, no
// network call.
//
// Every span this file creates carries only infrastructure attributes (tool
// name, model id, outcome, error presence/message). It deliberately NEVER
// attaches user message text, tool-call arguments, or tool/model output —
// spans are infrastructure telemetry, not a place for user or provider
// content to leak into an observability backend (the same posture the
// report_capability_gap tool's summary-field constraints establish for
// tool-authored text; see internal/capability).
var tracer = otel.Tracer("github.com/milo-os/assistant/internal/agent")

// tracedModel wraps an [agentcore.Model] so every inference request gets a
// "model.stream" span, running from the call to Stream until the returned
// reader is fully drained or Closed (so the span's duration reflects the
// whole streamed call, not just the time to open it), and — at that exact
// same point — one assistant_model_call_duration_seconds observation labeled
// success/error (see [appmetrics.Metrics.RecordModelCall]). A nil model
// returns nil so callers can pass through an unset Deps.Model unchanged. A
// nil metrics is a safe no-op ([appmetrics.Metrics]'s Record* methods
// tolerate a nil receiver).
func tracedModel(m agentcore.Model, metrics *appmetrics.Metrics) agentcore.Model {
	if m == nil {
		return nil
	}
	return &tracingModel{inner: m, metrics: metrics}
}

type tracingModel struct {
	inner   agentcore.Model
	metrics *appmetrics.Metrics
}

func (m *tracingModel) ModelID() string { return m.inner.ModelID() }

func (m *tracingModel) Stream(ctx context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	ctx, span := tracer.Start(ctx, "model.stream", trace.WithAttributes(
		attribute.String("model.id", m.inner.ModelID()),
	))
	start := time.Now()
	reader, err := m.inner.Stream(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		m.metrics.RecordModelCall("error", time.Since(start))
		return nil, err
	}
	return &tracingStreamReader{inner: reader, span: span, metrics: m.metrics, start: start}, nil
}

// tracingStreamReader ends its model.stream span once the wrapped stream is
// exhausted (Recv returns io.EOF or a terminal error) or explicitly Closed,
// whichever happens first, and at that same moment records one
// assistant_model_call_duration_seconds observation. It is not safe for
// concurrent use, matching [agentcore.StreamReader]'s existing
// single-consumer contract.
type tracingStreamReader struct {
	inner   agentcore.StreamReader
	span    trace.Span
	metrics *appmetrics.Metrics
	start   time.Time
	ended   bool
}

func (r *tracingStreamReader) Recv() (agentcore.StreamPart, error) {
	part, err := r.inner.Recv()
	if err != nil {
		r.endSpan(err)
	}
	return part, err
}

func (r *tracingStreamReader) Close() error {
	err := r.inner.Close()
	r.endSpan(nil)
	return err
}

func (r *tracingStreamReader) endSpan(err error) {
	if r.ended {
		return
	}
	r.ended = true
	outcome := "success"
	if err != nil && err != io.EOF {
		r.span.RecordError(err)
		r.span.SetStatus(codes.Error, err.Error())
		outcome = "error"
	}
	r.span.End()
	r.metrics.RecordModelCall(outcome, time.Since(r.start))
}

// tracedTools wraps every tool in a set so each Execute call gets a
// "tool.execute" span carrying only the tool's name — never its input
// (tool-call arguments, which may carry user-supplied data) or its textual
// result — and, right alongside that span, one assistant_tool_call_total
// increment labeled by tool name and outcome (success/error). A nil metrics
// is a safe no-op.
func tracedTools(tools agentcore.ToolSet, metrics *appmetrics.Metrics) agentcore.ToolSet {
	if len(tools) == 0 {
		return tools
	}
	wrapped := make(agentcore.ToolSet, len(tools))
	for name, t := range tools {
		wrapped[name] = &tracingTool{inner: t, name: name, metrics: metrics}
	}
	return wrapped
}

type tracingTool struct {
	inner   agentcore.Tool
	name    string
	metrics *appmetrics.Metrics
}

func (t *tracingTool) Definition() agentcore.ToolDefinition { return t.inner.Definition() }

func (t *tracingTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	ctx, span := tracer.Start(ctx, "tool.execute", trace.WithAttributes(
		attribute.String("tool.name", t.name),
	))
	defer span.End()

	result, err := t.inner.Execute(ctx, input)
	outcome := "success"
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		outcome = "error"
	}
	t.metrics.RecordToolCall(t.name, outcome)
	return result, err
}

// endTurnSpan closes the top-level "conversation.turn" span with the turn's
// terminal outcome. state becomes the conversation.outcome attribute
// (completed/failed/canceled); errMsg (when non-empty) only sets error
// presence and OTel's own span status, never the error text itself, since a
// tool or model error message can echo back user-supplied content.
func endTurnSpan(span trace.Span, state State, errMsg string) {
	span.SetAttributes(attribute.String("conversation.outcome", string(state)))
	if errMsg != "" {
		span.SetAttributes(attribute.Bool("error", true))
		span.SetStatus(codes.Error, "")
	}
	span.End()
}
