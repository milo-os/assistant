package patchcli

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// mentionTestModel is newTestModel with discovery already answered, so the
// tests drive the picker's filtering and insertion rather than its fetching
// (whose Cmds are captured but never run — see newTestModel).
func mentionTestModel() *chatModel {
	m := newTestModel()
	m.mentions.kindsLoaded = true
	m.mentions.kinds = []resourceKind{
		{token: "httpproxy", plural: "httpproxies", kind: "HTTPProxy", group: "networking.datumapis.com", version: "v1alpha1"},
		{token: "instance", plural: "instances", kind: "Instance", group: "compute.datumapis.com", version: "v1alpha1"},
		{token: "workload", plural: "workloads", kind: "Workload", group: "compute.datumapis.com", version: "v1alpha1"},
	}
	m.mentions.names = map[string][]string{
		"workload": {"web-frontend", "api-backend", "batch-runner"},
	}
	return m
}

// The two section headers, as rowLabels renders them.
const (
	referencedHeader = "── referenced in this conversation"
	allKindsHeader   = "── all resource kinds"
)

func rowLabels(rows []mentionRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.rule != "" {
			out = append(out, "── "+r.rule)
			continue
		}
		out = append(out, r.label)
	}
	return out
}

// ── when the list opens ─────────────────────────────────────────

func TestMentionQueryOnlyAtAWordBoundary(t *testing.T) {
	tests := []struct {
		typed string
		want  string
		open  bool
	}{
		{"@", "", true},
		{"look at @work", "work", true},
		{"@workload/api", "workload/api", true},
		{"(@work", "work", true},
		{"mail wells@gmail", "", false},
		{"plain text", "", false},
	}
	for _, tc := range tests {
		m := mentionTestModel()
		typeText(t, m, tc.typed)
		got, ok := m.mentionQuery()
		if ok != tc.open || got != tc.want {
			t.Errorf("after %q: query = %q,%v want %q,%v", tc.typed, got, ok, tc.want, tc.open)
		}
	}
}

// A "/" being typed is a command, not a mention, and vice versa: the two lists
// share one bar and must never both claim it.
func TestMentionListDoesNotDisplaceSlashSuggestions(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "/re")
	if rows := m.mentionRows(); len(rows) > 0 {
		t.Fatalf("a slash command must not open the mention list: %v", rowLabels(rows))
	}
	if got := m.currentSuggestions(); len(got) != 2 {
		t.Fatalf("slash suggestions stopped working: %v", got)
	}

	m2 := mentionTestModel()
	typeText(t, m2, "@work")
	if len(m2.currentSuggestions()) != 0 {
		t.Fatal("a mention must not be read as a slash command")
	}
	if len(m2.mentionRows()) == 0 {
		t.Fatal("the mention list should be open")
	}
}

// ── filtering ───────────────────────────────────────────────────

func TestMentionKindRowsFilterAndFuzzyMatch(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@")
	if got := rowLabels(m.mentionRows()); len(got) != 3 {
		t.Fatalf("a bare @ should offer every kind, got %v", got)
	}

	m = mentionTestModel()
	typeText(t, m, "@work")
	if got := rowLabels(m.mentionRows()); len(got) != 1 || got[0] != "@workload/" {
		t.Fatalf("prefix filter = %v, want only @workload/", got)
	}

	// Subsequence, the way a file picker matches: "hpx" finds "httpproxy".
	m = mentionTestModel()
	typeText(t, m, "@hpx")
	if got := rowLabels(m.mentionRows()); len(got) != 1 || got[0] != "@httpproxy/" {
		t.Fatalf("subsequence filter = %v, want only @httpproxy/", got)
	}

	m = mentionTestModel()
	typeText(t, m, "@zzz")
	rows := m.mentionRows()
	if len(rows) != 1 || rows[0].insert != "" {
		t.Fatalf("a query matching nothing should show one un-acceptable row, got %v", rows)
	}
}

