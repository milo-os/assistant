// The chat TUI's "@" resource picker: the inline list that opens when an "@"
// is typed at a word boundary, first offering the project's resource kinds and
// then, once a "kind/" is settled on, that kind's instances.
//
// It shares the one variable-height bar above the composer with the slash
// commands and the ctrl+r search line (see composerBar in chat_tui.go), so the
// transcript viewport trades exactly the rows this list takes.
//
// What is on screen is derived from the composer's text and cursor, never from
// a mode flag — the same approach currentSuggestions takes — so there is no way
// for the list and the input to disagree. Fetching is the one thing that is
// stateful: discovery is cached for the session and each kind's instances are
// fetched once, on the keystroke that first narrows to that kind.
package patchcli

import (
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// maxMentionRows caps how tall the mention list can grow. It is roomier than
// maxSuggestionRows because the slash commands are a closed set of nine while a
// project's kinds and instances are not.
const maxMentionRows = 6

// mentionState is everything the "@" picker keeps between keystrokes: the
// session's discovery cache, one instance listing per kind, and where the
// highlight sits.
type mentionState struct {
	kinds       []resourceKind
	kindsErr    string
	kindsLoaded bool
	loading     bool

	names    map[string][]string // kind token → that kind's object names
	namesErr map[string]string   // kind token → why the listing failed
	pending  map[string]bool     // kind token → a listing is in flight

	// dismissedQ is the query esc was pressed on; the list stays closed until
	// the token under the cursor is something else.
	dismissed  bool
	dismissedQ string

	index int
}

// Messages delivered by the background fetches below.
type (
	// mentionKindsMsg carries the session's API discovery (or why it failed).
	mentionKindsMsg struct {
		kinds []resourceKind
		err   error
	}
	// mentionNamesMsg carries one kind's object names (or why they failed).
	mentionNamesMsg struct {
		token string
		names []string
		err   error
	}
)

// mentionRow is one line of the list. insert is what accepting puts in the
// composer after the "@"; an empty insert marks a status row (loading, an
// error, no matches) that can be looked at but not accepted.
type mentionRow struct {
	insert string
	label  string
	desc   string
}

// ── the token under the cursor ────────────────────────────────

// mentionQuery returns the text after the "@" the cursor is currently inside,
// and whether there is one at all. The "@" must start a word — an "@" with a
// word character in front of it belongs to an email address, not a mention.
func (m *chatModel) mentionQuery() (string, bool) {
	lines := strings.Split(m.ta.Value(), "\n")
	row, col := m.ta.Line(), m.ta.Column()
	if row < 0 || row >= len(lines) {
		return "", false
	}
	r := []rune(lines[row])
	if col > len(r) {
		col = len(r)
	}
	i := col
	for i > 0 && isMentionRune(r[i-1]) {
		i--
	}
	if i == 0 || r[i-1] != '@' {
		return "", false
	}
	if at := i - 1; at > 0 && !isMentionBoundary(r[at-1]) {
		return "", false
	}
	return string(r[i:col]), true
}

// isMentionRune reports whether a rune can appear inside a mention token.
func isMentionRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_' || r == '/'
}

// isMentionBoundary reports whether a rune can sit immediately before the "@"
// that opens a mention: whitespace, or an opening bracket or quote.
func isMentionBoundary(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("([{\"'", r)
}

// ── the rows ──────────────────────────────────────────────────

// mentionRows is the list to show for the composer's current state, or nil when
// the "@" picker is not open. Pure: View and onKey both call it, and neither
// may start a fetch (that is ensureMentions' job).
func (m *chatModel) mentionRows() []mentionRow {
	q, ok := m.mentionQuery()
	if !ok || (m.mentions.dismissed && m.mentions.dismissedQ == q) {
		return nil
	}
	if !m.mentions.kindsLoaded {
		if m.mentions.kindsErr != "" {
			return []mentionRow{{label: "resource kinds unavailable", desc: m.mentions.kindsErr}}
		}
		return []mentionRow{{label: "loading resource kinds…"}}
	}

	token, rest, narrowed := strings.Cut(q, "/")
	if !narrowed {
		return kindRows(m.mentions.kinds, q)
	}
	k, found := m.kindByToken(token)
	if !found {
		return []mentionRow{{label: "no kind called " + token}}
	}
	if err := m.mentions.namesErr[k.token]; err != "" {
		return []mentionRow{{label: "could not list " + k.plural, desc: err}}
	}
	names, listed := m.mentions.names[k.token]
	if !listed {
		return []mentionRow{{label: "loading " + k.plural + "…"}}
	}
	return instanceRows(k, names, rest)
}

