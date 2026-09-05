package patchcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	spin "charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// newTestModel builds a chatModel suitable for driving Update/onKey directly,
// with no a2a client and no kubeconfig — tests here only exercise key
// handling and state transitions, never the goroutines that would touch the
// network (stream) or shell out (kubectl), since those Cmds are captured but
// never invoked. histPath is left empty, so nothing here writes a history file.
func newTestModel() *chatModel {
	st := newStyles(false)
	ta := newComposer(false)
	ta.SetWidth(80)
	ta.Focus()
	sp := spin.New(spin.WithSpinner(spin.Dot))
	return &chatModel{
		project:    "demo-project",
		ta:         ta,
		sp:         sp,
		st:         st,
		width:      120,
		termHeight: 24,
		vp:         viewport.New(viewport.WithWidth(120), viewport.WithHeight(10)),
		follow:     true,
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
	typeText(t, m, "/res")
	got := m.currentSuggestions()
	if len(got) != 1 || got[0] != "/resume" {
		t.Fatalf("'/res' should suggest only /resume, got %v", got)
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
	if m.ta.Value() != "/resume" {
		t.Fatalf("tab should complete input to /resume, got %q", m.ta.Value())
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

// ── /rename ───────────────────────────────────────────────────

// The input is one line, so /rename's argument arrives on it — bare, with an
// argument, and (not a match) as a prefix of some other word.
func TestCommandArgSplitsOnTheSameLine(t *testing.T) {
	cases := []struct {
		text    string
		wantArg string
		wantOK  bool
	}{
		{"/rename", "", true},
		{"/rename dfw quota escalation", "dfw quota escalation", true},
		{"/rename   spaced  ", "spaced", true},
		{"/renamed", "", false},
		{"rename x", "", false},
		{"tell me about /rename", "", false},
	}
	for _, c := range cases {
		arg, ok := commandArg(c.text, "/rename")
		if ok != c.wantOK || arg != c.wantArg {
			t.Errorf("commandArg(%q) = %q, %v; want %q, %v", c.text, arg, ok, c.wantArg, c.wantOK)
		}
	}
}

// The three ways /rename can't proceed are answered locally: none of them
// needs the service to know they are wrong, and a request would only come back
// with the same answer more slowly.
func TestRenameGuardsAnswerWithoutARequest(t *testing.T) {
	cases := []struct {
		name      string
		contextID string
		arg       string
		want      string
	}{
		{"no argument", "ctx-1", "", "usage: /rename <name>"},
		{"too long", "ctx-1", strings.Repeat("é", maxConversationNameLen+1), "too long"},
		{"no conversation yet", "", "a name", "nothing to rename"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestModel()
			m.contextID = c.contextID
			cmd := m.startRename(c.arg)
			if cmd != nil {
				t.Fatal("want no request kicked off")
			}
			if m.working {
				t.Fatal("want the model idle, not waiting on a rename")
			}
			if len(m.turns) != 1 || !strings.Contains(m.turns[0], c.want) {
				t.Fatalf("turns = %q, want one mentioning %q", m.turns, c.want)
			}
		})
	}
}

// A rename lands in the transcript and in /status; a failure says so and
// leaves the old name in place.
func TestRenameDoneUpdatesTheSessionName(t *testing.T) {
	m := newTestModel()
	m.working = true
	m.Update(renameDoneMsg{name: "dfw quota escalation"})
	if m.convName != "dfw quota escalation" {
		t.Fatalf("convName = %q", m.convName)
	}
	if m.working {
		t.Fatal("the rename should have cleared working")
	}
	m.Update(renameDoneMsg{name: "later", err: errors.New("boom")})
	if m.convName != "dfw quota escalation" {
		t.Fatalf("a failed rename changed the name to %q", m.convName)
	}
}

// Resuming picks the name up from the listing the picker already holds, so a
// named conversation is still named after /resume.
func TestResumingCarriesTheConversationName(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	named := titledConversation("conv-a", "api-backend not available")
	named.Status.Name = "dfw quota escalation"
	m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{named, titledConversation("conv-b", "other")}})
	m.Update(pickerTranscriptMsg{contextID: "conv-a"})
	if m.convName != "dfw quota escalation" {
		t.Fatalf("convName = %q, want the listed name", m.convName)
	}

	// And a conversation with no name resumes unnamed rather than keeping the
	// previous one's.
	m.picker = newPickerState(false)
	m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{titledConversation("conv-b", "other")}})
	m.Update(pickerTranscriptMsg{contextID: "conv-b"})
	if m.convName != "" {
		t.Fatalf("convName = %q, want empty", m.convName)
	}
}

// ── picker preview ────────────────────────────────────────────

