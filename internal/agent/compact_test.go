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
	"time"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/history"
	appmetrics "github.com/milo-os/assistant/internal/metrics"
	"github.com/milo-os/assistant/internal/usage"
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
		history.NewSummaryTurn("already compacted"), 0, 0); err != nil {
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

// TestCompactSupersededByAnotherCompaction: another compaction of the same
// conversation (another replica, a racing /compact) commits while this one is
// summarizing. Its through is from the old numbering, so the store refuses
// it; Compact reports success (the history is compacted), counts the outcome
// as superseded, and no turn the other summary did not cover is lost.
func TestCompactSupersededByAnotherCompaction(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	m := appmetrics.New()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000, Metrics: m})

	params := Params{ProjectName: "demo-project", ContextID: "conv-manual-superseded"}
	fillTurns(t, conv, params, 3)

	ctx := context.Background()
	model.duringSummarize = func() {
		model.duringSummarize = nil
		w, err := store.Window(ctx, params.ProjectName, params.ContextID)
		if err != nil {
			t.Errorf("other replica Window: %v", err)
			return
		}
		if err := store.MemoryStore.Compact(ctx, params.ProjectName, params.ContextID,
			history.NewSummaryTurn("other replica"), w.Positions[0], w.Compactions); err != nil {
			t.Errorf("other replica Compact: %v", err)
		}
	}

	if err := conv.Compact(ctx, params); err != nil {
		t.Fatalf("Compact = %v, want nil when another compaction already ran", err)
	}
	if store.compactCalls != 1 {
		t.Fatalf("compactCalls = %d, want 1", store.compactCalls)
	}
	turns, err := store.Turns(ctx, params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[0].AssistantText != "other replica" || history.IsSummaryTurn(turns[1]) || history.IsSummaryTurn(turns[2]) {
		t.Fatalf("turns = %+v, want the other replica's summary then the two turns it did not cover", turns)
	}
	for outcome, want := range map[string]float64{"superseded": 1, "success": 0, "failed_open": 0} {
		if got := counterValue(t, m.CompactionTotal, "assistant_history_compaction_total", map[string]string{"outcome": outcome}); got != want {
			t.Fatalf("compaction %s count = %v, want %v", outcome, got, want)
		}
	}
}

// TestCompactThroughFollowsPositions: a window that skipped a turn covers a
// wider span of positions than it has turns, so the last summarized position
// comes from the first kept turn's position, not from counting back from the
// head (which would land on a kept turn).
func TestCompactThroughFollowsPositions(t *testing.T) {
	gap := history.Window{
		Turns:     make([]history.Turn, 4),
		Positions: []int64{1, 2, 4, 5}, // turn 3 was empty and left out
		Head:      5,
	}
	tests := []struct {
		name     string
		window   history.Window
		batchLen int
		want     int64
	}{
		{"gap inside the summarized batch", gap, 2, 3},
		{"gap before the kept turns", gap, 3, 4},
		{"nothing kept", gap, 4, 5},
		{"contiguous", history.Window{Turns: make([]history.Turn, 3), Positions: []int64{7, 8, 9}, Head: 9}, 1, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactThrough(tt.window, tt.batchLen); got != tt.want {
				t.Fatalf("compactThrough = %d, want %d", got, tt.want)
			}
		})
	}
}

// skippingStore reads a window the way the Postgres store does: a turn with
// no text is left out of Turns and Positions, though it keeps its position.
type skippingStore struct{ *history.MemoryStore }

func (s skippingStore) Window(ctx context.Context, projectName, contextID string) (history.Window, error) {
	w, err := s.MemoryStore.Window(ctx, projectName, contextID)
	if err != nil {
		return w, err
	}
	out := history.Window{Head: w.Head, Compactions: w.Compactions}
	for i, turn := range w.Turns {
		if turn.UserText == "" && turn.AssistantText == "" {
			continue
		}
		out.Turns = append(out.Turns, turn)
		out.Positions = append(out.Positions, w.Positions[i])
	}
	return out, nil
}

