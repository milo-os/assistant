package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/internal/usage"
)

// scriptModel is a multi-step [agentcore.Model] whose Stream can either return
// a preset list of parts or fail outright (Stream error), one entry per call.
type scriptModel struct {
	turns []scriptTurn
	call  int
}

type scriptTurn struct {
	parts []agentcore.StreamPart
	err   error // if set, Stream returns this error for this call
}

func (m *scriptModel) ModelID() string { return "script" }

func (m *scriptModel) Stream(_ context.Context, _ agentcore.Request) (agentcore.StreamReader, error) {
	i := m.call
	m.call++
	if i >= len(m.turns) {
		// Safety net so an over-driven loop stops instead of panicking.
		return &partReader{parts: []agentcore.StreamPart{
			{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop},
		}}, nil
	}
	tn := m.turns[i]
	if tn.err != nil {
		return nil, tn.err
	}
	return &partReader{parts: tn.parts}, nil
}

// firstStepThenFail scripts one completed tool-call step (42/23 tokens, an
// unknown tool that feeds an error result back and continues the loop) followed
// by a second step that terminates the model stream with fail.
func firstStepThenFail(fail scriptTurn) *scriptModel {
	return &scriptModel{turns: []scriptTurn{
		{parts: []agentcore.StreamPart{
			{Kind: agentcore.StreamPartToolCall, ToolCall: &agentcore.ToolCall{ID: "c1", Name: "x", Input: json.RawMessage(`{}`)}},
			{Kind: agentcore.StreamPartStepFinish, Usage: agentcore.Usage{Input: 42, Output: 23}, FinishReason: agentcore.FinishToolCalls},
		}},
		fail,
	}}
}

func meterValues(events []usage.Event) map[string]string {
	byMeter := map[string]string{}
	for _, e := range events {
		byMeter[e.MeterName] = e.Value
	}
	return byMeter
}

// TestStreamErrorBillsCompletedStepUsage pins finding #3: usage from steps that
// completed before a mid-run Stream failure must survive into metering. Without
// the fix the terminal error part carries no usage, so a run that already ran a
// billed inference meters zero tokens.
func TestStreamErrorBillsCompletedStepUsage(t *testing.T) {
	model := firstStepThenFail(scriptTurn{err: errors.New("provider exploded")})
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(context.Background(), Params{UserText: "go", ProjectName: "demo-project", ContextID: "c"})
	drainEvents(t, stream)
	res := stream.Result()

	if res.State != StateFailed {
		t.Fatalf("state = %s, want failed (err=%s)", res.State, res.Error)
	}
	if !strings.Contains(res.Error, "provider exploded") {
		t.Fatalf("error = %q, want it to carry the provider failure", res.Error)
	}
	byMeter := meterValues(res.UsageEvents)
	if byMeter[usage.MeterInputTokens] != "42" || byMeter[usage.MeterOutputTokens] != "23" {
		t.Fatalf("completed-step usage lost on failure: input=%q output=%q, want 42/23",
			byMeter[usage.MeterInputTokens], byMeter[usage.MeterOutputTokens])
	}
}

// TestInStreamErrorPartFailsTheRun pins finding #2: a StreamPartError delivered
// as a stream part (how every adapter reports a failure) must fail the run.
// Without the fix runStep's default branch drops it, the loop reports a normal
// stop, and the run is marked completed — a failed inference billed as success.
func TestInStreamErrorPartFailsTheRun(t *testing.T) {
	model := firstStepThenFail(scriptTurn{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartError, FinishReason: agentcore.FinishError, Err: errors.New("boom")},
	}})
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(context.Background(), Params{UserText: "go", ProjectName: "demo-project", ContextID: "c"})
	drainEvents(t, stream)
	res := stream.Result()

	if res.State != StateFailed {
		t.Fatalf("state = %s, want failed — a dropped stream error was billed as a completion", res.State)
	}
	if !strings.Contains(res.Error, "boom") {
		t.Fatalf("error = %q, want it to carry the stream failure", res.Error)
	}
	byMeter := meterValues(res.UsageEvents)
	if byMeter[usage.MeterInputTokens] != "42" || byMeter[usage.MeterOutputTokens] != "23" {
		t.Fatalf("completed-step usage lost on failure: input=%q output=%q, want 42/23",
			byMeter[usage.MeterInputTokens], byMeter[usage.MeterOutputTokens])
	}
}

