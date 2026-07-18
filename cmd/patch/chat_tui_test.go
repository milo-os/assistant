package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	spin "charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// newTestModel builds a chatModel suitable for driving Update/onKey directly,
// with no a2a client and no kubeconfig — tests here only exercise key
// handling and state transitions, never the goroutines that would touch the
// network (stream) or shell out (kubectl), since those Cmds are captured but
// never invoked.
func newTestModel() *chatModel {
	st := newStyles(false)
	ti := textinput.New()
	ti.SetVirtualCursor(true)
	ti.Focus()
	sp := spin.New(spin.WithSpinner(spin.Dot))
	return &chatModel{
		project: "demo-project",
		ti:      ti,
		sp:      sp,
		st:      st,
		width:   120,
	}
}

func key(code rune, text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Text: text}
}

func typeText(t *testing.T, m *chatModel, s string) {
	t.Helper()
	for _, r := range s {
		m.onKey(key(r, string(r)))
	}
}

// ── autocomplete ────────────────────────────────────────────────

func TestSuggestionsForBareSlash(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/")
	got := m.currentSuggestions()
	if len(got) != len(commandNames) {
		t.Fatalf("bare '/' should suggest every command, got %v", got)
	}
}

func TestSuggestionsNarrowOnPrefix(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/re")
	got := m.currentSuggestions()
	if len(got) != 1 || got[0] != "/resume" {
		t.Fatalf("'/re' should suggest only /resume, got %v", got)
	}
}

func TestSuggestionsMultipleMatches(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/e")
	got := m.currentSuggestions()
	if len(got) != 2 || got[0] != "/export" || got[1] != "/exit" {
		t.Fatalf("'/e' should suggest /export and /exit in commandNames order, got %v", got)
	}
}

func TestNoSuggestionsForNonSlashOrExactCommand(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "hello")
	if got := m.currentSuggestions(); got != nil {
		t.Fatalf("plain text should suggest nothing, got %v", got)
	}
	m2 := newTestModel()
	typeText(t, m2, "/resume")
	if got := m2.currentSuggestions(); got != nil {
		t.Fatalf("an exact command should suggest nothing (it's already complete), got %v", got)
	}
}

func TestTabCompletesWithoutSubmitting(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/re")
	m.onKey(key(tea.KeyTab, ""))
	if m.ti.Value() != "/resume" {
		t.Fatalf("tab should complete input to /resume, got %q", m.ti.Value())
	}
	if m.picker.open {
		t.Fatal("tab must only complete, never execute")
	}
}

func TestUpDownCycleSuggestion(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/e") // /export, /exit
	if m.suggestIndex != 0 {
		t.Fatalf("suggestIndex should start at 0, got %d", m.suggestIndex)
	}
	m.onKey(key(tea.KeyDown, ""))
	if m.suggestIndex != 1 {
		t.Fatalf("down should advance to index 1, got %d", m.suggestIndex)
	}
	m.onKey(key(tea.KeyDown, ""))
	if m.suggestIndex != 0 {
		t.Fatalf("down should wrap back to index 0, got %d", m.suggestIndex)
	}
	m.onKey(key(tea.KeyUp, ""))
	if m.suggestIndex != 1 {
		t.Fatalf("up should wrap to index 1, got %d", m.suggestIndex)
	}
}

func TestSuggestIndexResetsOnNewPrefix(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/e")
	m.onKey(key(tea.KeyDown, "")) // highlight /exit
	if m.suggestIndex != 1 {
		t.Fatalf("setup: want index 1, got %d", m.suggestIndex)
	}
	typeText(t, m, "x") // narrows to "/ex" -> still matches /exit only
	if m.suggestIndex != 0 {
		t.Fatalf("typing should reset suggestIndex to 0, got %d", m.suggestIndex)
	}
}

func TestEnterResolvesUniquePrefixToCommand(t *testing.T) {
	m := newTestModel()
	m.contextID = "some-context"
	m.turns = []string{"leftover"}
	typeText(t, m, "/cl") // only /clear matches
	m.onKey(key(tea.KeyEnter, ""))
	if m.contextID != "" || m.turns != nil {
		t.Fatalf("enter on unique prefix '/cl' should have run /clear, got contextID=%q turns=%v", m.contextID, m.turns)
	}
}

func TestEnterResolvesHighlightedSuggestion(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/e") // /export, /exit
	m.onKey(key(tea.KeyDown, ""))
	got, cmd := m.onKey(key(tea.KeyEnter, ""))
	if cmd == nil {
		t.Fatal("enter on /exit (highlighted) should quit, want a non-nil tea.Quit cmd")
	}
	_ = got
}

func TestSuggestionBarRendersAndDismissesCorrectly(t *testing.T) {
	m := newTestModel()
	if m.suggestionBar() != "" {
		t.Fatal("empty input should show no suggestion bar")
	}
	typeText(t, m, "/re")
	if m.suggestionBar() == "" {
		t.Fatal("'/re' should render a suggestion bar")
	}
	typeText(t, m, "sume") // now exactly "/resume"
	if m.suggestionBar() != "" {
		t.Fatal("an exact, fully-typed command should not show a suggestion bar")
	}
}

