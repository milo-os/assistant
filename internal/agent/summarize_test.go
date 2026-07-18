package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/history"
)

// compactModel answers every plain conversational turn with "answer N" and
// every summarize call (recognized by its fixed system prompt) with a
// canned digest, optionally failing summarize calls instead. It records
// every request it saw so tests can inspect what the summarize helper sent.
type compactModel struct {
	requests      []agentcore.Request
	failSummarize bool
	summaryText   string // defaults to "digest" when empty
}

func (m *compactModel) ModelID() string { return "compact" }

func (m *compactModel) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.requests = append(m.requests, req)
	if req.System == summarizeSystemPrompt {
		if m.failSummarize {
			return nil, errors.New("summarize exploded")
		}
		text := m.summaryText
		if text == "" {
			text = "digest"
		}
		return &partReader{parts: []agentcore.StreamPart{
			{Kind: agentcore.StreamPartTextDelta, Text: text},
			{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop},
		}}, nil
	}
	return &partReader{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartTextDelta, Text: "answer " + strconv.Itoa(len(m.requests))},
		{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop,
			Usage: agentcore.Usage{Input: 10, Output: 5}},
	}}, nil
}

// summarizeCalls returns the requests the model received for summarize calls
// specifically (as opposed to ordinary conversational turns), in order.
func (m *compactModel) summarizeCalls() []agentcore.Request {
	var out []agentcore.Request
	for _, r := range m.requests {
		if r.System == summarizeSystemPrompt {
			out = append(out, r)
		}
	}
	return out
}

// spyStore wraps a *history.MemoryStore, counting and recording Compact
// calls while delegating everything (including the real Compact behavior) to
// the embedded store.
type spyStore struct {
	*history.MemoryStore
	compactCalls int
	lastSummary  history.Turn
	lastKeep     []history.Turn
}

func newSpyStore() *spyStore {
	return &spyStore{MemoryStore: history.NewMemoryStore()}
}

func (s *spyStore) Compact(ctx context.Context, projectName, contextID string, summary history.Turn, keep []history.Turn) error {
	s.compactCalls++
	s.lastSummary = summary
	s.lastKeep = append([]history.Turn(nil), keep...)
	return s.MemoryStore.Compact(ctx, projectName, contextID, summary, keep)
}

// fillTurns runs n turns of a fixed size (~30 estimated tokens: 112-char user
// text + the model's short canned answer) against conv/params, returning the
// final result.
func fillTurns(t *testing.T, conv *Conversation, params Params, n int) Result {
	t.Helper()
	var res Result
	for i := 0; i < n; i++ {
		params.UserText = strings.Repeat("a", 112)
		params.TaskID = "t" + strconv.Itoa(i)
		res = runTurn(t, conv, params)
		if res.State != StateCompleted {
			t.Fatalf("turn %d state = %s (err=%s)", i, res.State, res.Error)
		}
	}
	return res
}

// TestCompactionBelowThresholdDoesNotFire pins today's behavior unchanged for
// the overwhelming majority of conversations that never approach the budget:
// well under CompactionThresholdRatio, no summarize call and no Compact call
// happen at all.
func TestCompactionBelowThresholdDoesNotFire(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200})

	params := Params{ProjectName: "demo-project", ContextID: "conv-below"}
	fillTurns(t, conv, params, 3) // ~3*30 = 90 tokens, well under 0.8*200=160

	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0 below threshold", store.compactCalls)
	}
	if len(model.summarizeCalls()) != 0 {
		t.Fatalf("summarize calls = %d, want 0 below threshold", len(model.summarizeCalls()))
	}
}

// TestCompactionFiresAboveThreshold: once stored turns cross
// CompactionThresholdRatio of the budget, loadHistory synchronously
// summarizes and calls History.Compact exactly once, before the turn that
// crossed it is answered.
func TestCompactionFiresAboveThreshold(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200})

	params := Params{ProjectName: "demo-project", ContextID: "conv-above"}
	// 6 turns of ~30 tokens each = ~180 > threshold (160): triggers on turn 7's
	// loadHistory, before turn 7 is answered.
	fillTurns(t, conv, params, 7)

	if store.compactCalls != 1 {
		t.Fatalf("compactCalls = %d, want exactly 1", store.compactCalls)
	}
	if !history.IsSummaryTurn(store.lastSummary) {
		t.Fatalf("Compact's summary arg is not a summary turn: %+v", store.lastSummary)
	}
	if len(model.summarizeCalls()) != 1 {
		t.Fatalf("summarize calls = %d, want exactly 1", len(model.summarizeCalls()))
	}
	// The stored history afterward starts with the summary turn.
	turns, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || !history.IsSummaryTurn(turns[0]) {
		t.Fatalf("stored turns after compaction = %+v, want to start with a summary turn", turns)
	}
}