// TestInStreamCancellationIsCanceledNotFailed pins that a canceled model stream
// (context.Canceled surfaced as a stream error part) ends the run canceled —
// distinct from a failure — while still billing the tokens already consumed.
func TestInStreamCancellationIsCanceledNotFailed(t *testing.T) {
	model := firstStepThenFail(scriptTurn{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartError, FinishReason: agentcore.FinishError, Err: context.Canceled},
	}})
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(context.Background(), Params{UserText: "go", ProjectName: "demo-project", ContextID: "c"})
	drainEvents(t, stream)
	res := stream.Result()

	if res.State != StateCanceled {
		t.Fatalf("state = %s, want canceled", res.State)
	}
	byMeter := meterValues(res.UsageEvents)
	if byMeter[usage.MeterInputTokens] != "42" || byMeter[usage.MeterOutputTokens] != "23" {
		t.Fatalf("canceled run must still bill consumed tokens: input=%q output=%q, want 42/23",
			byMeter[usage.MeterInputTokens], byMeter[usage.MeterOutputTokens])
	}
}

// TestFailedBeforeAnyStepBillsNothing pins finding #10 end to end: a run that
// breaks before any model inference has zero token usage and must bill nothing
// at all — not even the messages meter (the pre-fix behavior billed messages=1
// for a run that produced no tokens).
func TestFailedBeforeAnyStepBillsNothing(t *testing.T) {
	model := &scriptModel{turns: []scriptTurn{{err: errors.New("down before first step")}}}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(context.Background(), Params{UserText: "go", ProjectName: "demo-project", ContextID: "c"})
	drainEvents(t, stream)
	res := stream.Result()

	if res.State != StateFailed {
		t.Fatalf("state = %s, want failed", res.State)
	}
	if len(res.UsageEvents) != 0 {
		t.Fatalf("a run that failed before any model step must bill nothing, got %d events: %+v",
			len(res.UsageEvents), res.UsageEvents)
	}
}

// TestFinalizeEmitsExactlyOncePerTurn pins finding #12: a turn emits its usage
// exactly once, and a drain-to-EOF followed by Close must NOT double-emit. A
// double-emit (finalize running twice) would bill every turn twice.
//
// This uses the tool-free mock completion so the pin depends only on finalize's
// once-guard, not on any provider tool round-trip: a single mock step produces
// the known fixed set — 42 input + 23 output + 1 messages = 3 events in one
// Emit batch. (The tool-invocation exact count is pinned in the e2e driver,
// where a real MCP round-trip runs.)
func TestFinalizeEmitsExactlyOncePerTurn(t *testing.T) {
	var (
		mu          sync.Mutex
		posts       int
		totalEvents int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var batch []map[string]any
		_ = json.Unmarshal(body, &batch)
		mu.Lock()
		posts++
		totalEvents += len(batch)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	emitter := usage.NewEmitter(usage.EmitterConfig{GatewayURL: srv.URL, Source: "http://svc/a2a"})

	conv := New(Deps{Model: mockmodel.New(), ModelMode: "mock", Emitter: emitter})
	stream := conv.Run(context.Background(), Params{
		UserText: "hello there", ProjectName: "demo-project", ContextID: "conv-1",
	})
	drainEvents(t, stream) // reaching io.EOF finalizes (and emits) exactly once
	if stream.Result().State != StateCompleted {
		t.Fatalf("state = %s, want completed", stream.Result().State)
	}
	// A Close after a full drain must be a no-op — it must NOT emit again.
	_ = stream.Close()

	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("usage must be emitted exactly once per turn, got %d Emit POSTs", posts)
	}
	if totalEvents != 3 {
		t.Fatalf("a completed turn must emit exactly 3 usage events (42 in, 23 out, 1 messages), got %d", totalEvents)
	}
}