func (s skippingStore) Turns(ctx context.Context, projectName, contextID string) ([]history.Turn, error) {
	w, err := s.Window(ctx, projectName, contextID)
	return w.Turns, err
}

// TestCompactDeletesASkippedTurnAtTheBoundary documents what happens to a
// no-text turn the window skipped, sitting between the last summarized turn
// and the first kept one: compactThrough ends the summarized range just before
// the first kept turn, so the skipped turn is deleted with it. That is
// harmless, since a turn with no text adds nothing to replay, and every turn
// with text survives either in the summary or as itself.
func TestCompactDeletesASkippedTurnAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	raw := history.NewMemoryStore()
	store := skippingStore{MemoryStore: raw}
	conv := New(Deps{Model: &compactModel{}, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000})
	params := Params{ProjectName: "demo-project", ContextID: "conv-gap"}

	seedTurns(t, raw, params, SummaryBatchTurns)
	if err := raw.Append(ctx, params.ProjectName, params.ContextID, history.Turn{}); err != nil {
		t.Fatal(err)
	}
	kept := []history.Turn{{UserText: "kept 1", AssistantText: "a"}, {UserText: "kept 2", AssistantText: "b"}}
	for _, turn := range kept {
		if err := raw.Append(ctx, params.ProjectName, params.ContextID, turn); err != nil {
			t.Fatal(err)
		}
	}
	window, err := store.Window(ctx, params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if got := compactThrough(window, SummaryBatchTurns); got != SummaryBatchTurns+1 {
		t.Fatalf("compactThrough = %d, want %d: the skipped turn's position", got, SummaryBatchTurns+1)
	}

	if err := conv.Compact(ctx, params); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after, err := raw.Window(ctx, params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Turns) != 3 || !history.IsSummaryTurn(after.Turns[0]) ||
		after.Turns[1] != kept[0] || after.Turns[2] != kept[1] {
		t.Fatalf("stored turns = %+v, want the summary and the two kept turns, the empty one gone", after.Turns)
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

// TestCompactSucceedsWhenDigestIsNotSmaller pins the outcome that matters:
// summarize and History.Compact both succeeded and the rewrite is persisted,
// so Compact reports success even though the digest came out bigger than the
// turns it replaced. Compaction's success is "the store was rewritten", not
// "the token estimate went down".
func TestCompactSucceedsWhenDigestIsNotSmaller(t *testing.T) {
	// ~500 estimated tokens of digest against ~90 tokens of history.
	model := &compactModel{summaryText: strings.Repeat("x", 2000)}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000})

	params := Params{ProjectName: "demo-project", ContextID: "conv-bigger-digest"}
	fillTurns(t, conv, params, 3)

	before, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}

	if err := conv.Compact(context.Background(), params); err != nil {
		t.Fatalf("Compact: %v, want nil — the rewrite was persisted", err)
	}
	if store.compactCalls != 1 {
		t.Fatalf("compactCalls = %d, want 1", store.compactCalls)
	}

	after, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || !history.IsSummaryTurn(after[0]) {
		t.Fatalf("turns after compact = %+v, want a single summary turn", after)
	}
	if history.EstimateTokens(after) <= history.EstimateTokens(before) {
		t.Fatalf("fixture no longer exercises the case: after=%d tokens, before=%d",
			history.EstimateTokens(after), history.EstimateTokens(before))
	}
}

// TestCompactSingleTurnConversationSucceeds is the shape seen live: one
// ordinary turn, whose digest is comfortably larger than the turn itself. A
// single non-summary turn is still foldable (it can be arbitrarily long), so
// it compacts rather than short-circuiting to ErrNothingToCompact.
func TestCompactSingleTurnConversationSucceeds(t *testing.T) {
	model := &compactModel{summaryText: strings.Repeat("y", 300)}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000})

	params := Params{ProjectName: "demo-project", ContextID: "conv-single-turn"}
	fillTurns(t, conv, params, 1)

	if err := conv.Compact(context.Background(), params); err != nil {
		t.Fatalf("Compact: %v, want nil", err)
	}
	if store.compactCalls != 1 {
		t.Fatalf("compactCalls = %d, want 1", store.compactCalls)
	}

	after, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || !history.IsSummaryTurn(after[0]) {
		t.Fatalf("turns after compact = %+v, want a single summary turn", after)
	}
}

