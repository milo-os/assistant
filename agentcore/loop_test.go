package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// ── Test helpers ──────────────────────────────────────────────

// scriptedModel returns a preset list of parts per Stream call and records
// every Request it received so tests can assert on the message history the
// loop built.
type scriptedModel struct {
	turns    [][]StreamPart
	call     int
	requests []Request
}

func (m *scriptedModel) ModelID() string { return "scripted" }

func (m *scriptedModel) Stream(_ context.Context, req Request) (StreamReader, error) {
	m.requests = append(m.requests, req)
	var parts []StreamPart
	if m.call < len(m.turns) {
		parts = m.turns[m.call]
	} else {
		// After the script is exhausted, emit a plain stop so a runaway loop
		// terminates instead of panicking the test.
		parts = []StreamPart{stepFinish(Usage{Input: 1, Output: 1}, FinishStop)}
	}
	m.call++
	return &sliceReader{parts: parts}, nil
}

type sliceReader struct {
	parts []StreamPart
	i     int
}

func (r *sliceReader) Recv() (StreamPart, error) {
	if r.i >= len(r.parts) {
		return StreamPart{}, io.EOF
	}
	p := r.parts[r.i]
	r.i++
	return p, nil
}

func (r *sliceReader) Close() error { return nil }

func textDelta(s string) StreamPart { return StreamPart{Kind: StreamPartTextDelta, Text: s} }

func toolCallPart(id, name, input string) StreamPart {
	return StreamPart{Kind: StreamPartToolCall, ToolCall: &ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}}
}

func stepFinish(u Usage, r FinishReason) StreamPart {
	return StreamPart{Kind: StreamPartStepFinish, Usage: u, FinishReason: r}
}

// funcTool is a Tool backed by a closure.
type funcTool struct {
	name string
	fn   func(ctx context.Context, input json.RawMessage) (string, error)
}

func (t funcTool) Definition() ToolDefinition {
	return ToolDefinition{Name: t.name, Description: t.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t funcTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	return t.fn(ctx, input)
}

// collect drains a stream to completion, returning every part.
func collect(t *testing.T, s StreamReader) []StreamPart {
	t.Helper()
	defer s.Close()
	var parts []StreamPart
	for {
		p, err := s.Recv()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("stream Recv: %v", err)
		}
		parts = append(parts, p)
	}
}

// terminal returns the last part, which must be Finish or Error.
func terminal(t *testing.T, parts []StreamPart) StreamPart {
	t.Helper()
	if len(parts) == 0 {
		t.Fatal("no parts emitted")
	}
	last := parts[len(parts)-1]
	if last.Kind != StreamPartFinish && last.Kind != StreamPartError {
		t.Fatalf("stream did not end with finish/error, got %q", last.Kind)
	}
	return last
}

func kinds(parts []StreamPart) []StreamPartKind {
	out := make([]StreamPartKind, len(parts))
	for i, p := range parts {
		out[i] = p.Kind
	}
	return out
}

// ── Rule 1: exit when a turn has no tool calls ────────────────