// kindByToken finds a discovered kind by its mention token.
func (m *chatModel) kindByToken(token string) (resourceKind, bool) {
	for _, k := range m.mentions.kinds {
		if k.token == token {
			return k, true
		}
	}
	return resourceKind{}, false
}

// kindRows are the first-level rows: the kinds matching what follows the "@".
// Accepting one inserts "@kind/", which leaves the list open on its instances.
func kindRows(kinds []resourceKind, query string) []mentionRow {
	matched := filterByScore(kinds, query, func(k resourceKind) string { return k.token })
	if len(matched) == 0 {
		return []mentionRow{{label: "no resource kind matches " + query}}
	}
	rows := make([]mentionRow, 0, len(matched))
	for _, k := range matched {
		rows = append(rows, mentionRow{
			insert: k.token + "/",
			label:  "@" + k.token + "/",
			desc:   k.kind + " · " + k.group,
		})
	}
	return rows
}

// instanceRows are the second-level rows: one kind's objects, filtered by what
// follows the "/". Accepting one inserts "@kind/name " — the trailing space
// both closes the list and separates the mention from the next word.
func instanceRows(k resourceKind, names []string, query string) []mentionRow {
	matched := filterByScore(names, query, func(s string) string { return s })
	if len(matched) == 0 {
		if len(names) == 0 {
			return []mentionRow{{label: "no " + k.plural + " in this project"}}
		}
		return []mentionRow{{label: "no " + k.plural + " match " + query}}
	}
	rows := make([]mentionRow, 0, len(matched))
	for _, name := range matched {
		rows = append(rows, mentionRow{
			insert: k.token + "/" + name + " ",
			label:  "@" + k.token + "/" + name,
		})
	}
	return rows
}

// filterByScore keeps the candidates matching query and puts prefix matches
// ahead of looser subsequence ones, preserving the input order within each
// band so rows do not reshuffle as the query narrows.
func filterByScore[T any](items []T, query string, key func(T) string) []T {
	type scored struct {
		item  T
		score int
	}
	var hits []scored
	for _, it := range items {
		if s := mentionScore(key(it), query); s > 0 {
			hits = append(hits, scored{it, s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]T, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.item)
	}
	return out
}

// mentionScore rates a candidate against a query: 2 for a prefix match, 1 for a
// subsequence (the letters in order, gaps allowed — "hpx" finds "httpproxy"),
// 0 for no match. An empty query matches everything.
func mentionScore(candidate, query string) int {
	if query == "" {
		return 2
	}
	c := strings.ToLower(candidate)
	q := []rune(strings.ToLower(query))
	if strings.HasPrefix(c, string(q)) {
		return 2
	}
	i := 0
	for _, r := range c {
		if i < len(q) && q[i] == r {
			i++
		}
	}
	if i == len(q) {
		return 1
	}
	return 0
}

// ── fetching ──────────────────────────────────────────────────

// ensureMentions starts whatever fetch the current token needs and nothing
// else: discovery once per session, then one listing per kind the user narrows
// to. Called from onKey after the composer has taken the keystroke, so it runs
// on the Update goroutine and may touch model state.
func (m *chatModel) ensureMentions() tea.Cmd {
	q, ok := m.mentionQuery()
	if !ok {
		// The cursor left the token; a later "@" starts fresh rather than
		// inheriting an esc from this one.
		m.mentions.dismissed = false
		return nil
	}
	if m.mentions.dismissed && m.mentions.dismissedQ == q {
		return nil
	}
	if !m.mentions.kindsLoaded {
		if m.mentions.loading || m.mentions.kindsErr != "" {
			return nil
		}
		m.mentions.loading = true
		return m.loadMentionKinds()
	}
	token, _, narrowed := strings.Cut(q, "/")
	if !narrowed {
		return nil
	}
	k, found := m.kindByToken(token)
	if !found {
		return nil
	}
	if _, listed := m.mentions.names[k.token]; listed {
		return nil
	}
	if m.mentions.pending[k.token] || m.mentions.namesErr[k.token] != "" {
		return nil
	}
	if m.mentions.pending == nil {
		m.mentions.pending = map[string]bool{}
	}
	m.mentions.pending[k.token] = true
	return m.loadMentionNames(k)
}

// loadMentionKinds fetches the project's API discovery in the background. Like
// the picker's loaders it captures ctx/view/project by value: the closure runs
// off the Update goroutine and must not read mutable model state.
func (m *chatModel) loadMentionKinds() tea.Cmd {
	ctx, view, project := m.ctx, m.view, m.project
	return func() tea.Msg {
		kinds, err := discoverResourceKinds(ctx, view, project)
		return mentionKindsMsg{kinds: kinds, err: err}
	}
}

// loadMentionNames fetches one kind's object names in the background. Same
// off-goroutine caveat as loadMentionKinds.
func (m *chatModel) loadMentionNames(k resourceKind) tea.Cmd {
	ctx, view, project := m.ctx, m.view, m.project
	return func() tea.Msg {
		names, err := listResourceNames(ctx, view, project, k)
		return mentionNamesMsg{token: k.token, names: names, err: err}
	}
}

// applyMentionKinds folds a finished discovery into the cache. A failure is
// remembered as text, not retried: it would fail the same way on the next
// keystroke, and the picker shows it on one line.
func (m *chatModel) applyMentionKinds(msg mentionKindsMsg) {
	m.mentions.loading = false
	if msg.err != nil {
		m.mentions.kindsErr = msg.err.Error()
		return
	}
	m.mentions.kinds = msg.kinds
	m.mentions.kindsLoaded = true
	m.mentions.index = 0
}

// applyMentionNames folds one finished listing into the cache, on the same
// remember-the-failure terms as applyMentionKinds.
func (m *chatModel) applyMentionNames(msg mentionNamesMsg) {
	delete(m.mentions.pending, msg.token)
	if msg.err != nil {
		if m.mentions.namesErr == nil {
			m.mentions.namesErr = map[string]string{}
		}
		m.mentions.namesErr[msg.token] = msg.err.Error()
		return
	}
	if m.mentions.names == nil {
		m.mentions.names = map[string][]string{}
	}
	m.mentions.names[msg.token] = msg.names
	m.mentions.index = 0
}

// ── keys ──────────────────────────────────────────────────────

// onMentionKey handles the keys the "@" list owns while it is open, reporting
// whether it took the key. Anything it does not take falls through to the
// composer's normal handling — enter on a status row still sends the message.
func (m *chatModel) onMentionKey(msg tea.KeyPressMsg, rows []mentionRow) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mentions.dismissed = true
		m.mentions.dismissedQ, _ = m.mentionQuery()
		m.mentions.index = 0
		return true, nil
	case "up", "down":
		if msg.String() == "up" {
			m.mentions.index = (m.mentions.index - 1 + len(rows)) % len(rows)
		} else {
			m.mentions.index = (m.mentions.index + 1) % len(rows)
		}
		return true, nil
	case "tab", "enter":
		row := rows[m.mentions.index%len(rows)]
		if row.insert == "" {
			// Nothing to accept: swallow tab (it would do nothing anyway) but
			// let enter send, so a typo can't trap the message in the composer.
			return msg.String() == "tab", nil
		}
		return true, m.acceptMention(row)
	}
	return false, nil
}