func TestMentionInstanceRowsFilter(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@workload/")
	if got := rowLabels(m.mentionRows()); len(got) != 3 || got[0] != "@workload/web-frontend" {
		t.Fatalf("instances = %v, want all three in listing order", got)
	}

	m = mentionTestModel()
	typeText(t, m, "@workload/api")
	if got := rowLabels(m.mentionRows()); len(got) != 1 || got[0] != "@workload/api-backend" {
		t.Fatalf("filtered instances = %v", got)
	}
}

// A kind whose instances have not arrived says so on one line rather than
// showing an empty list that reads as "there are none".
func TestMentionInstanceRowsWhileLoading(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@instance/")
	rows := m.mentionRows()
	if len(rows) != 1 || !strings.Contains(rows[0].label, "loading") || rows[0].insert != "" {
		t.Fatalf("rows = %v, want a single loading line", rows)
	}
}

// A failed listing is one line in the picker, never an error in the chat.
func TestMentionListingFailureShowsOneLine(t *testing.T) {
	m := mentionTestModel()
	m.mentions.namesErr = map[string]string{"instance": `instances is forbidden: User "u" cannot list`}
	typeText(t, m, "@instance/")
	rows := m.mentionRows()
	if len(rows) != 1 || rows[0].insert != "" || !strings.Contains(rows[0].desc, "cannot list") {
		t.Fatalf("rows = %+v, want one error line carrying the apiserver's message", rows)
	}
	if len(m.turns) != 0 {
		t.Fatal("a discovery failure must not write to the transcript")
	}
}

func TestMentionDiscoveryFailureShowsOneLine(t *testing.T) {
	m := newTestModel()
	m.mentions.kindsErr = "kubectl not found on PATH"
	typeText(t, m, "@")
	rows := m.mentionRows()
	if len(rows) != 1 || rows[0].desc != "kubectl not found on PATH" {
		t.Fatalf("rows = %+v", rows)
	}
}

// ── keys ────────────────────────────────────────────────────────

func TestMentionTabCompletesKindThenInstance(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@work")
	m.onKey(key(tea.KeyTab, ""))
	if got := m.ta.Value(); got != "@workload/" {
		t.Fatalf("after tab on a kind, composer = %q", got)
	}
	typeText(t, m, "api")
	m.onKey(key(tea.KeyTab, ""))
	if got := m.ta.Value(); got != "@workload/api-backend " {
		t.Fatalf("after tab on an instance, composer = %q", got)
	}
	if len(m.mentionRows()) != 0 {
		t.Fatal("the trailing space should close the list")
	}
}

// Enter accepts the highlighted row instead of sending, the same way it
// resolves a slash-command suggestion.
func TestMentionEnterAcceptsWithoutSending(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@workload/api")
	m.onKey(key(tea.KeyEnter, ""))
	if got := m.ta.Value(); got != "@workload/api-backend " {
		t.Fatalf("composer = %q", got)
	}
	if len(m.turns) != 0 {
		t.Fatal("accepting a mention must not submit the message")
	}
}

// Completing mid-sentence must not disturb what follows the cursor.
func TestMentionInsertInTheMiddleOfALine(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "why is @work down?")
	// Put the cursor back at the end of "@work".
	for range len(" down?") {
		m.onKey(tea.KeyPressMsg{Code: tea.KeyLeft})
	}
	m.onKey(key(tea.KeyTab, ""))
	if got := m.ta.Value(); got != "why is @workload/ down?" {
		t.Fatalf("composer = %q", got)
	}
}

