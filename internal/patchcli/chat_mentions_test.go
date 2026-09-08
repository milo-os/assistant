package patchcli

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func rowLabels(rows []mentionRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
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
	if got := m.suggestionRows(); got != 3 {
		t.Fatalf("suggestionRows = %d, want one per kind", got)
	}
	rows := m.mentionRows()
	if n := strings.Count(m.mentionBar(rows), "\n") + 1; n != 3 {
		t.Fatalf("mentionBar rendered %d lines, want 3", n)
	}
	// Capped, and still exactly as tall as it claims.
	m.mentions.kinds = make([]resourceKind, 0, 12)
	for _, tok := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		m.mentions.kinds = append(m.mentions.kinds, resourceKind{token: tok, plural: tok + "s", kind: tok, group: "g"})
	}
	if got := m.suggestionRows(); got != maxMentionRows {
		t.Fatalf("suggestionRows = %d, want the cap of %d", got, maxMentionRows)
	}
	if n := strings.Count(m.mentionBar(m.mentionRows()), "\n") + 1; n != maxMentionRows {
		t.Fatalf("mentionBar rendered %d lines, want %d", n, maxMentionRows)
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