func testConversation(name string) assistantv1alpha1.Conversation {
	c := assistantv1alpha1.Conversation{}
	c.Name = name
	return c
}

func titledConversation(name, title string) assistantv1alpha1.Conversation {
	c := testConversation(name)
	c.Status.Title = title
	c.Status.MessageCount = 4
	return c
}

// ── picker search ─────────────────────────────────────────────

func TestFilterConversationsMatchesEveryTermInTitleOrID(t *testing.T) {
	items := []assistantv1alpha1.Conversation{
		titledConversation("01a05ee5-aaaa", "Why is the api-backend workload not available?"),
		titledConversation("0b77c2d1-bbbb", "DFW quota is blocking the test instance"),
		titledConversation("0c99e3f2-cccc", ""),
	}
	cases := []struct {
		query string
		want  []int
	}{
		{"", []int{0, 1, 2}},
		{"quota", []int{1}},
		{"QUOTA dfw", []int{1}},
		{"api workload", []int{0}},
		{"0c99", []int{2}},
		{"nothing-here", []int{}},
	}
	for _, c := range cases {
		got := filterConversations(items, c.query)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("filter %q = %v, want %v", c.query, got, c.want)
		}
	}
}

// The row headline prefers what the user chose to call the conversation, then
// the derived title, and falls back to the id so a row is never blank.
func TestConversationTitlePrefersTheUserGivenName(t *testing.T) {
	named := titledConversation("01a05ee5-aaaa", "Why is the api-backend workload not available?")
	named.Status.Name = "dfw quota escalation"
	cases := []struct {
		name string
		in   assistantv1alpha1.Conversation
		want string
	}{
		{"name wins over title", named, "dfw quota escalation"},
		{"title when unnamed", titledConversation("0b77c2d1-bbbb", "quota triage"), "quota triage"},
		{"id when neither", testConversation("0c99e3f2-cccc"), "0c99e3f2-cccc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := conversationTitle(c.in); got != c.want {
				t.Errorf("conversationTitle = %q, want %q", got, c.want)
			}
		})
	}
}

// Renaming must not hide a conversation from a search for what it was
// originally about, so the name is searched alongside the title, not instead.
func TestFilterConversationsMatchesNamesToo(t *testing.T) {
	named := titledConversation("01a05ee5-aaaa", "Why is the api-backend workload not available?")
	named.Status.Name = "dfw quota escalation"
	items := []assistantv1alpha1.Conversation{named, titledConversation("0b77c2d1-bbbb", "unrelated")}
	for _, query := range []string{"escalation", "api-backend", "escalation api-backend"} {
		if got := filterConversations(items, query); fmt.Sprint(got) != "[0]" {
			t.Errorf("filter %q = %v, want [0]", query, got)
		}
	}
}

func TestPickerTypingNarrowsListAndPreviewsTopMatch(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	m.picker.transcript = true
	m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{
		titledConversation("conv-a", "api-backend not available"),
		titledConversation("conv-b", "quota triage for dfw"),
	}})
	m.Update(pickerPreviewMsg{contextID: "conv-a"}) // settle the initial fetch

	var cmd tea.Cmd
	for _, r := range "quota" {
		_, cmd = m.onPickerKey(key(r, string(r)))
	}
	if len(m.picker.filtered) != 1 || m.picker.filtered[0] != 1 {
		t.Fatalf("typing \"quota\" should leave only conv-b, got filtered=%v", m.picker.filtered)
	}
	if got, _ := m.picker.selected(); got.Name != "conv-b" {
		t.Fatalf("cursor should sit on the top match, got %q", got.Name)
	}
	if cmd == nil || !m.picker.previewPending["conv-b"] {
		t.Fatal("narrowing onto conv-b should kick off its preview fetch")
	}
	if strings.TrimSpace(m.ta.Value()) != "" {
		t.Fatalf("picker typing must not leak into the chat input, got %q", m.ta.Value())
	}

	// Enter resumes the highlighted match, not the first item of the full list.
	_, cmd = m.onPickerKey(key(tea.KeyEnter, ""))
	if cmd == nil || !m.picker.loading {
		t.Fatal("enter on a match should start loading that conversation")
	}
}

func TestPickerViewEmptyStates(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	m.Update(pickerListMsg{})
	if out := plain(m.pickerView().Content); !containsAll(out, "No conversations found in this project.") {
		t.Fatalf("empty project should say so, got:\n%s", out)
	}

	m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{titledConversation("conv-a", "hello there")}})
	typePicker(m, "zzz")
	if out := plain(m.pickerView().Content); !containsAll(out, `No conversations match "zzz".`) {
		t.Fatalf("no-match should name the query, got:\n%s", out)
	}
}

