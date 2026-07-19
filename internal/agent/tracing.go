package agent

import (
	"context"
	"encoding/json"
	"io"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/milo-os/assistant/agentcore"
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
// whole streamed call, not just the time to open it). A nil model returns
// nil so callers can pass through an unset Deps.Model unchanged.
func tracedModel(m agentcore.Model) agentcore.Model {
	if m == nil {
		return nil
	}
	return &tracingModel{inner: m}
}

type tracingModel struct {
	inner agentcore.Model
}

func (m *tracingModel) ModelID() string { return m.inner.ModelID() }

func (m *tracingModel) Stream(ctx context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	ctx, span := tracer.Start(ctx, "model.stream", trace.WithAttributes(
		attribute.String("model.id", m.inner.ModelID()),
	))
	reader, err := m.inner.Stream(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return nil, err
	}
	return &tracingStreamReader{inner: reader, span: span}, nil
}

// tracingStreamReader ends its model.stream span once the wrapped stream is
// exhausted (Recv returns io.EOF or a terminal error) or explicitly Closed,
// whichever happens first. It is not safe for concurrent use, matching
// [agentcore.StreamReader]'s existing single-consumer contract.
type tracingStreamReader struct {
	inner agentcore.StreamReader
	span  trace.Span
	ended bool
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
	if err != nil && err != io.EOF {
		r.span.RecordError(err)
		r.span.SetStatus(codes.Error, err.Error())
	}
	r.span.End()
}

// tracedTools wraps every tool in a set so each Execute call gets a
// "tool.execute" span carrying only the tool's name — never its input
// (tool-call arguments, which may carry user-supplied data) or its textual
// result.
func tracedTools(tools agentcore.ToolSet) agentcore.ToolSet {
	if len(tools) == 0 {
		return tools
	}
	wrapped := make(agentcore.ToolSet, len(tools))
	for name, t := range tools {
		wrapped[name] = &tracingTool{inner: t, name: name}
	}
	return wrapped
}

type tracingTool struct {
	inner agentcore.Tool
	name  string
}

func (t *tracingTool) Definition() agentcore.ToolDefinition { return t.inner.Definition() }

func (t *tracingTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	ctx, span := tracer.Start(ctx, "tool.execute", trace.WithAttributes(
		attribute.String("tool.name", t.name),
	))
	defer span.End()

	result, err := t.inner.Execute(ctx, input)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
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
