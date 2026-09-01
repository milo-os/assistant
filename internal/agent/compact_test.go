package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/milo-os/assistant/internal/history"
)

// TestCompactEmptyHistoryReturnsErrNothingToCompact covers both "no history
// store at all" (params.ContextID unset / History nil never applies here
// since Deps.History is set) and "store has never seen this contextId".
func TestCompactEmptyHistoryReturnsErrNothingToCompact(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200})

	err := conv.Compact(context.Background(), Params{ProjectName: "demo-project", ContextID: "conv-empty"})
	if !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("err = %v, want ErrNothingToCompact", err)
	}
	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0", store.compactCalls)
	}
}

// TestCompactSingleSummaryTurnReturnsErrNothingToCompact: a conversation
// already reduced to one summary turn (by a prior compaction) can't be
// compacted any further — there is nothing left to fold.
func TestCompactSingleSummaryTurnReturnsErrNothingToCompact(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200})

	params := Params{ProjectName: "demo-project", ContextID: "conv-single-summary"}
	if err := store.Compact(context.Background(), params.ProjectName, params.ContextID,
		history.NewSummaryTurn("already compacted"), nil); err != nil {
		t.Fatalf("setup Compact: %v", err)
	}
	store.compactCalls = 0 // reset the setup call before exercising Conversation.Compact

	err := conv.Compact(context.Background(), params)
	if !errors.Is(err, ErrNothingToCompact) {
		t.Fatalf("err = %v, want ErrNothingToCompact", err)
	}
	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0", store.compactCalls)
	}
}

// TestCompactNormalHistoryCompacts: a normal conversation compacts on
// request and the store reflects it afterward, regardless of the automatic
// threshold (HistoryTokenBudget here is generous, so maybeCompact would
// never have fired on its own).
func TestCompactNormalHistoryCompacts(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000})

	params := Params{ProjectName: "demo-project", ContextID: "conv-manual"}
	fillTurns(t, conv, params, 3)

	if err := conv.Compact(context.Background(), params); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if store.compactCalls != 1 {
		t.Fatalf("compactCalls = %d, want 1", store.compactCalls)
	}

	turns, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || !history.IsSummaryTurn(turns[0]) {
		t.Fatalf("turns after manual compact = %+v, want a single summary turn", turns)
	}
}

// TestCompactSummarizeFailureReturnsError: unlike the automatic path, a
// summarize failure on the manual path is a real error, and history is left
// untouched (no Compact call reaches the store).
func TestCompactSummarizeFailureReturnsError(t *testing.T) {
	model := &compactModel{failSummarize: true}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000})

	params := Params{ProjectName: "demo-project", ContextID: "conv-manual-fail"}
	fillTurns(t, conv, params, 3)

	before, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}

	if err := conv.Compact(context.Background(), params); err == nil {
		t.Fatal("Compact err = nil, want an error when summarize fails")
	}
	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0 (summarize failed, Compact must not be called)", store.compactCalls)
	}

	after, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("history changed after a failed manual compact: before=%d after=%d turns", len(before), len(after))
	}
}
