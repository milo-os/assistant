package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/agentcore/mockmodel"
	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/usage"
)

// recordingModel captures every Request it receives and reports input tokens
// proportional to the prompt's message count, so tests can assert both that
// replayed history reached the model and that its cost shows up in metering.
type recordingModel struct {
	requests []agentcore.Request
	fail     bool
}

func (m *recordingModel) ModelID() string { return "recording" }
func (m *recordingModel) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.requests = append(m.requests, req)
	if m.fail {
		return nil, errors.New("model exploded")
	}
	return &partReader{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartTextDelta, Text: "answer " + strconv.Itoa(len(m.requests))},
		{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop,
			Usage: agentcore.Usage{Input: int64(10 * len(req.Messages)), Output: 5}},
	}}, nil
}

func runTurn(t *testing.T, conv *Conversation, params Params) Result {
	t.Helper()
	stream := conv.Run(context.Background(), params)
	drainEvents(t, stream)
	return stream.Result()
}

// TestHistoryReplayAcrossTurns is the core multi-turn proof: turn 2 in the
// same context carries turn 1's exchange in its prompt, and the replayed
// tokens are billed (input meter grows with history).
func TestHistoryReplayAcrossTurns(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: history.NewMemoryStore()})

	p := Params{ProjectName: "demo-project", ContextID: "conv-1"}
	p.UserText, p.TaskID = "my favorite pipeline is p-42", "t1"
	r1 := runTurn(t, conv, p)
	p.UserText, p.TaskID = "what did I say?", "t2"
	r2 := runTurn(t, conv, p)

	if r1.State != StateCompleted || r2.State != StateCompleted {
		t.Fatalf("states = %s, %s", r1.State, r2.State)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model saw %d requests, want 2", len(model.requests))
	}

	// Turn 1: just the user message. Turn 2: replayed pair + new message.
	if n := len(model.requests[0].Messages); n != 1 {
		t.Fatalf("turn 1 prompt has %d messages, want 1", n)
	}
	msgs := model.requests[1].Messages
	if len(msgs) != 3 {
		t.Fatalf("turn 2 prompt has %d messages, want 3 (replayed user+assistant, new user)", len(msgs))
	}
	if msgs[0].Role != agentcore.RoleUser || msgs[0].Content[0].Text != "my favorite pipeline is p-42" {
		t.Fatalf("replayed user message wrong: %+v", msgs[0])
	}
	if msgs[1].Role != agentcore.RoleAssistant || msgs[1].Content[0].Text != "answer 1" {
		t.Fatalf("replayed assistant message wrong: %+v", msgs[1])
	}
	if msgs[2].Content[0].Text != "what did I say?" {
		t.Fatalf("new user message wrong: %+v", msgs[2])
	}

	// Metering: both turns share the conversation id, and turn 2's input
	// meter reflects the replayed history (30 = 10 x 3 messages vs 10).
	inputByTurn := map[string]string{}
	for i, r := range []Result{r1, r2} {
		for _, e := range r.UsageEvents {
			if e.MeterName == usage.MeterInputTokens {
				inputByTurn[strconv.Itoa(i+1)] = e.Value
			}
			if e.Resource.Ref.Name != "conv-1" {
				t.Fatalf("turn %d event conversation resource = %q", i+1, e.Resource.Ref.Name)
			}
		}
	}
	if inputByTurn["1"] != "10" || inputByTurn["2"] != "30" {
		t.Fatalf("input meters = %v, want turn1=10 turn2=30 (history growth billed)", inputByTurn)
	}
}

// TestHistoryIsolation: a different project or context never inherits turns —
// the project is the authorization boundary even for a guessed contextId.
func TestHistoryIsolation(t *testing.T) {
	model := &recordingModel{}
	store := history.NewMemoryStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(), History: store})

	runTurn(t, conv, Params{UserText: "secret", ProjectName: "proj-a", ContextID: "shared-ctx"})

	for _, p := range []Params{
		{UserText: "hi", ProjectName: "proj-b", ContextID: "shared-ctx"},
		{UserText: "hi", ProjectName: "proj-a", ContextID: "other-ctx"},
	} {
		runTurn(t, conv, p)
		last := model.requests[len(model.requests)-1]
		if len(last.Messages) != 1 {
			t.Fatalf("(%s,%s) prompt has %d messages, want 1 (no inherited history)",
				p.ProjectName, p.ContextID, len(last.Messages))
		}
	}
}

// TestHistorySkipsFailedTurns: a failed run is not recorded, so it never
// pollutes later prompts with a half-exchange.
func TestHistorySkipsFailedTurns(t *testing.T) {
	model := &recordingModel{fail: true}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: history.NewMemoryStore()})

	p := Params{ProjectName: "demo-project", ContextID: "conv-f"}
	p.UserText = "first (fails)"
	if r := runTurn(t, conv, p); r.State != StateFailed {
		t.Fatalf("setup: want failed run, got %s", r.State)
	}

	model.fail = false
	p.UserText = "second"
	runTurn(t, conv, p)
	last := model.requests[len(model.requests)-1]
	if len(last.Messages) != 1 {
		t.Fatalf("prompt after failed turn has %d messages, want 1", len(last.Messages))
	}
}

// TestHistoryNilStoreSingleTurn pins the default: no store, no memory.
func TestHistoryNilStoreSingleTurn(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	p := Params{ProjectName: "demo-project", ContextID: "conv-1"}
	p.UserText = "one"
	runTurn(t, conv, p)
	p.UserText = "two"
	runTurn(t, conv, p)
	if n := len(model.requests[1].Messages); n != 1 {
		t.Fatalf("nil store: turn 2 prompt has %d messages, want 1", n)
	}
}

// TestHistoryTruncationBudget: oldest turns fall out of the prompt once the
// replay budget is exceeded, newest survive.
func TestHistoryTruncationBudget(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: history.NewMemoryStore(), HistoryTokenBudget: 30})

	p := Params{ProjectName: "demo-project", ContextID: "conv-t"}
	// ~100 chars/turn => ~25 estimated tokens: budget 30 fits exactly one turn.
	p.UserText = strings.Repeat("a", 92) // + "answer 1" ≈ 100 chars
	runTurn(t, conv, p)
	p.UserText = strings.Repeat("b", 92)
	runTurn(t, conv, p)
	p.UserText = "third"
	runTurn(t, conv, p)

	msgs := model.requests[2].Messages
	if len(msgs) != 3 {
		t.Fatalf("turn 3 prompt has %d messages, want 3 (1 surviving turn + new message)", len(msgs))
	}
	if !strings.HasPrefix(msgs[0].Content[0].Text, "b") {
		t.Fatalf("oldest turn should have been truncated, prompt starts with %q", msgs[0].Content[0].Text[:1])
	}
}

// TestMockRecallEndToEnd drives the real mock model through two turns with
// history — the same behavioral probe the e2e uses: turn 2 quotes turn 1.
func TestMockRecallEndToEnd(t *testing.T) {
	conv := New(Deps{Model: mockmodel.New(), ModelMode: "mock", Emitter: noopEmitter(),
		History: history.NewMemoryStore()})

	p := Params{ProjectName: "demo-project", ContextID: "conv-r"}
	p.UserText = "my favorite pipeline is p-42"
	runTurn(t, conv, p)
	p.UserText = "what did I say?"
	r2 := runTurn(t, conv, p)

	if !strings.Contains(r2.Text, `"my favorite pipeline is p-42"`) {
		t.Fatalf("recall answer should quote turn 1, got: %q", r2.Text)
	}
}