// TestCompactPersistFailureReturnsError guards the other half of the split:
// with the token comparison gone, a store that refuses the rewrite must
// still surface as an error on the manual path.
func TestCompactPersistFailureReturnsError(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000})

	params := Params{ProjectName: "demo-project", ContextID: "conv-persist-fail"}
	fillTurns(t, conv, params, 3)
	store.failCompact = errors.New("store exploded")

	err := conv.Compact(context.Background(), params)
	if err == nil {
		t.Fatal("Compact err = nil, want an error when the store rejects the rewrite")
	}
	if !strings.Contains(err.Error(), "store exploded") {
		t.Fatalf("err = %v, want it to carry the store failure", err)
	}
}

// TestCompactStoreFailureFallsOpenOnAutomaticPath: the automatic path must
// still never fail a turn when the store rejects the rewrite.
func TestCompactStoreFailureFallsOpenOnAutomaticPath(t *testing.T) {
	model := &compactModel{}
	store := newSpyStore()
	store.failCompact = errors.New("store exploded")
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 200})

	params := Params{ProjectName: "demo-project", ContextID: "conv-auto-store-fail"}
	res := fillTurns(t, conv, params, 7) // crosses the threshold on turn 7

	if res.State != StateCompleted {
		t.Fatalf("turn state = %s, want completed despite a failed store rewrite", res.State)
	}
	if store.compactCalls == 0 {
		t.Fatal("compactCalls = 0, want the automatic path to have attempted a rewrite")
	}
}

// TestCompactKeepsTurnAppendedDuringSummarize: a turn recorded while the
// summary is being generated (another request on the same conversation
// finishing) is not in the snapshot the summary was built from, so the
// compaction must leave it in history after the summary instead of
// overwriting it.
func TestCompactKeepsTurnAppendedDuringSummarize(t *testing.T) {
	store := newSpyStore()
	params := Params{ProjectName: "demo-project", ContextID: "conv-inflight"}
	late := history.Turn{UserText: "late question", AssistantText: "late answer"}
	model := &compactModel{}
	model.duringSummarize = func() {
		if err := store.Append(context.Background(), params.ProjectName, params.ContextID, late); err != nil {
			t.Errorf("append during summarize: %v", err)
		}
	}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 1_000_000})
	fillTurns(t, conv, params, 3)

	if err := conv.Compact(context.Background(), params); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	turns, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || !history.IsSummaryTurn(turns[0]) || turns[1] != late {
		t.Fatalf("turns = %+v, want the summary followed by the turn appended mid-compaction", turns)
	}
}

// seedTurns appends n plain turns straight to the store.
func seedTurns(t *testing.T, store history.Store, params Params, n int) []history.Turn {
	t.Helper()
	for range n {
		turn := history.Turn{UserText: "question", AssistantText: "answer"}
		if err := store.Append(context.Background(), params.ProjectName, params.ContextID, turn); err != nil {
			t.Fatal(err)
		}
	}
	turns, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	return turns
}

// TestCompactTimeoutLeavesHistoryUntouched: a summarize call that never
// returns is cut off by the compaction deadline, the error says so, and the
// store is never asked to rewrite anything.
func TestCompactTimeoutLeavesHistoryUntouched(t *testing.T) {
	model := &compactModel{blockSummarize: true}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, CompactionTimeout: 20 * time.Millisecond})
	params := Params{ProjectName: "demo-project", ContextID: "conv-timeout"}
	before := seedTurns(t, store, params, 3)

	err := conv.Compact(context.Background(), params)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0 after a timed-out summarize", store.compactCalls)
	}
	after, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || history.IsSummaryTurn(after[0]) {
		t.Fatalf("history changed by a timed-out compaction: %+v", after)
	}
}