func TestPickerRowsAreOneLineUntilComfortableView(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{
		titledConversation("01a05ee5-1234-5678", "Why is the api-backend workload not available?"),
		testConversation("0b77c2d1-summary-only"),
	}})
	out := plain(m.pickerView().Content)
	if !containsAll(out, "Resume a previous conversation", "Type to search", "Sort: [Updated] Created",
		"❯ no activity  Why is the api-backend workload not available?", "0b77c2d1-summary-only",
		" 1 / 2 ", "enter resume", "ctrl+t transcript") {
		t.Fatalf("picker missing expected pieces, got:\n%s", out)
	}
	if strings.Contains(out, "4 messages") || strings.Contains(out, "01a05ee5-1234-5678") {
		t.Fatalf("compact rows should not carry the size or id, got:\n%s", out)
	}
	m.onPickerKey(key(tea.KeyTab, "")) // focus sort, then flip it
	m.onPickerKey(key(tea.KeyRight, ""))
	m.onPickerKey(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	out = plain(m.pickerView().Content)
	if !containsAll(out, "Sort: Updated [Created]", "4 messages · 01a05ee5-1234-5678") {
		t.Fatalf("comfortable view + Created sort missing, got:\n%s", out)
	}
	if !containsAll(out, "\n") || m.picker.search.Value() != "" {
		t.Fatalf("tab/arrows/ctrl+o must not type into the search box, got %q", m.picker.search.Value())
	}
}