func TestMentionUpDownMoveTheHighlight(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@workload/")
	m.onKey(key(tea.KeyDown, ""))
	if m.mentions.index != 1 {
		t.Fatalf("down should advance the highlight, got %d", m.mentions.index)
	}
	m.onKey(key(tea.KeyUp, ""))
	m.onKey(key(tea.KeyUp, ""))
	if m.mentions.index != 2 {
		t.Fatalf("up should wrap to the last row, got %d", m.mentions.index)
	}
	m.onKey(key(tea.KeyEnter, ""))
	if got := m.ta.Value(); got != "@workload/batch-runner " {
		t.Fatalf("enter should accept the highlighted row, got %q", got)
	}
}

// esc closes the list without touching the text, and typing on reopens it.
func TestMentionEscClosesTheList(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@work")
	m.onKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if len(m.mentionRows()) != 0 {
		t.Fatal("esc should close the list")
	}
	if m.ta.Value() != "@work" {
		t.Fatalf("esc must not edit the composer, got %q", m.ta.Value())
	}
	typeText(t, m, "l")
	if len(m.mentionRows()) == 0 {
		t.Fatal("typing on should reopen the list")
	}
}

// The list shares its bar with the composer's other rows, so its height has to
// be what the layout budgets for it.
func TestMentionRowsClaimTheBarHeight(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@")
	if got := m.suggestionRows(); got != 3+mentionGapRows {
		t.Fatalf("suggestionRows = %d, want one per kind plus the gap", got)
	}
	rows := m.mentionRows()
	if n := strings.Count(m.mentionBar(rows), "\n") + 1; n != 3+mentionGapRows {
		t.Fatalf("mentionBar rendered %d lines, want %d", n, 3+mentionGapRows)
	}
	// Capped, and still exactly as tall as it claims.
	m.mentions.kinds = make([]resourceKind, 0, 12)
	for _, tok := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		m.mentions.kinds = append(m.mentions.kinds, resourceKind{token: tok, plural: tok + "s", kind: tok, group: "g"})
	}
	if got := m.suggestionRows(); got != maxMentionRows+mentionGapRows {
		t.Fatalf("suggestionRows = %d, want the cap of %d plus the gap", got, maxMentionRows)
	}
	if n := strings.Count(m.mentionBar(m.mentionRows()), "\n") + 1; n != maxMentionRows+mentionGapRows {
		t.Fatalf("mentionBar rendered %d lines, want %d", n, maxMentionRows+mentionGapRows)
	}
}

// What is sent carries the mention with the API group the session discovered.
func TestMentionsInResolvesGroups(t *testing.T) {
	m := mentionTestModel()
	got := m.mentionsIn("why is @workload/api-backend down?")
	if len(got) != 1 || got[0].APIGroup != "compute.datumapis.com" {
		t.Fatalf("mentionsIn = %+v", got)
	}
}

// ── what the conversation has already referenced ────────────────

// referencing puts the conversation's mentions into the model the way a
// finished turn does, oldest turn first.
func referencing(m *chatModel, tokens ...string) *chatModel {
	for _, tok := range tokens {
		m.noteMentions(m.mentionsIn("look at @" + tok))
	}
	return m
}

// topShortcut is the first resource row under the "referenced" header — what
// the highlight lands on when the list opens.
func topShortcut(m *chatModel) string {
	rows := m.mentionRows()
	return rowLabels(rows)[selectedIndex(rows, 0)]
}

func TestRecentMentionsAreOfferedBeforeTheKinds(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend")
	typeText(t, m, "@")
	got := rowLabels(m.mentionRows())
	want := []string{
		referencedHeader, "@workload/api-backend",
		allKindsHeader, "@httpproxy/", "@instance/", "@workload/",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want each section named around its rows: %v", got, want)
	}
	// The shortcut row is the whole mention, not a kind to narrow further.
	if insert := m.mentionRows()[1].insert; insert != "workload/api-backend " {
		t.Fatalf("insert = %q, want the complete mention", insert)
	}
}

