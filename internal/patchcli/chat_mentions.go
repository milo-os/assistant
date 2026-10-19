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
	"charm.land/lipgloss/v2"
)

// maxMentionRows caps how tall the mention list can grow. It is roomier than
// maxSuggestionRows because the slash commands are a closed set of nine while a
// project's kinds and instances are not.
const maxMentionRows = 6

// mentionGapRows is the blank row the list keeps above itself. The transcript
// ends wherever the last answer ended, so without it a finished turn and the
// first row of the list are simply two adjacent lines of text and the list
// reads as part of the conversation.
const mentionGapRows = 1

// How many referenced resources a session remembers, and how many of them the
// list offers at once. The two sources are capped apart so that one answer
// listing forty workloads cannot evict what the user themselves pointed at.
// The list cap is far smaller than either: the shortcut is only a shortcut
// while it fits above the kinds without burying them — three plus the two
// section headers leaves two kinds on screen inside maxMentionRows.
const (
	maxTypedMentions = 20
	maxFoundMentions = 40
	maxRecentRows    = 3
)

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

	// recent is every resource this conversation has touched, most recently
	// first. It is the picker's shortcut: what a turn is about is nearly always
	// what the last turns were about, so these are offered straight away — no
	// kind to pick first, no listing to wait for — ranked by what the new
	// message is being drafted about (see rankRecent). They sit above the kinds
	// rather than replacing them, so every other resource in the project stays
	// one keystroke of filtering away.
	recent []recentResource

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
//
// A row with a rule is neither: it is the labelled divider between the
// conversation's own resources and everything else, drawn as a rule rather
// than a row of text and stepped straight over by ↑/↓.
type mentionRow struct {
	insert string
	label  string
	desc   string
	rule   string
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
	prose := m.draftProse()
	ranked := rankRecent(m.mentions.recent, prose)

	token, rest, narrowed := strings.Cut(q, "/")
	if !narrowed {
		// The referenced-already rows need no discovery, so they are offered
		// while it is still in flight — the status line then explains why the
		// kinds underneath them have not arrived instead of standing alone.
		rows := recentRows(ranked, q, parseMentions(prose))
		// Two lists of "@…" rows run together read as one long list, and the
		// top one is the surprising half: nothing about "@workload/x" says why
		// it is being offered before a kind has even been picked. So each
		// section is named — which is also why the rows themselves carry no
		// label of their own (see recentRows). A header is only ever drawn
		// over rows that are actually there.
		if len(rows) > 0 {
			rows = append([]mentionRow{{rule: "referenced in this conversation"}}, rows...)
		}
		if !m.mentions.kindsLoaded {
			if m.mentions.kindsErr != "" {
				return append(rows, mentionRow{label: "resource kinds unavailable", desc: m.mentions.kindsErr})
			}
			return append(rows, mentionRow{label: "loading resource kinds…"})
		}
		kinds := kindRows(m.mentions.kinds, q, len(rows) > 0)
		if len(rows) > 0 && len(kinds) > 0 {
			rows = append(rows, mentionRow{rule: "all resource kinds"})
		}
		return append(rows, kinds...)
	}

	if !m.mentions.kindsLoaded {
		if m.mentions.kindsErr != "" {
			return []mentionRow{{label: "resource kinds unavailable", desc: m.mentions.kindsErr}}
		}
		return []mentionRow{{label: "loading resource kinds…"}}
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
	return instanceRows(k, names, rest, ranked)
}

// mentionBarHeight is how many rows the list needs, gap excluded. The cap is on
// how many *resources* are on screen, so a list that spends rows on section
// headers is allowed those rows back — capping the two together would mean
// naming the sections cost the user two of them.
func mentionBarHeight(rows []mentionRow) int {
	height := maxMentionRows
	for _, r := range rows {
		if r.rule != "" {
			height++
		}
	}
	return min(len(rows), height)
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
// quiet drops the "nothing matches" line, for when rows above have already
// matched the query and the list would otherwise contradict itself.
func kindRows(kinds []resourceKind, query string, quiet bool) []mentionRow {
	matched := filterByScore(kinds, query, func(k resourceKind) string { return k.token })
	if len(matched) == 0 {
		if quiet {
			return nil
		}
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
//
// ranked is the conversation's already-referenced resources in relevance order
// (see rankRecent); the ones of this kind are moved to the front of the
// listing, so narrowing to a kind lands on the object this turn is most likely
// about rather than on whichever object the apiserver returned first.
func instanceRows(k resourceKind, names []string, query string, ranked []mention) []mentionRow {
	candidates := frontloadRecent(k, names, ranked)
	matched := filterByScore(candidates, query, func(s string) string { return s })
	if len(matched) == 0 {
		if len(candidates) == 0 {
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

// frontloadRecent returns k's object names with the ones this conversation has
// already referenced first, in ranked order. A referenced name the listing does
// not hold is kept anyway: the user pointed at it earlier and it resolved, so
// it exists — the listing is capped (maxMentionInstances) and may simply not
// reach it.
func frontloadRecent(k resourceKind, names []string, ranked []mention) []string {
	var front []string
	seen := make(map[string]bool, len(ranked))
	for _, mn := range ranked {
		if mn.Kind == k.token && !seen[mn.Name] {
			seen[mn.Name] = true
			front = append(front, mn.Name)
		}
	}
	if len(front) == 0 {
		return names
	}
	out := make([]string, 0, len(front)+len(names))
	out = append(out, front...)
	for _, n := range names {
		if !seen[n] {
			out = append(out, n)
		}
	}
	return out
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

// ── what the conversation has already referenced ──────────────

// recentResource is one remembered resource and where it came from: typed is a
// resource the user pointed at with "@", as against one a tool reported back
// mid-turn. Both are offered, but a turn that lists forty workloads should not
// be able to crowd out the one the user named, so the two are counted — and
// evicted — separately, and typed wins a tie in the ranking.
type recentResource struct {
	mention
	typed bool
}

// noteMentions records resources the user pointed at. See noteResources for the
// shared bookkeeping; called with each submitted message's mentions and with a
// resumed transcript's, in the order the turns happened.
func (m *chatModel) noteMentions(ms []mention) { m.noteResources(ms, true) }

// noteFound records resources a tool reported back during a turn (the
// `resources` field of a finished tool-activity event). These are the answer's
// own subject matter — "which workloads are failing?" names resources the user
// has no other way to point at — so they join the shortcut list too, below
// anything typed.
func (m *chatModel) noteFound(ms []mention) { m.noteResources(ms, false) }

// noteResources folds resources into the shortcut list, most recently first and
// without duplicates: seeing one again moves it back to the front rather than
// adding a second entry, and a resource the user types is promoted to typed for
// good.
func (m *chatModel) noteResources(ms []mention, typed bool) {
	for _, mn := range ms {
		if mn.Kind == "" || mn.Name == "" {
			continue
		}
		entry := recentResource{mention: mn, typed: typed}
		recent := make([]recentResource, 0, len(m.mentions.recent)+1)
		recent = append(recent, entry)
		for _, old := range m.mentions.recent {
			if old.Kind == mn.Kind && old.Name == mn.Name {
				recent[0].typed = recent[0].typed || old.typed
				continue
			}
			recent = append(recent, old)
		}
		m.mentions.recent = capRecent(recent)
	}
}

// capRecent trims the list to the per-source caps, keeping the most recent of
// each. Counting the sources apart is what makes one big listing cost the
// user's own references nothing.
func capRecent(recent []recentResource) []recentResource {
	out := recent[:0]
	var typed, found int
	for _, r := range recent {
		if r.typed {
			if typed++; typed > maxTypedMentions {
				continue
			}
		} else if found++; found > maxFoundMentions {
			continue
		}
		out = append(out, r)
	}
	return out
}

// draftProse is the message being drafted with the "@" token under the cursor
// cut out: that token is the query, scored on its own terms, while everything
// around it is what the user is actually saying — and that is what a resource's
// relevance is measured against. Mentions already written into the draft stay
// in, because their words describe this turn's subject as much as any other
// word does ("@workload/web-frontend is 502ing, check @" should reach for the
// proxy called web-frontend).
func (m *chatModel) draftProse() string {
	text := m.ta.Value()
	q, ok := m.mentionQuery()
	if !ok {
		return text
	}
	token := "@" + q
	if i := strings.LastIndex(text, token); i >= 0 {
		return text[:i] + " " + text[i+len(token):]
	}
	return text
}

// rankRecent orders the conversation's resources by how much each has to do
// with the message being drafted, then by source (typed over found), then by
// recency (the input order) — so before a word is typed the list is simply
// "most recent first", and it re-sorts as the sentence takes shape.
func rankRecent(recent []recentResource, draft string) []mention {
	if len(recent) == 0 {
		return nil
	}
	words := wordsIn(draft)
	type scored struct {
		r   recentResource
		rel int
	}
	hits := make([]scored, 0, len(recent))
	for _, r := range recent {
		hits = append(hits, scored{r, relevanceTo(r.mention, words)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rel != hits[j].rel {
			return hits[i].rel > hits[j].rel
		}
		// Equally relevant: what the user themselves pointed at goes first.
		return hits[i].r.typed && !hits[j].r.typed
	})
	out := make([]mention, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.r.mention)
	}
	return out
}

// relevanceTo counts the words a resource and the draft have in common.
func relevanceTo(mn mention, draft map[string]bool) int {
	if len(draft) == 0 {
		return 0
	}
	score := 0
	for w := range wordsIn(mn.Kind + " " + mn.Name) {
		if draft[w] {
			score++
		}
	}
	return score
}

// wordsIn is the vocabulary relevance is measured in: lowercased runs of
// letters and digits, plus the "-._"-joined tokens they came from, so a draft
// that says "frontend" still reaches @workload/web-frontend and one that says
// "web-frontend" still matches it whole. Words shorter than three runes are
// dropped — "a", "is" and "v1" are in every sentence and single out nothing.
func wordsIn(text string) map[string]bool {
	words := map[string]bool{}
	add := func(w string) {
		if len([]rune(w)) >= 3 {
			words[w] = true
		}
	}
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-._", r)
	}) {
		token = strings.Trim(token, "-._")
		if token == "" {
			continue
		}
		add(token)
		for _, part := range strings.FieldsFunc(token, func(r rune) bool {
			return strings.ContainsRune("-._", r)
		}) {
			add(part)
		}
	}
	return words
}

// recentRows are the shortcut rows at the top of the list: referenced-already
// resources matching what follows the "@", in the order rankRecent put them.
// Unlike a kind row, accepting one is the whole mention — "@kind/name " —
// because the resource is already known.
//
// They carry no description. A per-row "referenced earlier" would repeat the
// same words down the left of the bar for no gain; the divider under the last
// of them says it once (see mentionRows).
//
// Query matching is on "kind/name" and on the name alone, so both halves of
// what the user remembers reach it. Anything already written into the draft is
// dropped: it has been referenced, and offering it again is a row spent on
// something nobody is going to accept.
func recentRows(ranked []mention, query string, drafted []mention) []mentionRow {
	if len(ranked) == 0 {
		return nil
	}
	written := make(map[string]bool, len(drafted))
	for _, mn := range drafted {
		written[mn.Kind+"/"+mn.Name] = true
	}
	rows := make([]mentionRow, 0, maxRecentRows)
	for _, band := range []int{2, 1} { // prefix matches ahead of subsequence ones
		for _, mn := range ranked {
			token := mn.Kind + "/" + mn.Name
			if written[token] {
				continue
			}
			if max(mentionScore(token, query), mentionScore(mn.Name, query)) != band {
				continue
			}
			rows = append(rows, mentionRow{insert: token + " ", label: "@" + token})
			if len(rows) == maxRecentRows {
				return rows
			}
		}
	}
	return rows
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
		step := 1
		if msg.String() == "up" {
			step = -1
		}
		m.mentions.index = nextSelectable(rows, selectedIndex(rows, m.mentions.index), step)
		return true, nil
	case "tab", "enter":
		row := rows[selectedIndex(rows, m.mentions.index)]
		if row.insert == "" {
			// Nothing to accept: swallow tab (it would do nothing anyway) but
			// let enter send, so a typo can't trap the message in the composer.
			return msg.String() == "tab", nil
		}
		return true, m.acceptMention(row)
	}
	return false, nil
}

// nextSelectable is the index one step from i in the given direction, wrapping
// at both ends and passing over the section headers — which are labels, not
// somewhere the highlight can rest. A list of nothing but headers (there is no
// such list) would return i rather than spin.
func nextSelectable(rows []mentionRow, i, step int) int {
	for range len(rows) {
		i = (i + step + len(rows)) % len(rows)
		if rows[i].rule == "" {
			return i
		}
	}
	return i
}

// selectedIndex resolves the stored highlight against the rows actually on
// screen. The highlight is reset to 0 whenever the query changes, and row 0 is
// a section header as often as not, so "highlighted" means the first row from
// there that can actually be accepted.
func selectedIndex(rows []mentionRow, i int) int {
	if len(rows) == 0 {
		return 0
	}
	i %= len(rows)
	if rows[i].rule == "" {
		return i
	}
	return nextSelectable(rows, i, 1)
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
	height := mentionBarHeight(rows)
	idx := selectedIndex(rows, m.mentions.index)

	start := 0
	if len(rows) > height {
		start = max(idx-height/2, 0)
		start = min(start, len(rows)-height)
	}
	end := min(start+height, len(rows))

	lines := make([]string, 0, height)
	for i := start; i < end; i++ {
		row := rows[i]
		if row.rule != "" {
			lines = append(lines, m.mentionRule(row.rule))
			continue
		}
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
	return strings.Repeat("\n", mentionGapRows) + strings.Join(lines, "\n")
}

// mentionRule draws a divider row: its label set into a rule that runs to the
// width of the bar, in the same subtle token the transcript's own rules use
// (compactionRule), so it separates the two lists without competing with them.
// The label leads rather than centring — the eye is travelling down the left
// edge of the rows, not across them.
func (m *chatModel) mentionRule(label string) string {
	lead, text := "── ", label+" "
	fill := m.contentWidth() - lipgloss.Width(lead+text)
	if fill < 0 {
		fill = 0
	}
	return m.st.subtle.Render(lead + text + strings.Repeat("─", fill))
}

// mentionsIn parses the mentions out of a message about to be sent and labels
// each with its API group from the session's discovery, where it is known.
func (m *chatModel) mentionsIn(text string) []mention {
	return resolveMentionGroups(parseMentions(text), m.mentions.kinds)
}