// TestSummarizationDisabledSkipsCompaction: the escape hatch behaves exactly
// as if summarization did not exist, even once the conversation is well past
// the compaction threshold.
func TestSummarizationDisabledSkipsCompaction(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200, SummarizationDisabled: true})

	params := Params{ProjectName: "demo-project", ContextID: "conv-disabled"}
	fillTurns(t, conv, params, 7) // same shape as the firing test above

	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0 with SummarizationDisabled", store.compactCalls)
	}
	if len(model.summarizeCalls()) != 0 {
		t.Fatalf("summarize calls = %d, want 0 with SummarizationDisabled", len(model.summarizeCalls()))
	}
}

// TestSummarizeFailureFallsOpenToTruncate: when the summarize model call
// errors, the turn must still complete (plain Truncate, no Compact call) —
// a user must never see a failed turn because history maintenance failed.
func TestSummarizeFailureFallsOpenToTruncate(t *testing.T) {
	model := &compactModel{failSummarize: true}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200})

	params := Params{ProjectName: "demo-project", ContextID: "conv-failopen"}
	res := fillTurns(t, conv, params, 7) // crosses the threshold on turn 7

	if res.State != StateCompleted {
		t.Fatalf("turn state = %s, want completed despite a failed summarize call", res.State)
	}
	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0 (summarize failed, Compact must not be called)", store.compactCalls)
	}
	// History was neither compacted nor lost: the raw turns are still there.
	turns, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || history.IsSummaryTurn(turns[0]) {
		t.Fatalf("turns after a failed summarize = %+v, want plain uncompacted turns", turns)
	}
}

// TestSecondCompactionFoldsInExistingSummary proves anchored iterative
// summarization: once a conversation has already been compacted once, a
// second compaction's summarize call includes the FIRST summary turn's
// digest in its prompt, not just the newly-aging raw turns.
func TestSecondCompactionFoldsInExistingSummary(t *testing.T) {
	model := &compactModel{summaryText: "first digest"}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200})

	params := Params{ProjectName: "demo-project", ContextID: "conv-anchor"}
	// First compaction fires on turn 7 (as in TestCompactionFiresAboveThreshold).
	fillTurns(t, conv, params, 7)
	if store.compactCalls != 1 {
		t.Fatalf("setup: compactCalls after first batch = %d, want 1", store.compactCalls)
	}

	// After compaction the store holds just the summary (~7 estimated tokens).
	// 6 more ~30-token turns push it back over the 160 threshold, triggering a
	// second compaction on the next (14th overall) turn.
	fillTurns(t, conv, params, 6)
	if store.compactCalls != 2 {
		t.Fatalf("compactCalls after second batch = %d, want 2", store.compactCalls)
	}

	calls := model.summarizeCalls()
	if len(calls) != 2 {
		t.Fatalf("summarize calls = %d, want 2", len(calls))
	}
	second := calls[1]
	if len(second.Messages) < 2 {
		t.Fatalf("second summarize call has %d messages, want at least 2 (anchored summary pair + new turns)",
			len(second.Messages))
	}

	// The rendered marker+digest pair the first summary turn produces, to
	// compare against without duplicating history's unexported marker text.
	wantMarker := history.Messages([]history.Turn{history.NewSummaryTurn("first digest")})

	if second.Messages[0].Content[0].Text != wantMarker[0].Content[0].Text {
		t.Fatalf("second summarize prompt does not start with the summary marker: got %q, want %q",
			second.Messages[0].Content[0].Text, wantMarker[0].Content[0].Text)
	}
	if second.Messages[1].Content[0].Text != "first digest" {
		t.Fatalf("second summarize prompt does not carry forward the first digest: got %q",
			second.Messages[1].Content[0].Text)
	}
}
