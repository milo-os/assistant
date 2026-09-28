// The chat TUI's conversation picker: the full-screen overlay behind `/resume`
// and `patch resume`, modelled on the session picker in coding agents like
// Claude Code — a search box on top, the project's conversations under it
// newest first, each as a title (its opening message) over an age/size/id
// line, and a live preview of the highlighted one alongside when the terminal
// is wide enough.
//
// Listing and transcripts come from the conversations apiserver through the
// same ReadView as `patch conversations`, never the chat transport:
// per the apiserver design, discovery and resuming are separate paths, and
// this overlay is the discovery half. It is also where a conversation's
// lifecycle is managed by hand — ctrl+a archives (or unarchives) and ctrl+d
// deletes the highlighted row, through that same ReadView — since this is the
// one screen that shows a person all of their conversations at once.
package patchcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// pickerState is the picker's model: the listing, the search query and the
// subset of the listing it selects, the cursor within that subset, and the
// preview cache.
type pickerState struct {
	open    bool
	loading bool
	// direct is a `patch resume <id>` start: one transcript is loading with
	// no listing behind it, so the loading/error screens read differently.
	direct bool
	err    string

	search textinput.Model
	// focus is where tab has moved the keyboard: the search box, or one of
	// the two options on the search line, so ←/→ change that option instead
	// of moving the search cursor.
	focus pickerFocus
	// sortCreated orders rows by creation time instead of last activity.
	sortCreated bool
	// archived shows the archive (spec.archived=true) instead of the
	// conversations in everyday use. It is a separate listing fetched from
	// the apiserver, not a filter over one already held, because the default
	// listing never contains archived conversations at all.
	archived bool
	// comfortable adds a detail line (size, id) under every row (ctrl+o);
	// transcript shows the highlighted conversation's preview under the
	// list (ctrl+t). Both are off by default so the list stays dense.
	comfortable bool
	transcript  bool

	items []assistantv1alpha1.Conversation
	// filtered indexes items — the rows the current query matches, in the
	// listing's order (newest activity first). cursor indexes filtered.
	filtered []int
	cursor   int

	// preview/previewErr/previewPending back the preview pane: the highlighted
	// conversation's transcript is fetched lazily as the cursor moves and
	// cached by contextID (conversation Name) so revisiting an item never
	// re-fetches. previewPending guards against firing a second fetch for the
	// same id while one is already in flight.
	preview        map[string][]assistantv1alpha1.ConversationMessage
	previewErr     map[string]string
	previewPending map[string]bool

	// confirmDelete is the context id ctrl+d is waiting on a y for; the next
	// key either confirms it or cancels it (see onPickerKey).
	confirmDelete string
	// acting is the context id an archive or delete request is in flight
	// for. One at a time: the notice line reports a single outcome, and a
	// second request racing the first would leave it reporting the wrong one.
	acting string
	// notice is a one-line status under the list — the outcome of the last
	// archive/delete, or its progress while acting — and noticeErr marks it a
	// failure. Unlike err it never replaces the list: a failed archive leaves
	// every row where it was.
	notice    string
	noticeErr bool
}

// pickerFocus is which part of the picker's search line has the keyboard.
type pickerFocus int

const (
	pickerFocusSearch pickerFocus = iota
	pickerFocusSort
	pickerFocusShow
	pickerFocusCount
)

// newPickerState returns an open, loading picker with a focused, empty search
// box and its preview caches ready to write to (a zero pickerState's maps are
// nil).
func newPickerState(dark bool) pickerState {
	search := textinput.New()
	search.Placeholder = "Type to search"
	search.Prompt = ""
	search.SetVirtualCursor(true)
	search.SetStyles(newInputStyles(dark))
	search.Focus()
	return pickerState{
		open:           true,
		loading:        true,
		search:         search,
		preview:        map[string][]assistantv1alpha1.ConversationMessage{},
		previewErr:     map[string]string{},
		previewPending: map[string]bool{},
	}
}

// selected returns the highlighted conversation, if any.
func (p *pickerState) selected() (assistantv1alpha1.Conversation, bool) {
	if len(p.filtered) == 0 || p.cursor < 0 || p.cursor >= len(p.filtered) {
		return assistantv1alpha1.Conversation{}, false
	}
	return p.items[p.filtered[p.cursor]], true
}