// TestAutomaticCompactionIsBounded: the automatic path runs before the
// turn's deadline exists, so it needs its own. With a hung summarize the
// turn must still be answered (falling open to plain truncation) and the
// stored history must only gain the new turn.
func TestAutomaticCompactionIsBounded(t *testing.T) {
	model := &compactModel{blockSummarize: true}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		History: store, HistoryTokenBudget: 4, CompactionTimeout: 20 * time.Millisecond})
	params := Params{ProjectName: "demo-project", ContextID: "conv-auto-timeout"}
	before := seedTurns(t, store, params, 4)

	params.UserText = "next question"
	res := runTurn(t, conv, params)
	if res.State != StateCompleted {
		t.Fatalf("state = %s, want completed despite the hung summarize", res.State)
	}
	if !model.summarizeDeadline {
		t.Fatal("automatic summarize ran without a deadline")
	}
	if store.compactCalls != 0 {
		t.Fatalf("compactCalls = %d, want 0", store.compactCalls)
	}
	after, err := store.Turns(context.Background(), params.ProjectName, params.ContextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 || history.IsSummaryTurn(after[0]) {
		t.Fatalf("turns = %+v, want the %d seeded turns plus the new one", after, len(before))
	}
}

// TestSummarizeIsMeteredAndAttributed: the summarize call carries the same
// gateway attribution headers as a turn's model calls, and its tokens are
// emitted on the conversation's token meters — without billing a message,
// since a summary is not one the user exchanged.
func TestSummarizeIsMeteredAndAttributed(t *testing.T) {
	var (
		mu     sync.Mutex
		events []usage.CloudEvent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var batch []usage.CloudEvent
		if err := json.Unmarshal(body, &batch); err != nil {
			t.Errorf("decode usage batch: %v", err)
		}
		mu.Lock()
		events = append(events, batch...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	emitter := usage.NewEmitter(usage.EmitterConfig{GatewayURL: srv.URL, Source: "http://svc/a2a"})

	model := &compactModel{summaryUsage: agentcore.Usage{Input: 120, Output: 30, CacheRead: 7}}
	store := newSpyStore()
	conv := New(Deps{Model: model, ModelMode: "gateway", Emitter: emitter, History: store})
	params := Params{ProjectName: "demo-project", ContextID: "conv-metered"}
	seedTurns(t, store, params, 3)

	if err := conv.Compact(context.Background(), params); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	calls := model.summarizeCalls()
	if len(calls) != 1 {
		t.Fatalf("summarize calls = %d, want 1", len(calls))
	}
	want := attributionHeaders("gateway", params.ProjectName, params.ContextID)
	for k, v := range want {
		if calls[0].Headers[k] != v {
			t.Fatalf("summarize header %s = %q, want %q (all: %v)", k, calls[0].Headers[k], v, calls[0].Headers)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	got := map[string]string{}
	for _, e := range events {
		if e.Subject != "projects/"+params.ProjectName || e.Data.Resource == nil || e.Data.Resource.Name != params.ContextID {
			t.Fatalf("event attributed to %s / %+v, want the compacted conversation", e.Subject, e.Data.Resource)
		}
		if e.Data.Dimensions["model"] != model.ModelID() {
			t.Fatalf("event model = %q, want %q", e.Data.Dimensions["model"], model.ModelID())
		}
		got[e.Type] = e.Data.Value
	}
	wantMeters := map[string]string{
		usage.MeterInputTokens:     "120",
		usage.MeterOutputTokens:    "30",
		usage.MeterCacheReadTokens: "7",
	}
	if len(got) != len(wantMeters) {
		t.Fatalf("emitted meters = %v, want exactly %v (no messages meter)", got, wantMeters)
	}
	for meter, value := range wantMeters {
		if got[meter] != value {
			t.Fatalf("meter %s = %q, want %q (all: %v)", meter, got[meter], value, got)
		}
	}
}