func TestRule1_ExitsWhenNoToolCalls(t *testing.T) {
	m := &scriptedModel{turns: [][]StreamPart{
		{textDelta("hello"), stepFinish(Usage{Input: 10, Output: 5}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{Model: m, Messages: []Message{UserMessage("hi")}}))

	fin := terminal(t, parts)
	if fin.Kind != StreamPartFinish || fin.FinishReason != FinishStop {
		t.Fatalf("want finish/stop, got %v/%v", fin.Kind, fin.FinishReason)
	}
	if m.call != 1 {
		t.Fatalf("model should be called exactly once, got %d", m.call)
	}
}

func TestRule1_MaxTokensWithPendingToolCallsFinishesWithoutExecuting(t *testing.T) {
	executed := false
	tool := funcTool{name: "t", fn: func(context.Context, json.RawMessage) (string, error) {
		executed = true
		return "ran", nil
	}}
	// The step emits a tool call but the model ran out of output budget:
	// finish reason is length, so the (possibly truncated) call must NOT run.
	m := &scriptedModel{turns: [][]StreamPart{
		{toolCallPart("c1", "t", `{}`), stepFinish(Usage{Input: 3, Output: 4}, FinishLength)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{"t": tool},
	}))

	fin := terminal(t, parts)
	if fin.FinishReason != FinishLength {
		t.Fatalf("want finish reason length, got %v", fin.FinishReason)
	}
	if executed {
		t.Fatal("tool must not execute when the step was truncated at max tokens")
	}
}

// ── Rule 2: all tool results batched into one message ─────────

func TestRule2_AllToolResultsInSingleMessage(t *testing.T) {
	tool := funcTool{name: "t", fn: func(_ context.Context, in json.RawMessage) (string, error) {
		return "result-for-" + string(in), nil
	}}
	m := &scriptedModel{turns: [][]StreamPart{
		{
			toolCallPart("c1", "t", `{"n":1}`),
			toolCallPart("c2", "t", `{"n":2}`),
			stepFinish(Usage{Input: 5, Output: 5}, FinishToolCalls),
		},
		{textDelta("done"), stepFinish(Usage{Input: 2, Output: 2}, FinishStop)},
	}}
	collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{"t": tool},
	}))

	// The second request must carry: [user, assistant(2 tool calls), tool(2 results)].
	if len(m.requests) != 2 {
		t.Fatalf("want 2 model calls, got %d", len(m.requests))
	}
	second := m.requests[1].Messages
	var toolMsgs []Message
	for _, msg := range second {
		if msg.Role == RoleTool {
			toolMsgs = append(toolMsgs, msg)
		}
	}
	if len(toolMsgs) != 1 {
		t.Fatalf("tool results must be in exactly one message, got %d tool messages", len(toolMsgs))
	}
	if got := len(toolMsgs[0].Content); got != 2 {
		t.Fatalf("the single tool message must hold both results, got %d", got)
	}
}

// ── Rule 3: step limit ends gracefully ────────────────────────

func TestRule3_StepLimitEndsGracefully(t *testing.T) {
	tool := funcTool{name: "t", fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	// A model that always asks for another tool call would loop forever.
	looping := [][]StreamPart{}
	for i := 0; i < 10; i++ {
		looping = append(looping, []StreamPart{
			toolCallPart("c", "t", `{}`), stepFinish(Usage{Input: 1, Output: 1}, FinishToolCalls),
		})
	}
	m := &scriptedModel{turns: looping}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{"t": tool}, StepLimit: 3,
	}))

	fin := terminal(t, parts)
	if fin.FinishReason != FinishStepLimit {
		t.Fatalf("want finish reason step-limit, got %v", fin.FinishReason)
	}
	if m.call != 3 {
		t.Fatalf("model must be called exactly StepLimit times, got %d", m.call)
	}
	// Usage is still aggregated across the 3 steps.
	if fin.TotalUsage.Input != 3 {
		t.Fatalf("want aggregated input 3, got %d", fin.TotalUsage.Input)
	}
}

func TestRule3_DefaultStepLimitIsEight(t *testing.T) {
	tool := funcTool{name: "t", fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	m := &scriptedModel{} // exhausted script → always emits stop after step 1
	// Force endless tool calls by scripting more than 8 tool-call turns.
	for i := 0; i < 20; i++ {
		m.turns = append(m.turns, []StreamPart{toolCallPart("c", "t", `{}`), stepFinish(Usage{Input: 1}, FinishToolCalls)})
	}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{"t": tool},
	}))
	if fin := terminal(t, parts); fin.FinishReason != FinishStepLimit {
		t.Fatalf("want step-limit at default, got %v", fin.FinishReason)
	}
	if m.call != DefaultStepLimit {
		t.Fatalf("default step limit should be %d, got %d calls", DefaultStepLimit, m.call)
	}
}

// ── Rule 4: usage aggregation incl. cache, per-step hook ───────

func TestRule4_AggregatesUsageIncludingCacheAndFiresStepHook(t *testing.T) {
	tool := funcTool{name: "t", fn: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
	m := &scriptedModel{turns: [][]StreamPart{
		{toolCallPart("c1", "t", `{}`), stepFinish(Usage{Input: 100, Output: 30, CacheRead: 20, CacheWrite: 15}, FinishToolCalls)},
		{textDelta("done"), stepFinish(Usage{Input: 50, Output: 10, CacheRead: 5, CacheWrite: 0}, FinishStop)},
	}}

	var steps []Usage
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{"t": tool},
		OnStep: func(u Usage) { steps = append(steps, u) },
	}))

	fin := terminal(t, parts)
	want := Usage{Input: 150, Output: 40, CacheRead: 25, CacheWrite: 15}
	if fin.TotalUsage != want {
		t.Fatalf("aggregated usage = %+v, want %+v", fin.TotalUsage, want)
	}
	if len(steps) != 2 {
		t.Fatalf("OnStep should fire once per step, got %d", len(steps))
	}
	if steps[0].CacheWrite != 15 || steps[1].CacheRead != 5 {
		t.Fatalf("per-step cache usage dropped: %+v", steps)
	}
}