// refilter recomputes filtered from the current query and sort, and rewinds
// the cursor to the top match.
func (p *pickerState) refilter() {
	p.filtered = filterConversations(p.items, p.search.Value())
	at := func(i int) time.Time {
		if p.sortCreated {
			return p.items[i].CreationTimestamp.Time
		}
		return p.items[i].Status.LastActiveAt.Time
	}
	sort.SliceStable(p.filtered, func(a, b int) bool { return at(p.filtered[a]).After(at(p.filtered[b])) })
	p.cursor = 0
}

// filterConversations returns the indexes of the conversations matching
// query, in the input's order. Every whitespace-separated term must appear,
// case-insensitively, in the name, the title or the context id — so "quota dfw"
// narrows the way a person expects, without a fuzzy-match ranking that
// would reorder rows out of newest-first as they type. The name is searched
// alongside the title, not instead of it: someone who renamed a conversation
// may well still look for it by what they originally asked.
func filterConversations(items []assistantv1alpha1.Conversation, query string) []int {
	terms := strings.Fields(strings.ToLower(query))
	out := make([]int, 0, len(items))
	for i, c := range items {
		hay := strings.ToLower(c.Status.Name + " " + c.Status.Title + " " + c.Name)
		ok := true
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// openPicker opens the picker and starts the listing fetch.
func (m *chatModel) openPicker() tea.Cmd {
	m.picker = newPickerState(m.dark)
	return tea.Batch(m.loadPickerList(), m.sp.Tick, textinput.Blink)
}

// resumeDirect loads one conversation's transcript without the listing —
// `patch resume <context-id>`. The picker overlay shows the loading spinner
// and, should the fetch fail, the error with esc as the way out into a fresh
// chat; on success the transcript lands in the chat exactly as a picked row
// would (pickerTranscriptMsg).
func (m *chatModel) resumeDirect(contextID string) tea.Cmd {
	m.picker = newPickerState(m.dark)
	m.picker.direct = true
	return tea.Batch(m.loadPickerTranscript(contextID), m.sp.Tick)
}

// onPickerKey handles input while the picker is open. Navigation keys move
// the cursor, enter resumes, esc cancels, tab cycles focus from the search box
// through the Sort and Show options (←/→ change whichever is focused),
// ctrl+o/ctrl+t toggle the detail line and the transcript preview, ctrl+a
// archives the highlighted conversation (unarchives it, when showing the
// archive), ctrl+d deletes it behind an inline y/n, and everything else edits
// the search box — so typing narrows the list rather than leaking into the
// chat input underneath.
func (m *chatModel) onPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// A pending delete owns the very next key. Only an explicit y may destroy
	// a conversation; anything else — esc, an arrow, a stray letter — cancels,
	// and is consumed rather than also acting on the list, so the reflexive
	// esc that backs out of the question does not close the picker as well.
	if id := m.picker.confirmDelete; id != "" {
		m.picker.confirmDelete = ""
		if msg.String() == "y" || msg.String() == "Y" {
			return m, m.deletePickerConversation(id)
		}
		m.picker.notice, m.picker.noticeErr = "not deleted", false
		return m, nil
	}
	// An outcome stays on screen until the user does something else — unless
	// a request is still running, in which case the line is its progress.
	if m.picker.acting == "" {
		m.picker.notice, m.picker.noticeErr = "", false
	}
	switch msg.String() {
	case "esc":
		m.picker = pickerState{}
		return m, nil
	case "up", "ctrl+p":
		if m.picker.cursor > 0 {
			m.picker.cursor--
		}
		return m, m.maybeLoadPreview()
	case "down", "ctrl+n":
		if m.picker.cursor < len(m.picker.filtered)-1 {
			m.picker.cursor++
		}
		return m, m.maybeLoadPreview()
	case "pgup":
		m.picker.cursor = max(m.picker.cursor-m.pickerRows(), 0)
		return m, m.maybeLoadPreview()
	case "pgdown":
		m.picker.cursor = min(m.picker.cursor+m.pickerRows(), max(len(m.picker.filtered)-1, 0))
		return m, m.maybeLoadPreview()
	case "enter":
		if m.picker.loading {
			return m, nil
		}
		c, ok := m.picker.selected()
		if !ok {
			return m, nil
		}
		m.picker.loading = true
		m.picker.err = ""
		return m, tea.Batch(m.loadPickerTranscript(c.Name), m.sp.Tick)
	case "tab":
		m.picker.focus = (m.picker.focus + 1) % pickerFocusCount
		return m, nil
	case "ctrl+o":
		m.picker.comfortable = !m.picker.comfortable
		return m, nil
	case "ctrl+t":
		m.picker.transcript = !m.picker.transcript
		return m, m.maybeLoadPreview()
	case "ctrl+a":
		c, ok := m.pickerActionTarget()
		if !ok {
			return m, nil
		}
		return m, m.archivePickerConversation(c, !m.picker.archived)
	case "ctrl+d":
		c, ok := m.pickerActionTarget()
		if !ok {
			return m, nil
		}
		m.picker.confirmDelete = c.Name
		return m, nil
	case "left", "right":
		switch m.picker.focus {
		case pickerFocusSort:
			m.picker.sortCreated = !m.picker.sortCreated
			m.picker.refilter()
			return m, m.maybeLoadPreview()
		case pickerFocusShow:
			m.picker.archived = !m.picker.archived
			return m, m.reloadPickerList()
		}
	}
	if m.picker.direct || (msg.Text == "" && !isEditingKey(msg.String())) {
		return m, nil
	}
	before := m.picker.search.Value()
	var cmd tea.Cmd
	m.picker.search, cmd = m.picker.search.Update(msg)
	if m.picker.search.Value() != before {
		m.picker.refilter()
		cmd = tea.Batch(cmd, m.maybeLoadPreview())
	}
	return m, cmd
}

// pickerActionTarget is the conversation ctrl+a/ctrl+d act on: the highlighted
// row — unless there is none, the listing is still loading, this is a direct
// `resume <id>` load with no list behind it, or another archive/delete is
// still in flight.
func (m *chatModel) pickerActionTarget() (assistantv1alpha1.Conversation, bool) {
	if m.picker.direct || m.picker.loading || m.picker.acting != "" {
		return assistantv1alpha1.Conversation{}, false
	}
	return m.picker.selected()
}

// pickerNoticeTitleWidth caps how much of a conversation's title the notice
// line quotes, leaving room on one line for the verb and the outcome.
const pickerNoticeTitleWidth = 48

// archivePickerConversation archives (archived=true) or unarchives c in the
// background. Same off-goroutine rule as loadPickerList: everything the
// request needs is captured by value here, and only the returned
// pickerArchiveMsg — handled in Update — touches the model.
func (m *chatModel) archivePickerConversation(c assistantv1alpha1.Conversation, archived bool) tea.Cmd {
	title := previewLine(conversationTitle(c), pickerNoticeTitleWidth)
	verb := "archiving "
	if !archived {
		verb = "unarchiving "
	}
	m.picker.acting = c.Name
	m.picker.notice, m.picker.noticeErr = verb+title+"…", false
	ctx, view, project, id := m.ctx, m.view, m.project, c.Name
	return func() tea.Msg {
		_, err := view.setConversationArchived(ctx, project, id, archived)
		if err != nil {
			err = errors.New(readViewErrorText(view, err))
		}
		return pickerArchiveMsg{contextID: id, title: title, archived: archived, err: err}
	}
}

// deletePickerConversation deletes the conversation with the given id in the
// background, once its y/n has been answered. Same capture-by-value rule as
// archivePickerConversation.
func (m *chatModel) deletePickerConversation(contextID string) tea.Cmd {
	title := m.picker.titleOf(contextID)
	m.picker.acting = contextID
	m.picker.notice, m.picker.noticeErr = "deleting "+title+"…", false
	ctx, view, project := m.ctx, m.view, m.project
	return func() tea.Msg {
		err := view.deleteConversation(ctx, project, contextID)
		if err != nil {
			err = errors.New(readViewErrorText(view, err))
		}
		return pickerDeleteMsg{contextID: contextID, title: title, err: err}
	}
}

// onPickerArchived folds an archive/unarchive result into the picker. On
// success the row leaves this listing locally — it now belongs to the other
// Show option — rather than refetching a list whose only change is known.
func (m *chatModel) onPickerArchived(msg pickerArchiveMsg) tea.Cmd {
	if !m.picker.open || m.picker.acting != msg.contextID {
		// The picker was closed (or reopened) while the request ran; there is
		// no notice line left to report on.
		return nil
	}
	m.picker.acting = ""
	if msg.err != nil {
		verb := "archive "
		if !msg.archived {
			verb = "unarchive "
		}
		m.picker.notice, m.picker.noticeErr = "could not "+verb+msg.title+": "+msg.err.Error(), true
		return nil
	}
	m.picker.removeItem(msg.contextID)
	if msg.archived {
		m.picker.notice = "archived " + msg.title + " — tab to Show, → Archived to find it"
	} else {
		m.picker.notice = "unarchived " + msg.title + " — it is back with your active conversations"
	}
	m.picker.noticeErr = false
	return m.maybeLoadPreview()
}

// onPickerDeleted folds a delete result into the picker, and into the chat
// underneath when that is the conversation that was deleted: left alone, the
// chat would keep the old context id, and its next turn would quietly start a
// new conversation under that id with none of the history still on screen.
func (m *chatModel) onPickerDeleted(msg pickerDeleteMsg) tea.Cmd {
	if msg.err == nil && msg.contextID != "" && msg.contextID == m.contextID {
		m.contextID = ""
		m.convName = ""
		m.turns = nil
		m.raw = nil
		m.mentions.recent = nil
		m.answer.Reset()
		m.activity, m.turnActivity = nil, nil
		m.follow = true
		m.turns = append(m.turns, m.st.subtle.Render("this conversation was deleted — your next message starts a new one"))
		m.rebuildViewport()
	}
	if !m.picker.open || m.picker.acting != msg.contextID {
		return nil
	}
	m.picker.acting = ""
	if msg.err != nil {
		m.picker.notice, m.picker.noticeErr = "could not delete "+msg.title+": "+msg.err.Error(), true
		return nil
	}
	m.picker.removeItem(msg.contextID)
	m.picker.notice, m.picker.noticeErr = "deleted "+msg.title, false
	return m.maybeLoadPreview()
}

// removeItem drops one conversation from the listing and its preview caches,
// keeping the cursor where it was (clamped to the rows left) so the user's
// place in a long list survives an archive or delete.
func (p *pickerState) removeItem(contextID string) {
	i := slices.IndexFunc(p.items, func(c assistantv1alpha1.Conversation) bool { return c.Name == contextID })
	if i < 0 {
		return
	}
	p.items = slices.Delete(p.items, i, i+1)
	delete(p.preview, contextID)
	delete(p.previewErr, contextID)
	delete(p.previewPending, contextID)
	cursor := p.cursor
	p.refilter()
	p.cursor = min(cursor, max(len(p.filtered)-1, 0))
}

// titleOf is the listed conversation's label for the notice line, falling back
// to its id when it is not in the listing.
func (p *pickerState) titleOf(contextID string) string {
	for _, c := range p.items {
		if c.Name == contextID {
			return previewLine(conversationTitle(c), pickerNoticeTitleWidth)
		}
	}
	return contextID
}

// reloadPickerList refetches the listing for the current Show option,
// emptying the rows first so the previous listing is never shown under the
// other option's label.
func (m *chatModel) reloadPickerList() tea.Cmd {
	m.picker.items, m.picker.filtered, m.picker.cursor = nil, nil, 0
	m.picker.loading, m.picker.err = true, ""
	return tea.Batch(m.loadPickerList(), m.sp.Tick)
}

// loadPickerList fetches the project's conversation listing in the background
// (the same read path as `patch conversations list`), newest activity first —
// the everyday listing, or the archive when the Show option says so.
// It captures ctx/view/project/archived by value at call time rather than
// reading m inside the closure — like stream(), this runs off the Update
// goroutine, so it must never touch mutable model state directly; only the
// returned Msg, handled in Update, may.
func (m *chatModel) loadPickerList() tea.Cmd {
	ctx, view, project, archived := m.ctx, m.view, m.project, m.picker.archived
	return func() tea.Msg {
		out, err := view.get(ctx, project, conversationsListPath(project, archived))
		if err != nil {
			return pickerListMsg{archived: archived, err: errors.New(readViewErrorText(view, err))}
		}
		var list assistantv1alpha1.ConversationList
		if err := json.Unmarshal(out, &list); err != nil {
			return pickerListMsg{archived: archived, err: err}
		}
		sort.SliceStable(list.Items, func(i, j int) bool {
			return list.Items[i].Status.LastActiveAt.After(list.Items[j].Status.LastActiveAt.Time)
		})
		return pickerListMsg{archived: archived, items: list.Items}
	}
}

// fetchTranscript fetches one conversation's full transcript (the messages
// subresource). Shared by loadPickerTranscript (resume) and loadPickerPreview
// (the picker's preview pane) — same data, different destination Msg.
func fetchTranscript(ctx context.Context, view ReadView, project, contextID string) ([]assistantv1alpha1.ConversationMessage, error) {
	out, err := view.get(ctx, project, messagesPath(project, contextID))
	if err != nil {
		return nil, errors.New(readViewErrorText(view, err))
	}
	var msgs assistantv1alpha1.ConversationMessages
	if err := json.Unmarshal(out, &msgs); err != nil {
		return nil, err
	}
	return msgs.Items, nil
}

// loadPickerTranscript fetches one conversation's full transcript in the
// background, for Update to fold into m.turns on arrival (resuming it as the
// active chat). Same off-goroutine caveat as loadPickerList.
func (m *chatModel) loadPickerTranscript(contextID string) tea.Cmd {
	ctx, view, project := m.ctx, m.view, m.project
	return func() tea.Msg {
		items, err := fetchTranscript(ctx, view, project, contextID)
		if err != nil {
			return pickerTranscriptMsg{err: err}
		}
		return pickerTranscriptMsg{contextID: contextID, items: items}
	}
}

// loadPickerPreview fetches one conversation's transcript in the background
// for the preview pane — the result is cached in m.picker.preview, never
// resumed into the main chat. Same off-goroutine caveat as loadPickerList.
func (m *chatModel) loadPickerPreview(contextID string) tea.Cmd {
	ctx, view, project := m.ctx, m.view, m.project
	return func() tea.Msg {
		items, err := fetchTranscript(ctx, view, project, contextID)
		if err != nil {
			return pickerPreviewMsg{contextID: contextID, err: err}
		}
		return pickerPreviewMsg{contextID: contextID, items: items}
	}
}

// maybeLoadPreview kicks off a background fetch for the currently
// highlighted conversation's preview, unless the transcript pane is hidden,
// it is already cached, or it is already in flight. Called whenever the
// cursor moves and when the pane is toggled on.
func (m *chatModel) maybeLoadPreview() tea.Cmd {
	if !m.picker.transcript {
		return nil
	}
	c, ok := m.picker.selected()
	if !ok {
		return nil
	}
	id := c.Name
	if _, ok := m.picker.preview[id]; ok {
		return nil
	}
	if _, ok := m.picker.previewErr[id]; ok {
		return nil
	}
	if m.picker.previewPending[id] {
		return nil
	}
	m.picker.previewPending[id] = true
	return m.loadPickerPreview(id)
}

// pickerFooterRows is the vertical space the picker's footer takes: the rule
// line plus two lines of key hints.
const pickerFooterRows = 3

// pickerRows is how many conversation rows fit between the search line and
// the footer, given the row height and whether the transcript pane is open.
func (m *chatModel) pickerRows() int {
	// outer box padding (2) + header (1) + blank (1) + search line (1) +
	// blank (1) + footer.
	avail := m.termHeight - 6 - pickerFooterRows - m.pickerNoticeRows()
	if m.picker.transcript {
		avail -= pickerPreviewMaxLines + 3 // pane header, blank, messages, ⋮
	}
	per := 1
	if m.picker.comfortable {
		per = 2
	}
	return max(avail/per, 1)
}

// pickerView renders the picker as a full-screen overlay: header, the search
// line with the sort option right-aligned on it, the rows (or a spinner, an
// error, or an empty-state line), the optional transcript pane, and — pinned
// to the bottom — a rule carrying the cursor position over the key hints.
func (m *chatModel) pickerView() tea.View {
	cw := m.contentWidth()
	var b strings.Builder
	b.WriteString(m.st.header.Render("Resume a previous conversation") + m.st.subtle.Render("  ·  project "+m.project))
	if m.picker.archived {
		b.WriteString(m.st.subtle.Render("  ·  archived"))
	}
	b.WriteString("\n\n")

	if !m.picker.direct {
		b.WriteString(m.searchLine(cw) + "\n\n")
	}
	switch {
	case m.picker.loading && m.picker.direct:
		b.WriteString(m.sp.View() + " " + m.st.subtle.Render("loading conversation…"))
	case m.picker.loading && len(m.picker.items) == 0:
		b.WriteString(m.sp.View() + " " + m.st.subtle.Render("loading…"))
	case m.picker.err != "":
		b.WriteString(m.st.err.Render("⚠ " + m.picker.err))
	case len(m.picker.items) == 0 && m.picker.archived:
		b.WriteString(m.st.subtle.Render("No archived conversations in this project."))
	case len(m.picker.items) == 0:
		b.WriteString(m.st.subtle.Render("No conversations found in this project."))
	case len(m.picker.filtered) == 0:
		b.WriteString(m.st.subtle.Render(fmt.Sprintf("No conversations match %q.", m.picker.search.Value())))
	default:
		b.WriteString(m.pickerRowsView(cw))
		if m.picker.transcript {
			b.WriteString("\n\n" + m.previewPane(cw))
		}
	}
	top := strings.TrimRight(b.String(), "\n")

	// Pin the footer to the bottom: pad the top section to fill the rows the
	// footer and the box padding leave.
	fill := m.termHeight - 2 - pickerFooterRows - m.pickerNoticeRows() - strings.Count(top, "\n") - 1
	if fill < 1 {
		fill = 1
	}
	return m.newView(m.st.box.Render(top + strings.Repeat("\n", fill) + m.pickerFooter(cw)))
}

// searchLine is the search box (borderless, placeholder "Type to search")
// with the Sort and Show option groups right-aligned on the same line.
func (m *chatModel) searchLine(width int) string {
	opt := func(name string, on bool) string {
		if on {
			return m.st.userText.Render("[" + name + "]")
		}
		return m.st.subtle.Render(name)
	}
	label := func(name string, f pickerFocus) string {
		if m.picker.focus == f {
			return m.st.you.Render(name + ": ")
		}
		return m.st.subtle.Render(name + ": ")
	}
	sortBar := label("Sort", pickerFocusSort) + opt("Updated", !m.picker.sortCreated) + " " + opt("Created", m.picker.sortCreated)
	showBar := label("Show", pickerFocusShow) + opt("Active", !m.picker.archived) + " " + opt("Archived", m.picker.archived)
	bar := sortBar + "   " + showBar
	barW := lipgloss.Width(bar)
	searchW := max(width-barW-2, 10)
	m.picker.search.SetWidth(searchW)
	search := lipgloss.NewStyle().Width(searchW).MaxWidth(searchW).Render(m.picker.search.View())
	return search + strings.Repeat(" ", max(width-searchW-barW, 1)) + bar
}

// pickerAgeWidth is the age column's width: the "❯ " gutter, the widest
// age ("no activity", or a 2006-01-02 date), and two spaces of gap.
const pickerAgeWidth = 15

// pickerRowsView renders the matching rows, windowed to pickerRows() around
// the cursor. A row is the age column then the title, on alternating
// backgrounds, the cursor row on its own; comfortable view adds a detail
// line (size, id) under each.
func (m *chatModel) pickerRowsView(width int) string {
	var b strings.Builder
	n := len(m.picker.filtered)
	rows := m.pickerRows()
	start := 0
	if n > rows {
		start = max(m.picker.cursor-rows/2, 0)
		start = min(start, n-rows)
	}
	end := min(start+rows, n)
	titleW := max(width-pickerAgeWidth, 10)
	for i := start; i < end; i++ {
		c := m.picker.items[m.picker.filtered[i]]
		selected := i == m.picker.cursor
		bg := lipgloss.NewStyle()
		if selected {
			bg = m.st.rowSel
		} else if i%2 == 0 {
			bg = m.st.rowAlt
		}
		gutter, age := "  ", m.st.subtle
		title := m.st.userText
		if selected {
			gutter, age, title = m.st.you.Render("❯ "), m.st.you, m.st.you.Bold(true)
		}
		line := gutter + age.Render(fmt.Sprintf("%-*s", pickerAgeWidth-2, rowAge(c, m.picker.sortCreated))) +
			title.Render(previewLine(conversationTitle(c), titleW))
		b.WriteString(bg.Width(width).Render(line) + "\n")
		if m.picker.comfortable {
			detail := previewLine(messageCount(c)+" · "+c.Name, titleW)
			b.WriteString(bg.Width(width).Render(strings.Repeat(" ", pickerAgeWidth)+m.st.subtle.Render(detail)) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// pickerFooter is the rule line carrying "cursor / matches" at its right
// end, over two lines of key hints — preceded, when there is one, by the
// notice line (a pending delete's question, or an archive/delete outcome). The
// direct-load variant has only the way out.
func (m *chatModel) pickerFooter(width int) string {
	if m.picker.direct {
		return m.st.subtle.Render(strings.Repeat("─", width)) + "\n" +
			m.st.hint.Render("esc to start a fresh conversation instead") + "\n"
	}
	pos := ""
	if n := len(m.picker.filtered); n > 0 {
		pos = fmt.Sprintf(" %d / %d ", m.picker.cursor+1, n)
	}
	rule := strings.Repeat("─", max(width-lipgloss.Width(pos)-1, 0)) + pos + "─"
	hint := func(key, what string) string { return m.st.userText.Render(key) + " " + m.st.hint.Render(what) }
	sep := "    "
	archive := "archive"
	if m.picker.archived {
		archive = "unarchive"
	}
	line1 := strings.Join([]string{
		hint("enter", "resume"), hint("ctrl+a", archive), hint("ctrl+d", "delete"), hint("esc", "exit"),
		hint("tab", "focus options"), hint("←/→", "change option"),
	}, sep)
	line2 := strings.Join([]string{
		hint("ctrl+o", "comfortable view"), hint("ctrl+t", "transcript"), hint("↑/↓", "browse"), hint("type", "to search"),
		hint("ctrl+c", "exit"),
	}, sep)
	footer := m.st.subtle.Render(rule) + "\n" + line1 + "\n" + line2
	if notice := m.pickerNotice(width); notice != "" {
		footer = notice + "\n" + footer
	}
	return footer
}

// pickerNoticeRows is the height the notice line takes: one row while there
// is something to say, none otherwise, so the list keeps its space.
func (m *chatModel) pickerNoticeRows() int {
	if m.picker.direct || (m.picker.confirmDelete == "" && m.picker.notice == "") {
		return 0
	}
	return 1
}

// pickerNotice renders the notice line. A pending delete's question is drawn
// in the error style, because what it asks about is irreversible; progress
// and success are subtle, and a failure carries the error style and the
// reason.
func (m *chatModel) pickerNotice(width int) string {
	switch {
	case m.picker.direct:
		return ""
	case m.picker.confirmDelete != "":
		id := m.picker.confirmDelete
		question := previewLine("delete "+m.picker.titleOf(id)+" ("+id+")? this cannot be undone", max(width-30, 20))
		return m.st.err.Render(question) + "   " + m.st.userText.Render("y") + " " + m.st.hint.Render("delete") +
			"  " + m.st.hint.Render("any other key cancels")
	case m.picker.notice == "":
		return ""
	case m.picker.noticeErr:
		return m.st.err.Render(previewLine("⚠ "+m.picker.notice, width))
	default:
		return m.st.subtle.Render(previewLine(m.picker.notice, width))
	}
}

// conversationTitle is the row's headline: the name the user gave this
// conversation, else the opening message the apiserver reports, else the
// context id. A user-chosen name wins because it is the label they picked for
// exactly this purpose; a conversation reduced to a compaction summary has no
// opening message, and an apiserver predating status.title/status.name reports
// neither, so the id is the one label always there to show.
func conversationTitle(c assistantv1alpha1.Conversation) string {
	if n := strings.TrimSpace(c.Status.Name); n != "" {
		return n
	}
	if t := strings.TrimSpace(c.Status.Title); t != "" {
		return t
	}
	return c.Name
}

// pickerName is the user-given name of the listed conversation with the given
// context id, or "" when it has none (or is not in the listing at all) — how
// the chat picks the name up when a picked conversation is resumed.
func pickerName(items []assistantv1alpha1.Conversation, contextID string) string {
	for _, c := range items {
		if c.Name == contextID {
			return strings.TrimSpace(c.Status.Name)
		}
	}
	return ""
}

// rowAge words the row's age column: "3h ago" for recent activity, the bare
// date once ago() falls back to one, and a placeholder when the listing
// carried no timestamp at all. Under the Created sort it reads the creation
// time instead, so the column matches the order.
func rowAge(c assistantv1alpha1.Conversation, created bool) string {
	t := c.Status.LastActiveAt.Time
	if created {
		t = c.CreationTimestamp.Time
	}
	if t.IsZero() {
		return "no activity"
	}
	a := ago(t)
	if strings.Contains(a, "-") {
		return a
	}
	return a + " ago"
}

// messageCount pluralizes the row's size.
func messageCount(c assistantv1alpha1.Conversation) string {
	if c.Status.MessageCount == 1 {
		return "1 message"
	}
	return fmt.Sprintf("%d messages", c.Status.MessageCount)
}

// pickerPreviewMaxLines caps how many messages the preview pane shows: the
// conversation's opening message (what it was about) plus its most recent
// exchange (where it left off) — the two most useful signals for "do I want
// to resume this one," without needing the full transcript.
const pickerPreviewMaxLines = 6

// previewPane renders the highlighted conversation's transcript preview in a
// column width wide — answering "what's in this conversation" directly in
// the picker instead of forcing a resume-and-look. Truncated to
// pickerPreviewMaxLines messages: the opening message plus the most recent
// tail.
func (m *chatModel) previewPane(width int) string {
	var b strings.Builder
	b.WriteString(m.st.header.Render("preview"))
	c, ok := m.picker.selected()
	if !ok {
		return b.String()
	}
	id := c.Name
	b.WriteString(m.st.subtle.Render("  ·  " + id))
	b.WriteString("\n\n")
	lineW := max(width-10, 20) // minus the widest label ("Summary: ")
	switch {
	case m.picker.previewErr[id] != "":
		b.WriteString(m.st.err.Render("⚠ " + m.picker.previewErr[id]))
	// preview[id] == nil while pending means the fetch (kicked off by
	// maybeLoadPreview, which marks pending before returning its Cmd) is
	// still in flight; once it lands, either preview[id] or previewErr[id]
	// above is populated and pending is cleared, so this is never confused
	// with a legitimately empty conversation.
	case m.picker.preview[id] == nil && m.picker.previewPending[id]:
		b.WriteString(m.st.subtle.Render("loading preview…"))
	default:
		items := m.picker.preview[id]
		if len(items) == 0 {
			b.WriteString(m.st.subtle.Render("(no messages)"))
			break
		}
		shown := items
		if len(shown) > pickerPreviewMaxLines {
			tail := append([]assistantv1alpha1.ConversationMessage{}, items[0])
			tail = append(tail, items[len(items)-(pickerPreviewMaxLines-1):]...)
			shown = tail
		}
		for i, mm := range shown {
			label, style := m.st.you.Render("You"), m.st.userText
			switch mm.Role {
			case "assistant":
				label, style = m.st.patch.Render("Patch"), m.st.subtle
			case "summary":
				label, style = m.st.subtle.Render("Summary"), m.st.subtle
			}
			b.WriteString(label + ": " + style.Render(previewLine(mm.Content, lineW)) + "\n")
			if i == 0 && len(items) > pickerPreviewMaxLines {
				b.WriteString(m.st.subtle.Render("⋮") + "\n")
			}
		}
	}
	return lipgloss.NewStyle().Width(width).Render(b.String())
}

// previewLine collapses a message to one line and truncates it to maxRunes,
// so a multi-paragraph message doesn't blow out the preview pane's height.
func previewLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes < 1 {
		return "…"
	}
	return string(r[:maxRunes-1]) + "…"
}