func TestPickerTranscriptToggleShowsPreview(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{titledConversation("conv-a", "hello")}})
	if m.picker.previewPending["conv-a"] {
		t.Fatal("no preview fetch until the transcript pane is opened")
	}
	_, cmd := m.onPickerKey(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if cmd == nil || !m.picker.previewPending["conv-a"] {
		t.Fatal("ctrl+t should open the pane and fetch the highlighted transcript")
	}
	m.Update(pickerPreviewMsg{contextID: "conv-a", items: []assistantv1alpha1.ConversationMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi back"}}})
	if out := plain(m.pickerView().Content); !containsAll(out, "preview", "Patch: hi back") {
		t.Fatalf("transcript pane should render under the rows, got:\n%s", out)
	}
}

func TestPickerEscCancels(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	m.Update(pickerListMsg{items: []assistantv1alpha1.Conversation{titledConversation("conv-a", "x")}})
	m.onKey(key(tea.KeyEscape, ""))
	if m.picker.open {
		t.Fatal("esc should close the picker")
	}
}

// plain strips ANSI styling so assertions can match text the renderer split
// across styled spans (e.g. the placeholder's cursor cell).
func plain(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

func typePicker(m *chatModel, s string) {
	for _, r := range s {
		m.onPickerKey(key(r, string(r)))
	}
}

// ── resume on start ───────────────────────────────────────────

func TestResumeOnStartOpensPickerAtFirstLayout(t *testing.T) {
	m := newTestModel()
	m.resumeOnStart = pickerOnStart
	cmd := m.layout(120, 40)
	if cmd == nil || !m.picker.open || !m.picker.loading {
		t.Fatalf("first layout should open a loading picker and fetch the list (cmd=%v open=%v loading=%v)", cmd != nil, m.picker.open, m.picker.loading)
	}
	if m.resumeOnStart != "" {
		t.Fatal("resumeOnStart must be consumed so a later resize doesn't reopen the picker")
	}
	if m.layout(100, 40) != nil {
		t.Fatal("a second layout must not re-trigger the resume")
	}
}

func TestResumeOnStartWithIDLoadsTranscriptDirectly(t *testing.T) {
	m := newTestModel()
	m.resumeOnStart = "ctx-42"
	if cmd := m.layout(120, 40); cmd == nil || !m.picker.direct || !m.picker.loading {
		t.Fatal("first layout should start a direct transcript load")
	}
	if out := plain(m.pickerView().Content); !containsAll(out, "loading conversation") || strings.Contains(out, "Type to search") {
		t.Fatalf("direct load shows a spinner and no search box, got:\n%s", out)
	}
	// Typing while a direct load is pending goes nowhere — there is no list to filter.
	m.onPickerKey(key('q', "q"))
	if m.picker.search.Value() != "" {
		t.Fatal("direct mode has no search box to type into")
	}

	m.Update(pickerTranscriptMsg{contextID: "ctx-42", items: []assistantv1alpha1.ConversationMessage{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}})
	if m.picker.open || m.contextID != "ctx-42" || len(m.raw) != 2 {
		t.Fatalf("transcript should land in the chat and close the overlay (open=%v ctx=%q turns=%d)", m.picker.open, m.contextID, len(m.raw))
	}
}

func TestResumeDirectErrorOffersFreshChat(t *testing.T) {
	m := newTestModel()
	m.resumeOnStart = "ctx-missing"
	m.layout(120, 40)
	m.Update(pickerTranscriptMsg{err: errors.New("kubectl: not found")})
	out := plain(m.pickerView().Content)
	if !containsAll(out, "kubectl: not found", "esc to start a fresh conversation") {
		t.Fatalf("direct-load failure should show the error and the way out, got:\n%s", out)
	}
	m.onKey(key(tea.KeyEscape, ""))
	if m.picker.open {
		t.Fatal("esc should drop into a fresh chat")
	}
}

func TestPickerListLoadTriggersInitialPreviewFetch(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	m.picker.transcript = true
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
	m.picker = newPickerState(false)
	m.picker.transcript = true
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a"), testConversation("conv-b")}
	m.picker.refilter()
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
	m.picker = newPickerState(false)
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.refilter()
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
	m.picker = newPickerState(false)
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.refilter()
	m.picker.preview["conv-a"] = []assistantv1alpha1.ConversationMessage{
		{Role: "user", Content: "diagnose pipeline p-7"},
		{Role: "assistant", Content: "found the issue"},
	}
	out := m.previewPane(60)
	if !containsAll(out, "diagnose pipeline p-7", "found the issue") {
		t.Fatalf("preview pane should show the cached transcript, got:\n%s", out)
	}
}

func TestPreviewPaneRendersSummaryDistinctly(t *testing.T) {
	m := newTestModel()
	m.picker = newPickerState(false)
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.refilter()
	m.picker.preview["conv-a"] = []assistantv1alpha1.ConversationMessage{
		{Role: "summary", Content: "compacted digest of earlier turns"},
		{Role: "user", Content: "what's next"},
	}
	out := m.previewPane(60)
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
	m.picker = newPickerState(false)
	m.picker.items = []assistantv1alpha1.Conversation{testConversation("conv-a")}
	m.picker.refilter()
	m.picker.previewPending["conv-a"] = true
	if out := m.previewPane(60); !containsAll(out, "loading") {
		t.Fatalf("pending preview should show a loading state, got:\n%s", out)
	}
}

// TestCompactWithNoConversationIsNoop pins that /compact before any turn has
// happened (no contextID yet — there is nothing on the server to compact)
// reports that inline without touching m.working or spawning the network
// goroutine (m.prog is nil in this test model, so that goroutine would panic
// on send — this path must return before it's launched).
func TestCompactWithNoConversationIsNoop(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/compact")
	m.onKey(key(tea.KeyEnter, ""))
	if m.working {
		t.Fatal("/compact with no conversation should not enter the working state")
	}
	if len(m.turns) != 1 || !strings.Contains(m.turns[0], "nothing to compact") {
		t.Fatalf("turns = %v, want a single 'nothing to compact' line", m.turns)
	}
}

// TestCompactDoneMsg_Success/_NothingToCompact/_Error drive
// chatModel.Update(compactDoneMsg{...}) directly — the same way the picker
// tests drive pickerListMsg — rather than going through the real /compact
// goroutine, so these stay pure state-transition tests.
func TestCompactDoneMsg_Success(t *testing.T) {
	m := newTestModel()
	m.working = true
	m.Update(compactDoneMsg{err: nil})
	if m.working {
		t.Fatal("a completed /compact should clear the working state")
	}
	if len(m.turns) != 1 || !strings.Contains(m.turns[0], "history compacted") {
		t.Fatalf("turns = %v, want a 'history compacted' line", m.turns)
	}
	if len(m.raw) != 1 || m.raw[0].role != "system" {
		t.Fatalf("raw = %+v, want one system-role entry", m.raw)
	}
}

func TestCompactDoneMsg_NothingToCompact(t *testing.T) {
	m := newTestModel()
	m.working = true
	m.Update(compactDoneMsg{err: ErrNothingToCompact})
	if m.working {
		t.Fatal("compactDoneMsg should clear the working state even on ErrNothingToCompact")
	}
	if len(m.turns) != 1 || !strings.Contains(m.turns[0], "nothing to compact") {
		t.Fatalf("turns = %v, want a 'nothing to compact' line", m.turns)
	}
}

func TestCompactDoneMsg_Error(t *testing.T) {
	m := newTestModel()
	m.working = true
	m.Update(compactDoneMsg{err: errors.New("boom")})
	if m.working {
		t.Fatal("compactDoneMsg should clear the working state even on a real error")
	}
	if len(m.turns) != 1 || !strings.Contains(m.turns[0], "boom") {
		t.Fatalf("turns = %v, want the error text surfaced", m.turns)
	}
}

// ── scrollback ──────────────────────────────

// fillTranscript gives the model more finalized turns than the 10-row test
// viewport can show, so there is something to scroll back through.
func fillTranscript(m *chatModel, n int) {
	for i := 0; i < n; i++ {
		m.turns = append(m.turns, m.turnBlock("You", fmt.Sprintf("message %d", i)))
		m.raw = append(m.raw, transcriptTurn{role: "user", content: fmt.Sprintf("message %d", i)})
	}
	m.rebuildViewport()
}

func TestScrollUpStopsFollowing(t *testing.T) {
	m := newTestModel()
	fillTranscript(m, 40)
	if !m.vp.AtBottom() {
		t.Fatal("a fresh transcript should start pinned to the bottom")
	}
	m.onKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if m.follow {
		t.Fatal("pgup should stop following the newest output")
	}
	if m.vp.AtBottom() {
		t.Fatal("pgup should have moved the viewport off the bottom")
	}
}

// The regression this whole feature exists for: history the user scrolled back
// to must stay put while an answer streams in below it.
func TestStreamChunkKeepsPositionWhileScrolledUp(t *testing.T) {
	m := newTestModel()
	m.working = true
	fillTranscript(m, 40)
	m.onKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	before := m.vp.YOffset()

	for _, chunk := range []string{"one", " two", " three"} {
		m.Update(streamChunkMsg{text: chunk})
	}

	if m.follow {
		t.Fatal("streaming should not re-arm follow on its own")
	}
	if got := m.vp.YOffset(); got != before {
		t.Fatalf("streamed chunks moved the view: offset %d, want %d", got, before)
	}
}

func TestStreamChunkFollowsWhenAtBottom(t *testing.T) {
	m := newTestModel()
	m.working = true
	fillTranscript(m, 40)
	m.Update(streamChunkMsg{text: strings.Repeat("more output\n", 20)})
	if !m.vp.AtBottom() {
		t.Fatal("streaming while following should keep the newest output in view")
	}
}

func TestEscReturnsToTheLatest(t *testing.T) {
	m := newTestModel()
	fillTranscript(m, 40)
	m.onKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	m.onKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.follow || !m.vp.AtBottom() {
		t.Fatalf("esc should jump to the bottom and resume following (follow=%v atBottom=%v)",
			m.follow, m.vp.AtBottom())
	}
}

// Scrolling back down to the end re-arms follow without needing esc.
func TestScrollingBackToBottomResumesFollow(t *testing.T) {
	m := newTestModel()
	fillTranscript(m, 40)
	m.onKey(tea.KeyPressMsg{Code: tea.KeyPgUp})
	for i := 0; i < 10 && !m.vp.AtBottom(); i++ {
		m.onKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	}
	if !m.follow {
		t.Fatal("reaching the bottom again should resume following")
	}
}

// Bare arrows scroll only when no slash-command suggestion list is showing,
// which owns ↑/↓ for cycling matches.
func TestArrowsScrollOnlyWithoutSuggestions(t *testing.T) {
	m := newTestModel()
	fillTranscript(m, 40)
	m.onKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.follow {
		t.Fatal("bare up-arrow should scroll the transcript")
	}

	m.onKey(tea.KeyPressMsg{Code: tea.KeyEscape}) // back to the bottom
	typeText(t, m, "/re")
	before := m.vp.YOffset()
	m.onKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.vp.YOffset(); got != before {
		t.Fatalf("up-arrow scrolled while suggestions were showing: offset %d, want %d", got, before)
	}
}

func TestMouseWheelScrolls(t *testing.T) {
	m := newTestModel()
	fillTranscript(m, 40)
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.follow || m.vp.AtBottom() {
		t.Fatal("the wheel should scroll the transcript and stop following")
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

// ── composer ──────────────────────────────────────────────────

// modKey builds a modified keypress (shift+enter, ctrl+j, …); those carry no
// .Text, so onKey matches them by String() alone.
func modKey(code rune, mod tea.KeyMod) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: mod}
}

var (
	shiftEnter = modKey(tea.KeyEnter, tea.ModShift)
	altEnter   = modKey(tea.KeyEnter, tea.ModAlt)
	ctrlJ      = modKey('j', tea.ModCtrl)
	ctrlR      = modKey('r', tea.ModCtrl)
	enterKey   = key(tea.KeyEnter, "")
	upKey      = key(tea.KeyUp, "")
	downKey    = key(tea.KeyDown, "")
)

// newSubmitTestModel is newTestModel wired up enough to survive a real submit:
// that path spawns the streaming goroutine, which needs a client to call and a
// program to send events to. Both contexts are already cancelled, so the turn
// fails at once and its messages are dropped — these tests are about what
// submit does to the composer, not about the turn.
func newSubmitTestModel(t *testing.T) *chatModel {
	t.Helper()
	base := newTestServiceWith(t, &echoExecutor{})
	ctx, cancel := context.WithCancel(context.Background())
	client, err := newClient(ctx, base, StaticToken("good"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	m := newTestModel()
	m.ctx = ctx
	m.client = client
	m.prog = tea.NewProgram(m, tea.WithContext(ctx))
	m.layout(120, 40)
	return m
}

func TestNewlineKeysInsertWithoutSending(t *testing.T) {
	for name, k := range map[string]tea.KeyPressMsg{"shift+enter": shiftEnter, "ctrl+j": ctrlJ, "alt+enter": altEnter} {
		m := newTestModel()
		typeText(t, m, "one")
		m.onKey(k)
		typeText(t, m, "two")
		if got := m.ta.Value(); got != "one\ntwo" {
			t.Errorf("%s should break the line, got %q", name, got)
		}
		if len(m.raw) != 0 {
			t.Errorf("%s must not send the message", name)
		}
	}
}

func TestTrailingBackslashBeforeEnterInsertsNewline(t *testing.T) {
	m := newTestModel()
	typeText(t, m, `one\`)
	m.onKey(enterKey)
	typeText(t, m, "two")
	if got := m.ta.Value(); got != "one\ntwo" {
		t.Fatalf("a trailing backslash should be consumed and break the line, got %q", got)
	}
	// A backslash anywhere but just before the cursor is ordinary text.
	m2 := newSubmitTestModel(t)
	typeText(t, m2, `a\b`)
	m2.onKey(enterKey)
	if len(m2.raw) != 1 || m2.raw[0].content != `a\b` {
		t.Fatalf("enter should have sent the line verbatim, got %+v", m2.raw)
	}
}

func TestEnterSendsAndClearsTheComposer(t *testing.T) {
	m := newSubmitTestModel(t)
	typeText(t, m, "one")
	m.onKey(shiftEnter)
	typeText(t, m, "two")
	m.onKey(enterKey)

	if len(m.raw) != 1 || m.raw[0].role != "user" || m.raw[0].content != "one\ntwo" {
		t.Fatalf("enter should send the whole multi-line draft, got %+v", m.raw)
	}
	if m.ta.Value() != "" || m.ta.LineCount() != 1 {
		t.Fatalf("the composer should be empty again, got %q over %d lines", m.ta.Value(), m.ta.LineCount())
	}
	if len(m.history) != 1 || m.history[0] != "one\ntwo" {
		t.Fatalf("the sent prompt should be in history, got %v", m.history)
	}
}

func TestComposerGrowsAndTakesRowsFromTheTranscript(t *testing.T) {
	m := newTestModel()
	m.layout(120, 40)
	base := m.viewportHeight()
	if m.composerRows() != 1 {
		t.Fatalf("an empty composer is one row, got %d", m.composerRows())
	}
	m.onKey(ctrlJ)
	if m.composerRows() != 2 || m.viewportHeight() != base-1 {
		t.Fatalf("a second composer row should cost the viewport one: rows=%d height=%d (was %d)",
			m.composerRows(), m.viewportHeight(), base)
	}
	for range maxComposerRows + 5 {
		m.onKey(ctrlJ)
	}
	if m.composerRows() != maxComposerRows {
		t.Fatalf("the composer should stop growing at %d rows, got %d", maxComposerRows, m.composerRows())
	}
	if m.viewportHeight() != base-(maxComposerRows-1) {
		t.Fatalf("viewport height should track the cap, got %d", m.viewportHeight())
	}
}

// ── prompt history ────────────────────────────────────────────

func TestHistoryRecallWalksNewestFirstAndBack(t *testing.T) {
	m := newTestModel()
	m.history = []string{"first", "second"}
	m.histIdx = len(m.history)

	m.onKey(upKey)
	if m.ta.Value() != "second" {
		t.Fatalf("up should recall the newest prompt, got %q", m.ta.Value())
	}
	m.onKey(upKey)
	if m.ta.Value() != "first" {
		t.Fatalf("a second up should step further back, got %q", m.ta.Value())
	}
	m.onKey(downKey)
	if m.ta.Value() != "second" {
		t.Fatalf("down should step forward again, got %q", m.ta.Value())
	}
	m.onKey(downKey)
	if m.ta.Value() != "" {
		t.Fatalf("stepping past the newest entry should restore the draft, got %q", m.ta.Value())
	}
}

func TestHistoryRecallKeepsTheDraftItInterrupted(t *testing.T) {
	m := newTestModel()
	m.history = []string{"older"}
	m.histIdx = 1
	m.setComposer("half typed") // as if recalled, so up/down still walk history
	m.onKey(upKey)
	if m.ta.Value() != "older" {
		t.Fatalf("up should recall, got %q", m.ta.Value())
	}
	m.onKey(downKey)
	if m.ta.Value() != "half typed" {
		t.Fatalf("down should put the interrupted draft back, got %q", m.ta.Value())
	}
}

func TestUpDownScrollWhenThereIsNoHistoryLeftToRecall(t *testing.T) {
	m := newTestModel()
	fillTranscript(m, 40)
	m.onKey(upKey)
	if m.follow {
		t.Fatal("with no history, up should scroll the transcript")
	}

	m2 := newTestModel()
	fillTranscript(m2, 40)
	m2.history = []string{"only one"}
	m2.histIdx = 1
	m2.onKey(upKey) // consumes the one entry
	if !m2.follow {
		t.Fatal("a recall must not scroll the transcript")
	}
	m2.onKey(upKey) // nothing older left
	if m2.follow {
		t.Fatal("once history is exhausted, up should scroll again")
	}
	if m2.ta.Value() != "only one" {
		t.Fatalf("scrolling must not disturb the recalled prompt, got %q", m2.ta.Value())
	}
}

func TestUpDownMoveTheCursorOnceTheComposerIsMultiLineOrEdited(t *testing.T) {
	m := newTestModel()
	m.history = []string{"recallable"}
	m.histIdx = 1
	typeText(t, m, "a")
	m.onKey(ctrlJ)
	typeText(t, m, "b")
	m.onKey(upKey)
	if m.ta.Value() != "a\nb" {
		t.Fatalf("up on a multi-line composer must not recall, got %q", m.ta.Value())
	}
	if m.ta.Line() != 0 {
		t.Fatalf("up should have moved the cursor to the first line, got row %d", m.ta.Line())
	}

	m2 := newTestModel()
	m2.history = []string{"recallable"}
	m2.histIdx = 1
	typeText(t, m2, "typed")
	m2.onKey(upKey)
	if m2.ta.Value() != "typed" {
		t.Fatalf("up on an edited composer must not recall over it, got %q", m2.ta.Value())
	}
}

func TestRecordHistoryDedupesAndPersists(t *testing.T) {
	m := newTestModel()
	m.histPath = filepath.Join(t.TempDir(), "history-demo.txt")
	m.recordHistory("hello")
	m.recordHistory("hello")
	m.recordHistory("")
	if len(m.history) != 1 {
		t.Fatalf("a repeat of the last prompt should not be recorded twice, got %v", m.history)
	}
	if got := loadHistory(m.histPath); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("history should have been written, got %v", got)
	}
}

func TestHistoryFileRoundTripsAwkwardPrompts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history-demo.txt")
	want := []string{"one\ntwo", `a \ b`, `ends with \`, "plain"}
	saveHistory(path, want)
	got := loadHistory(path)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history round trip = %q, want %q", got, want)
	}
}

func TestHistoryIsPerProjectAndFailsQuietly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	path := historyPath("demo/project")
	if path == "" {
		t.Skip("no user config dir on this platform")
	}
	if !strings.HasSuffix(path, filepath.Join("patch", "history-demo_project.txt")) {
		t.Fatalf("history path should be per project and filename-safe, got %q", path)
	}
	// Nothing written yet, and an unwritable path: both are silent no-ops.
	if got := loadHistory(path); got != nil {
		t.Fatalf("a missing history file should read as empty, got %v", got)
	}
	saveHistory("", []string{"dropped"})
	if got := loadHistory(""); got != nil {
		t.Fatalf("no path means no history, got %v", got)
	}
}

// ── reverse search ────────────────────────────────────────────

func TestReverseSearchFindsCyclesAndAccepts(t *testing.T) {
	m := newTestModel()
	m.history = []string{"deploy the api", "check quota for dfw", "deploy the worker"}
	m.histIdx = len(m.history)

	m.onKey(ctrlR)
	if !m.rsearch {
		t.Fatal("ctrl+r should open the search")
	}
	typeText(t, m, "DEPLOY") // case-insensitive
	if got := m.searchHit(); got != "deploy the worker" {
		t.Fatalf("the newest match should win, got %q", got)
	}
	if out := plain(m.composerBar()); !containsAll(out, "reverse-i-search", "DEPLOY", "deploy the worker") {
		t.Fatalf("the search line should show the query and the match, got %q", out)
	}
	m.onKey(ctrlR)
	if got := m.searchHit(); got != "deploy the api" {
		t.Fatalf("another ctrl+r should step to the next older match, got %q", got)
	}
	m.onKey(ctrlR) // no third match; stay put
	if got := m.searchHit(); got != "deploy the api" {
		t.Fatalf("stepping past the last match should stay put, got %q", got)
	}
	m.onKey(enterKey)
	if m.rsearch || m.ta.Value() != "deploy the api" {
		t.Fatalf("enter should accept the match into the composer (open=%v value=%q)", m.rsearch, m.ta.Value())
	}
}

func TestReverseSearchEscLeavesTheComposerAlone(t *testing.T) {
	m := newTestModel()
	m.history = []string{"deploy the api"}
	m.histIdx = 1
	typeText(t, m, "draft")
	m.onKey(ctrlR)
	typeText(t, m, "deploy")
	m.onKey(key(tea.KeyEscape, ""))
	if m.rsearch || m.ta.Value() != "draft" {
		t.Fatalf("esc should cancel without touching the composer (open=%v value=%q)", m.rsearch, m.ta.Value())
	}
}

func TestReverseSearchWithNoMatchSaysSo(t *testing.T) {
	m := newTestModel()
	m.history = []string{"deploy the api"}
	m.histIdx = 1
	m.onKey(ctrlR)
	typeText(t, m, "zzz")
	if out := plain(m.composerBar()); !strings.Contains(out, "(no match)") {
		t.Fatalf("an unmatched query should say so, got %q", out)
	}
	m.onKey(enterKey)
	if m.ta.Value() != "" {
		t.Fatalf("accepting nothing should leave the composer empty, got %q", m.ta.Value())
	}
}

// ── paste chips ───────────────────────────────────────────────

func TestLargePasteCollapsesToChipsAndExpandsOnSubmit(t *testing.T) {
	m := newSubmitTestModel(t)
	first := strings.Repeat("alpha\n", 41) + "alpha" // 42 lines
	second := "a\nb\nc\nd\ne"                        // 5 lines

	m.Update(tea.PasteMsg{Content: first})
	if got := m.ta.Value(); got != "[Pasted text #1 +42 lines]" {
		t.Fatalf("a 42-line paste should collapse to a chip, got %q", got)
	}
	typeText(t, m, " and ")
	m.Update(tea.PasteMsg{Content: second})
	if got := m.ta.Value(); got != "[Pasted text #1 +42 lines] and [Pasted text #2 +5 lines]" {
		t.Fatalf("a second paste should get its own chip, got %q", got)
	}
	if out := plain(m.composerBar()); !containsAll(out, "[Pasted text #1 +42 lines]", "[Pasted text #2 +5 lines]", "expands on send") {
		t.Fatalf("the chip legend should name both pastes, got %q", out)
	}

	m.onKey(enterKey)
	want := first + " and " + second
	if len(m.raw) != 1 || m.raw[0].content != want {
		t.Fatalf("chips should expand back into the sent message, got %q", m.raw[0].content)
	}
	if len(m.pastes) != 0 {
		t.Fatalf("sending should retire the chips, got %v", m.pastes)
	}
}

func TestSmallPasteGoesInVerbatim(t *testing.T) {
	m := newTestModel()
	m.Update(tea.PasteMsg{Content: "one\ntwo"})
	if m.ta.Value() != "one\ntwo" || len(m.pastes) != 0 {
		t.Fatalf("a small paste should land as text, got %q (%d chips)", m.ta.Value(), len(m.pastes))
	}
	m2 := newTestModel()
	m2.Update(tea.PasteMsg{Content: strings.Repeat("x", pasteChipChars+1)})
	if len(m2.pastes) != 1 || m2.ta.Value() != "[Pasted text #1 +1 line]" {
		t.Fatalf("a long single line should still chip, got %q", m2.ta.Value())
	}
}

func TestExpandPasteChipsLeavesUnknownChipsAlone(t *testing.T) {
	got := expandPasteChips("[Pasted text #1 +2 lines] [Pasted text #9 +2 lines]", []string{"real"})
	if got != "real [Pasted text #9 +2 lines]" {
		t.Fatalf("an index with no paste behind it should stay literal, got %q", got)
	}
}

// ── autocomplete alongside the composer ───────────────────────

func TestSuggestionsOnlyEngageOnASingleSlashLine(t *testing.T) {
	m := newTestModel()
	typeText(t, m, "/res")
	if len(m.currentSuggestions()) != 1 {
		t.Fatal("setup: '/res' should suggest /resume alone")
	}
	m.onKey(ctrlJ) // now two lines
	if got := m.currentSuggestions(); got != nil {
		t.Fatalf("a multi-line draft is a message, not a command, got %v", got)
	}
	if out := plain(m.composerBar()); strings.Contains(out, "/resume") {
		t.Fatalf("the suggestion bar should be gone too, got %q", out)
	}
}

func TestHelpOverlayDocumentsTheComposerKeys(t *testing.T) {
	m := newTestModel()
	out := plain(m.helpView().Content)
	if !containsAll(out, "shift+enter", "ctrl+j", "alt+enter", "ctrl+r", "recall past prompts",
		"scroll the transcript when there is no history left to recall") {
		t.Fatalf("/help should document the composer, got:\n%s", out)
	}
}