// ── Rule 5: tool errors and unknown tools feed back, no abort ─

func TestRule5_ToolExecutionErrorBecomesErrorResult(t *testing.T) {
	failing := funcTool{name: "boom", fn: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("kaboom")
	}}
	m := &scriptedModel{turns: [][]StreamPart{
		{toolCallPart("c1", "boom", `{}`), stepFinish(Usage{}, FinishToolCalls)},
		{textDelta("recovered"), stepFinish(Usage{}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{"boom": failing},
	}))

	var toolResult *ToolResult
	for _, p := range parts {
		if p.Kind == StreamPartToolResult {
			toolResult = p.ToolResult
		}
	}
	if toolResult == nil || !toolResult.IsError || toolResult.Content != "kaboom" {
		t.Fatalf("want error tool result 'kaboom', got %+v", toolResult)
	}
	if fin := terminal(t, parts); fin.Kind != StreamPartFinish {
		t.Fatal("a tool error must not abort the run")
	}
	// The error result was fed back and the model produced a final answer.
	if m.call != 2 {
		t.Fatalf("loop should continue after tool error, got %d calls", m.call)
	}
}

func TestRule5_UnknownToolBecomesErrorResult(t *testing.T) {
	m := &scriptedModel{turns: [][]StreamPart{
		{toolCallPart("c1", "ghost", `{}`), stepFinish(Usage{}, FinishToolCalls)},
		{textDelta("ok"), stepFinish(Usage{}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{},
	}))

	var toolResult *ToolResult
	for _, p := range parts {
		if p.Kind == StreamPartToolResult {
			toolResult = p.ToolResult
		}
	}
	if toolResult == nil || !toolResult.IsError || toolResult.Content != "unknown tool: ghost" {
		t.Fatalf("want unknown-tool error result, got %+v", toolResult)
	}
	if fin := terminal(t, parts); fin.Kind != StreamPartFinish {
		t.Fatal("an unknown tool must not abort the run")
	}
}

// ── Rule 6: unified stream part kinds ─────────────────────────

func TestRule6_EmitsUnifiedStreamParts(t *testing.T) {
	tool := funcTool{name: "t", fn: func(context.Context, json.RawMessage) (string, error) { return "tool-out", nil }}
	m := &scriptedModel{turns: [][]StreamPart{
		{textDelta("thinking "), toolCallPart("c1", "t", `{}`), stepFinish(Usage{Input: 1}, FinishToolCalls)},
		{textDelta("answer"), stepFinish(Usage{Input: 1}, FinishStop)},
	}}
	parts := collect(t, Run(context.Background(), LoopOptions{
		Model: m, Messages: []Message{UserMessage("go")}, Tools: ToolSet{"t": tool},
	}))

	seen := map[StreamPartKind]bool{}
	for _, k := range kinds(parts) {
		seen[k] = true
	}
	for _, want := range []StreamPartKind{
		StreamPartTextDelta, StreamPartToolCall, StreamPartToolResult, StreamPartStepFinish, StreamPartFinish,
	} {
		if !seen[want] {
			t.Fatalf("missing stream part kind %q; got %v", want, kinds(parts))
		}
	}
}

func TestModelErrorSurfacesAsErrorPart(t *testing.T) {
	m := &errModel{err: errors.New("transport exploded")}
	parts := collect(t, Run(context.Background(), LoopOptions{Model: m, Messages: []Message{UserMessage("go")}}))
	fin := terminal(t, parts)
	if fin.Kind != StreamPartError || fin.Err == nil {
		t.Fatalf("want error part, got %+v", fin)
	}
}

type errModel struct{ err error }

func (m *errModel) ModelID() string { return "err" }
func (m *errModel) Stream(context.Context, Request) (StreamReader, error) {
	return nil, m.err
}

func TestCanceledContextFinishesCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &scriptedModel{turns: [][]StreamPart{{textDelta("x"), stepFinish(Usage{}, FinishStop)}}}
	parts := collect(t, Run(ctx, LoopOptions{Model: m, Messages: []Message{UserMessage("go")}}))
	if fin := terminal(t, parts); fin.FinishReason != FinishCanceled {
		t.Fatalf("want canceled finish, got %v", fin.FinishReason)
	}
}