// A resource is remembered once, at the front — re-referencing it must not
// spend a second row on it.
func TestRecentMentionsDedupeAndMoveToTheFront(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend", "httpproxy/edge", "workload/api-backend")
	got := make([]string, 0, len(m.mentions.recent))
	for _, mn := range m.mentions.recent {
		got = append(got, mn.Kind+"/"+mn.Name)
	}
	if want := []string{"workload/api-backend", "httpproxy/edge"}; !slices.Equal(got, want) {
		t.Fatalf("recent = %v, want %v", got, want)
	}
}

// The point of the feature: the draft re-sorts the shortcut rows, so the
// resource the sentence is about is the one that is highlighted.
func TestRecentMentionsSortByRelevanceToTheDraft(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/batch-runner", "workload/web-frontend", "httpproxy/edge")

	typeText(t, m, "@")
	if got := topShortcut(m); got != "@httpproxy/edge" {
		t.Fatalf("with nothing drafted the newest reference should lead, got %q", got)
	}

	m = referencing(mentionTestModel(), "workload/batch-runner", "workload/web-frontend", "httpproxy/edge")
	typeText(t, m, "is the frontend still crashing? check @")
	if got := topShortcut(m); got != "@workload/web-frontend" {
		t.Fatalf("the draft says frontend; rows = %v", rowLabels(m.mentionRows()))
	}

	// A word from a mention already written into the draft counts too: the
	// proxy and the workload share a name, and the draft is about that name.
	m = referencing(mentionTestModel(), "workload/web-frontend", "httpproxy/web-frontend", "httpproxy/edge")
	typeText(t, m, "@workload/web-frontend is 502ing, look at @")
	if got := topShortcut(m); got != "@httpproxy/web-frontend" {
		t.Fatalf("rows = %v, want the same-named proxy first", rowLabels(m.mentionRows()))
	}
}

// Anything already written into the draft is not offered again.
func TestRecentMentionsAlreadyInTheDraftAreDropped(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend")
	typeText(t, m, "@workload/api-backend and @")
	for _, label := range rowLabels(m.mentionRows()) {
		if label == "@workload/api-backend" {
			t.Fatalf("a mention already in the draft should not be offered again: %v", rowLabels(m.mentionRows()))
		}
	}
}

// The shortcut narrows with the query, on either half of the mention, and
// never displaces the kinds that let the user reach everything else.
func TestRecentMentionsFilterAlongsideTheKinds(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend", "httpproxy/edge")

	typeText(t, m, "@api")
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, []string{referencedHeader, "@workload/api-backend"}) {
		t.Fatalf("a name query should reach the referenced resource, got %v", got)
	}

	// "work" matches the referenced workload *and* the kind: both are offered,
	// so the other workloads in the project are still one tab away.
	m = referencing(mentionTestModel(), "workload/api-backend", "httpproxy/edge")
	typeText(t, m, "@work")
	want := []string{referencedHeader, "@workload/api-backend", allKindsHeader, "@workload/"}
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want the referenced workload and the kind, divided: %v", got, want)
	}

	// A query matching a referenced resource but no kind must not also claim
	// that nothing matches.
	m = referencing(mentionTestModel(), "workload/api-backend")
	typeText(t, m, "@api-back")
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, []string{referencedHeader, "@workload/api-backend"}) {
		t.Fatalf("rows = %v, want just the referenced resource", got)
	}
}

// Discovery is not needed to offer something the conversation already used, so
// the shortcut is there during the first keystrokes of a cold session.
func TestRecentMentionsShowWhileDiscoveryIsStillLoading(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend")
	m.mentions.kindsLoaded, m.mentions.kinds = false, nil
	typeText(t, m, "@")
	got := rowLabels(m.mentionRows())
	want := []string{referencedHeader, "@workload/api-backend", "loading resource kinds…"}
	if !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want the referenced resource above the loading line: %v", got, want)
	}
}