// acceptMention replaces the token under the cursor with the accepted row. The
// old token is removed by feeding the composer its own backspace, so the cursor
// ends up where the textarea itself put it — mentions can be completed in the
// middle of a sentence, not just at the end.
func (m *chatModel) acceptMention(row mentionRow) tea.Cmd {
	q, ok := m.mentionQuery()
	if !ok || row.insert == "" {
		return nil
	}
	for range len([]rune(q)) + 1 { // + the "@" itself
		m.ta, _ = m.ta.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	m.ta.InsertString("@" + row.insert)
	m.mentions.index = 0
	m.mentions.dismissed = false
	return m.ensureMentions()
}

// ── rendering ─────────────────────────────────────────────────

// mentionBar renders the "@" list into the shared bar above the input, windowed
// around the highlighted row and padded to exactly m.suggestionRows() lines so
// View's height budget never has to guess. Mirrors suggestionBar.
func (m *chatModel) mentionBar(rows []mentionRow) string {
	height := m.suggestionRows()
	idx := m.mentions.index % len(rows)

	start := 0
	if len(rows) > height {
		start = max(idx-height/2, 0)
		start = min(start, len(rows)-height)
	}
	end := min(start+height, len(rows))

	lines := make([]string, 0, height)
	for i := start; i < end; i++ {
		row := rows[i]
		desc := row.desc
		if i == idx && row.insert != "" {
			desc = strings.TrimSpace(desc + "   tab/enter inserts · ↑↓ select · esc closes")
			lines = append(lines, m.st.you.Render(row.label)+"  "+m.st.hint.Render(desc))
			continue
		}
		line := m.st.subtle.Render(row.label)
		if desc != "" {
			line += "  " + m.st.subtle.Render(desc)
		}
		lines = append(lines, line)
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// mentionsIn parses the mentions out of a message about to be sent and labels
// each with its API group from the session's discovery, where it is known.
func (m *chatModel) mentionsIn(text string) []mention {
	return resolveMentionGroups(parseMentions(text), m.mentions.kinds)
}