// ── picker preview ────────────────────────────────────────────

func testConversation(name string) assistantv1alpha1.Conversation {
	c := assistantv1alpha1.Conversation{}
	c.Name = name
	return c
}

func TestPickerListLoadTriggersInitialPreviewFetch(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState()
	_, cmd := m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{
		testConversation("conv-a"), testConversation("conv-b"),
	}})
	if cmd == nil {
		t.Fatal("loading a non-empty list should kick off a preview fetch for the top item")
	}
	if !m.picker.previewPending["conv-a"] {
		t.Fatal("cursor starts at 0 (conv-a); its preview should be marked pending")
	}
	if m.picker.previewPending["conv-b"] {
		t.Fatal("conv-b is not selected; its preview should not be requested yet")
	}
}

func TestPickerCursorMoveTriggersPreviewFetchOnce(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState()
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a"), testConversation("conv-b")}
	m.picker.loading = false

	_, cmd := m.onPickerKey(key(tea.KeyDown, ""))
	if cmd == nil {
		t.Fatal("moving onto conv-b for the first time should fetch its preview")
	}
	if !m.picker.previewPending["conv-b"] {
		t.Fatal("conv-b should be marked pending after the move")
	}

	// Simulate the fetch completing, then move back and forth: no re-fetch.
	m.Update(pickerPreviewMsg{contextID: "conv-b", items: []assistantv1alpha1.ConversationMessage{
		{Role: "user", Content: "hi"},
	}})
	if m.picker.previewPending["conv-b"] {
		t.Fatal("pending should clear once the preview msg lands")
	}
	_, cmd = m.onPickerKey(key(tea.KeyUp, "")) // back to conv-a (still pending from initial state? not set here)
	_ = cmd
	_, cmd = m.onPickerKey(key(tea.KeyDown, "")) // back to conv-b, already cached
	if cmd != nil {
		t.Fatal("revisiting a cached conversation must not re-fetch its preview")
	}
}

func TestPickerPreviewErrorIsCachedNotRetried(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState()
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.loading = false
	m.picker.previewPending["conv-a"] = true

	m.Update(pickerPreviewMsg{contextID: "conv-a", err: errors.New("boom")})
	if m.picker.previewErr["conv-a"] != "boom" {
		t.Fatalf("preview error should be cached, got %q", m.picker.previewErr["conv-a"])
	}
	if cmd := m.maybeLoadPreview(); cmd != nil {
		t.Fatal("an already-errored preview should not be retried automatically")
	}
}

func TestPreviewPaneRendersCachedMessages(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState()
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.preview["conv-a"] = []assistantv1alpha1.ConversationMessage{
		{Role: "user", Content: "diagnose pipeline p-7"},
		{Role: "assistant", Content: "found the issue"},
	}
	out := m.previewPane()
	if !containsAll(out, "diagnose pipeline p-7", "found the issue") {
		t.Fatalf("preview pane should show the cached transcript, got:\n%s", out)
	}
}

func TestPreviewPaneRendersSummaryDistinctly(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState()
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.preview["conv-a"] = []assistantv1alpha1.ConversationMessage{
		{Role: "summary", Content: "compacted digest of earlier turns"},
		{Role: "user", Content: "what's next"},
	}
	out := m.previewPane()
	if !containsAll(out, "Summary", "compacted digest of earlier turns") {
		t.Fatalf("preview pane should label a summary message distinctly, got:\n%s", out)
	}
}

func TestPickerTranscriptMsgRendersSummaryDistinctly(t *testing.T) {
	m := newTestModel()
	_, _ = m.Update(pickerTranscriptMsg{
		contextID: "conv-a",
		items: []assistantv1alpha1.ConversationMessage{
			{Role: "summary", Content: "compacted digest of earlier turns"},
			{Role: "user", Content: "what's next"},
			{Role: "assistant", Content: "here's the plan"},
		},
	})
	if len(m.raw) != 3 || m.raw[0].role != "summary" {
		t.Fatalf("resumed transcript should preserve the summary role, got: %+v", m.raw)
	}
	joined := strings.Join(m.turns, "\n")
	if !containsAll(joined, "Summary", "compacted digest of earlier turns") {
		t.Fatalf("resumed transcript should render the summary turn distinctly, got:\n%s", joined)
	}
}

func TestExportTranscriptLabelsSummaryDistinctly(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	m := newTestModel()
	m.raw = []transcriptTurn{
		{role: "summary", content: "compacted digest of earlier turns"},
		{role: "user", content: "what's next"},
	}
	name, err := m.exportTranscript()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(data), "## Summary", "compacted digest of earlier turns") {
		t.Fatalf("exported transcript should label the summary turn distinctly, got:\n%s", data)
	}
}

func TestPreviewPaneShowsLoadingThenContent(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState()
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.previewPending["conv-a"] = true
	if out := m.previewPane(); !containsAll(out, "loading") {
		t.Fatalf("pending preview should show a loading state, got:\n%s", out)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