// Narrowing to a kind lands on the object this conversation is about rather
// than on whatever the listing returned first — but still lists them all.
func TestRecentMentionsLeadTheInstanceList(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/batch-runner")
	typeText(t, m, "@workload/")
	want := []string{"@workload/batch-runner", "@workload/web-frontend", "@workload/api-backend"}
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, want) {
		t.Fatalf("instances = %v, want the referenced one first: %v", got, want)
	}
}

// A referenced object the (capped) listing does not hold is still offered: the
// user pointed at it earlier, so it exists.
func TestRecentMentionsSurviveATruncatedListing(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/legacy-cron")
	typeText(t, m, "@workload/legacy")
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, []string{"@workload/legacy-cron"}) {
		t.Fatalf("rows = %v, want the referenced object even though the listing lacks it", got)
	}
}

// The shortcut belongs to the conversation: /clear starts a new one.
func TestClearForgetsTheReferencedResources(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend")
	typeText(t, m, "/clear")
	m.onKey(key(tea.KeyEnter, ""))
	if len(m.mentions.recent) != 0 {
		t.Fatalf("recent = %+v, want nothing after /clear", m.mentions.recent)
	}
}

// Resuming a conversation re-reads what its turns referenced, so the shortcut
// is there for the first message of the resumed session too — newest first,
// and only from what the user wrote.
func TestResumeRebuildsTheReferencedResources(t *testing.T) {
	m := mentionTestModel()
	m.layout(120, 40)
	m.Update(pickerTranscriptMsg{contextID: "ctx-7", items: []assistantv1alpha1.ConversationMessage{
		{Role: "user", Content: "is @workload/api-backend up?"},
		{Role: "assistant", Content: "it is, unlike @workload/web-frontend"},
		{Role: "user", Content: "what about @httpproxy/edge?"},
	}})
	got := make([]string, 0, len(m.mentions.recent))
	for _, mn := range m.mentions.recent {
		got = append(got, mn.Kind+"/"+mn.Name)
	}
	if want := []string{"httpproxy/edge", "workload/api-backend"}; !slices.Equal(got, want) {
		t.Fatalf("recent = %v, want %v — newest user reference first, the assistant's own ignored", got, want)
	}
	if m.mentions.recent[0].APIGroup != "networking.datumapis.com" {
		t.Fatalf("a resumed reference should carry its discovered group, got %+v", m.mentions.recent[0])
	}
}

// ── the section headers ────────────────────────────────────────

// A header is a label, not a row: ↑/↓ step over both of them, in both
// directions, so neither can be what tab/enter acts on — including on the
// list's first frame, where row 0 is a header.
func TestMentionHeadersAreSteppedOver(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend")
	typeText(t, m, "@")
	rows := m.mentionRows()
	if rows[0].rule == "" || rows[2].rule == "" {
		t.Fatalf("expected a header at 0 and 2, got %v", rowLabels(rows))
	}

	// The stored highlight is 0 — a header — so the list opens on row 1.
	if got := selectedIndex(rows, m.mentions.index); got != 1 {
		t.Fatalf("the list should open on the first shortcut, got row %d", got)
	}
	m.onKey(key(tea.KeyDown, ""))
	if m.mentions.index != 3 {
		t.Fatalf("down from the last shortcut should land past the header, got %d", m.mentions.index)
	}
	m.onKey(key(tea.KeyUp, ""))
	if m.mentions.index != 1 {
		t.Fatalf("up should come back over the header, got %d", m.mentions.index)
	}
	// Wrapping past the top lands on the last kind, not on a header.
	m.onKey(key(tea.KeyUp, ""))
	if m.mentions.index != len(rows)-1 {
		t.Fatalf("up from the top should wrap to the last row, got %d", m.mentions.index)
	}
	m.onKey(key(tea.KeyEnter, ""))
	if got := m.ta.Value(); got != "@workload/" {
		t.Fatalf("enter should accept the wrapped-to row, got %q", got)
	}
}

// Headers are drawn as labelled rules, under a blank row that keeps the whole
// list off the end of the transcript — and the bar stays exactly as tall as it
// told the layout it would be.
func TestMentionHeadersRenderAsRulesUnderAGap(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend")
	m.layout(120, 40)
	typeText(t, m, "@")
	rows := m.mentionRows()
	lines := strings.Split(plain(m.mentionBar(rows)), "\n")
	if lines[0] != "" {
		t.Fatalf("first line = %q, want a blank row between the transcript and the list", lines[0])
	}
	if !strings.HasPrefix(lines[1], "── referenced in this conversation ─") {
		t.Fatalf("line = %q, want the shortcut section named in a rule", lines[1])
	}
	if !strings.HasPrefix(lines[3], "── all resource kinds ─") {
		t.Fatalf("line = %q, want the kinds section named in a rule", lines[3])
	}
	if n := len(lines); n != m.suggestionRows() {
		t.Fatalf("mentionBar rendered %d lines, want the %d it budgeted", n, m.suggestionRows())
	}
}

// Naming the sections must not cost the user the rows the cap is there to
// bound: the cap counts resources, not labels.
func TestMentionHeadersDoNotEatTheRowBudget(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend", "httpproxy/edge")
	for _, tok := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		m.mentions.kinds = append(m.mentions.kinds, resourceKind{token: tok, plural: tok + "s", kind: tok, group: "g"})
	}
	typeText(t, m, "@")
	rows := m.mentionRows()
	height := mentionBarHeight(rows)
	resources := 0
	for _, r := range rows[:height] {
		if r.rule == "" {
			resources++
		}
	}
	if resources != maxMentionRows {
		t.Fatalf("%d resource rows on screen, want the full %d", resources, maxMentionRows)
	}
	if height != maxMentionRows+2 {
		t.Fatalf("bar height = %d, want the cap plus its two headers", height)
	}
}

// With nothing referenced yet there is nothing to name, and the list is the
// plain one it always was.
func TestMentionHeadersAbsentWithoutShortcuts(t *testing.T) {
	m := mentionTestModel()
	typeText(t, m, "@")
	for _, r := range m.mentionRows() {
		if r.rule != "" {
			t.Fatalf("a header with no section to name: %v", rowLabels(m.mentionRows()))
		}
	}
}

// A header is only drawn over rows that are there: a query matching one side
// and not the other gets one header, not two.
func TestMentionHeadersOnlyOverTheirOwnRows(t *testing.T) {
	m := referencing(mentionTestModel(), "workload/api-backend")
	typeText(t, m, "@api-back") // matches the shortcut, no kind
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, []string{referencedHeader, "@workload/api-backend"}) {
		t.Fatalf("rows = %v, want the shortcut and its header alone", got)
	}

	m = referencing(mentionTestModel(), "workload/api-backend")
	typeText(t, m, "@inst") // matches a kind, not the shortcut
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, []string{"@instance/"}) {
		t.Fatalf("rows = %v, want the kind alone, unheaded", got)
	}
}

// ── resources the assistant found ───────────────────────────────

// found feeds the model the resources a tool reported, the way a finished
// tool-activity event does.
func found(m *chatModel, tokens ...string) *chatModel {
	refs := make([]mention, 0, len(tokens))
	for _, tok := range tokens {
		kind, name, _ := strings.Cut(tok, "/")
		refs = append(refs, mention{Kind: kind, Name: name})
	}
	m.Update(streamActivityMsg{gen: m.turnGen, act: toolActivity{
		Phase: "finished", Name: "list_workloads", OK: true, Resources: refs,
	}})
	return m
}

// The case that started this: asking what workloads exist should leave them
// all one "@" away, without the user having typed any of them.
func TestFoundResourcesBecomeShortcuts(t *testing.T) {
	m := found(mentionTestModel(), "workload/web-frontend", "workload/api-backend")
	typeText(t, m, "@")
	want := []string{
		referencedHeader, "@workload/api-backend", "@workload/web-frontend",
		allKindsHeader, "@httpproxy/", "@instance/", "@workload/",
	}
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, want) {
		t.Fatalf("rows = %v, want the listed workloads offered: %v", got, want)
	}
}

// They rank by the draft like any other shortcut.
func TestFoundResourcesRankAgainstTheDraft(t *testing.T) {
	m := found(mentionTestModel(), "workload/web-frontend", "workload/api-backend", "workload/batch-runner")
	typeText(t, m, "is the frontend down? @")
	if got := topShortcut(m); got != "@workload/web-frontend" {
		t.Fatalf("top shortcut = %q, want the one the draft is about", got)
	}
}

// Equally relevant, what the user pointed at themselves goes first: the
// assistant's listing is a wider net than their own choice.
func TestTypedMentionsOutrankFoundOnes(t *testing.T) {
	m := found(mentionTestModel(), "workload/web-frontend", "workload/api-backend")
	m.noteMentions([]mention{{Kind: "workload", Name: "batch-runner"}})
	typeText(t, m, "@")
	if got := topShortcut(m); got != "@workload/batch-runner" {
		t.Fatalf("top shortcut = %q, want the resource the user typed", got)
	}
}

// A listing big enough to fill the memory must not evict the user's own
// references — the two sources are counted apart.
func TestFoundResourcesCannotEvictTypedOnes(t *testing.T) {
	m := mentionTestModel()
	m.noteMentions([]mention{{Kind: "workload", Name: "mine"}})
	names := make([]string, 0, maxFoundMentions*2)
	for i := range maxFoundMentions * 2 {
		names = append(names, fmt.Sprintf("workload/listed-%d", i))
	}
	found(m, names...)

	var typed, kept int
	for _, r := range m.mentions.recent {
		if r.typed {
			typed++
			if r.Name != "mine" {
				t.Fatalf("unexpected typed entry %+v", r)
			}
		} else {
			kept++
		}
	}
	if typed != 1 {
		t.Fatalf("the user's own reference was evicted by a big listing (%d typed left)", typed)
	}
	if kept != maxFoundMentions {
		t.Fatalf("kept %d found resources, want the cap of %d", kept, maxFoundMentions)
	}
	// And it is still reachable.
	typeText(t, m, "@mine")
	if got := rowLabels(m.mentionRows()); !slices.Equal(got, []string{referencedHeader, "@workload/mine"}) {
		t.Fatalf("rows = %v, want the typed reference still offered", got)
	}
}

// Typing a resource the assistant found promotes it: it is the user's own
// reference from then on, and is not demoted by a later listing either.
func TestTypingAFoundResourcePromotesIt(t *testing.T) {
	m := found(mentionTestModel(), "workload/web-frontend")
	m.noteMentions([]mention{{Kind: "workload", Name: "web-frontend"}})
	if len(m.mentions.recent) != 1 || !m.mentions.recent[0].typed {
		t.Fatalf("recent = %+v, want one promoted entry", m.mentions.recent)
	}
	found(m, "workload/web-frontend")
	if len(m.mentions.recent) != 1 || !m.mentions.recent[0].typed {
		t.Fatalf("recent = %+v, want the promotion to stick", m.mentions.recent)
	}
}

// A stale turn's events are dropped everywhere else; they must not seed the
// picker either.
func TestFoundResourcesFromAStaleTurnAreIgnored(t *testing.T) {
	m := mentionTestModel()
	m.Update(streamActivityMsg{gen: m.turnGen + 1, act: toolActivity{
		Phase: "finished", Name: "list_workloads", OK: true,
		Resources: []mention{{Kind: "workload", Name: "ghost"}},
	}})
	if len(m.mentions.recent) != 0 {
		t.Fatalf("recent = %+v, want nothing from an abandoned turn", m.mentions.recent)
	}
}
