// Opt-in full-screen chat UI for the `patch` CLI, built on Bubble Tea v2. It is
// an alternative to the line-based REPL (runRepl) selected with `chat --tui`;
// the plain and interactive modes are untouched.
//
// Layout: a header, a scrollable transcript viewport, a status line (spinner
// while the assistant works, else a hint), and a text input at the bottom, all
// wrapped in a padded container. Assistant answers stream in live and are
// rendered as markdown via glamour, so bold/bullets/tables come out formatted
// rather than as raw `**`.
//
// Colors & contrast: Bubble Tea (not glamour) owns the OSC-11 terminal
// background query — glamour querying the terminal itself would leak the
// response into raw-mode input. We ask for the background in Init, learn
// dark/light from the reply, then build both the glamour style and the lipgloss
// speaker colors explicitly for that background.
//
// Threading: the conversation's contextId is learned from the event stream (as
// the REPL does) and sent on every later turn, so the whole session is one
// conversation with memory.
package patchcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	spin "charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	glamourstyles "github.com/charmbracelet/glamour/styles"

	"github.com/a2aproject/a2a-go/v2/a2a"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// Streaming messages delivered to the model via p.Send from the turn goroutine.
type (
	// streamChunkMsg is one partial answer chunk (artifact/message text).
	streamChunkMsg struct{ text string }
	// streamDoneMsg ends a turn; contextID threads the conversation forward.
	streamDoneMsg struct {
		contextID string
		failed    bool
		failMsg   string
	}
	// streamErrMsg is a transport/stream error surfaced by the a2a client.
	streamErrMsg struct{ err error }
	// compactDoneMsg ends a /compact request (POST /v1/compact, outside the
	// a2a client): err is nil on success, [ErrNothingToCompact] when the
	// server had nothing to fold, or any other error on a real failure.
	compactDoneMsg struct{ err error }
	// pickerListMsg delivers the picker's conversation listing (or an error).
	pickerListMsg struct {
		items []assistantv1alpha1.Conversation
		err   error
	}
	// pickerTranscriptMsg delivers the selected conversation's full transcript
	// (or an error) to resume from.
	pickerTranscriptMsg struct {
		contextID string
		items     []assistantv1alpha1.ConversationMessage
		err       error
	}
	// pickerPreviewMsg delivers one conversation's transcript (or an error) for
	// the picker's preview pane — the same fetch as pickerTranscriptMsg, but
	// cached by contextID instead of resuming into the main chat.
	pickerPreviewMsg struct {
		contextID string
		items     []assistantv1alpha1.ConversationMessage
		err       error
	}
)

// transcriptTurn is one unstyled turn, parallel to chatModel.turns (which
// holds already-rendered, ANSI-styled blocks for the viewport). /export walks
// this slice instead, so the exported file is plain text.
type transcriptTurn struct {
	role    string // "user", "assistant", or "system" (a stream/transport error)
	content string
}

// pickerState is the chat TUI's conversation picker: a `/resume` overlay that
// lists the project's conversations (via the conversations apiserver, same
// kubectl path as `patch conversations list`) and loads the selected one's
// transcript to resume without leaving the TUI.
type pickerState struct {
	open    bool
	loading bool
	err     string
	items   []assistantv1alpha1.Conversation
	cursor  int

	// preview/previewErr/previewPending back the preview pane: the highlighted
	// conversation's transcript is fetched lazily as the cursor moves and
	// cached by contextID (conversation Name) so revisiting an item never
	// re-fetches. previewPending guards against firing a second fetch for the
	// same id while one is already in flight.
	preview        map[string][]assistantv1alpha1.ConversationMessage
	previewErr     map[string]string
	previewPending map[string]bool
}

// newPickerState returns an open, loading picker with its preview caches
// ready to write to (a zero pickerState's maps are nil).
func newPickerState() pickerState {
	return pickerState{
		open:           true,
		loading:        true,
		preview:        map[string][]assistantv1alpha1.ConversationMessage{},
		previewErr:     map[string]string{},
		previewPending: map[string]bool{},
	}
}

// styles bundles the lipgloss styles that depend on the terminal background, so
// they can be rebuilt as one unit when the background is learned.
type styles struct {
	header   lipgloss.Style // app title
	you      lipgloss.Style // "You" speaker label
	patch    lipgloss.Style // "Patch" speaker label + spinner
	userText lipgloss.Style // the user's own message body text
	hint     lipgloss.Style // status hint line
	subtle   lipgloss.Style // subtitle / placeholder decoration
	err      lipgloss.Style // error text
	box      lipgloss.Style // outer padded container
	inputBox lipgloss.Style // bordered container around the text input
}

// newStyles builds a palette readable on the given background. Colors are
// explicit truecolor hex (NOT ANSI palette indices, which map to washed-out
// shades under custom terminal themes), chosen per-background via
// lipgloss.LightDark — first arg is shown on a LIGHT bg (so a dark color),
// second on a DARK bg (a light color). Header/labels are bold; nothing is Faint.
func newStyles(dark bool) styles {
	ld := lipgloss.LightDark(dark)
	you := ld(lipgloss.Color("#1155CC"), lipgloss.Color("#7AB7FF"))    // blue
	violet := ld(lipgloss.Color("#6D28D9"), lipgloss.Color("#C084FC")) // deep/light violet
	gray := ld(lipgloss.Color("#5C574A"), lipgloss.Color("#A8A29E"))   // warm readable gray
	red := ld(lipgloss.Color("#C0182B"), lipgloss.Color("#FF6B6B"))
	border := ld(lipgloss.Color("#C9BFA0"), lipgloss.Color("#4A453B")) // subtle line on cream/dark
	body := ld(lipgloss.Color("#2A2620"), lipgloss.Color("#EAE6DA"))   // = input text color

	return styles{
		header:   lipgloss.NewStyle().Bold(true).Foreground(violet),
		you:      lipgloss.NewStyle().Bold(true).Foreground(you),
		patch:    lipgloss.NewStyle().Bold(true).Foreground(violet),
		userText: lipgloss.NewStyle().Foreground(body),
		hint:     lipgloss.NewStyle().Foreground(gray),
		subtle:   lipgloss.NewStyle().Foreground(gray),
		err:      lipgloss.NewStyle().Bold(true).Foreground(red),
		box:      lipgloss.NewStyle().Padding(1, 2),
		inputBox: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1),
	}
}

// newInputStyles styles the text input itself. The bubbles defaults render text
// and placeholder faint (nearly invisible on a cream bg), so override the
// foreground colors explicitly per background for text, placeholder, prompt,
// and cursor.
func newInputStyles(dark bool) textinput.Styles {
	ld := lipgloss.LightDark(dark)
	text := ld(lipgloss.Color("#2A2620"), lipgloss.Color("#EAE6DA"))        // near-black on light
	placeholder := ld(lipgloss.Color("#8B8577"), lipgloss.Color("#6B6B6B")) // subdued but visible
	prompt := ld(lipgloss.Color("#1155CC"), lipgloss.Color("#7AB7FF"))      // = you blue
	cursor := ld(lipgloss.Color("#6D28D9"), lipgloss.Color("#C084FC"))      // = patch violet

	s := textinput.DefaultStyles(dark)
	for _, st := range []*textinput.StyleState{&s.Focused, &s.Blurred} {
		st.Text = st.Text.Foreground(text)
		st.Placeholder = st.Placeholder.Foreground(placeholder)
		st.Prompt = st.Prompt.Foreground(prompt)
	}
	s.Cursor.Color = cursor
	return s
}

// chatModel is the Bubble Tea v2 model for the full-screen chat. Methods use a
// pointer receiver so the streaming goroutine (which holds *tea.Program) and
// Update mutate one shared model.
type chatModel struct {
	ctx        context.Context
	prog       *tea.Program
	client     *serviceClient
	project    string
	kubeconfig string // for the /resume picker's kubectl calls; "" uses normal resolution
	baseURL    string // for /compact's POST /v1/compact call (outside the a2a client)
	token      TokenSource

	vp       viewport.Model
	ti       textinput.Model
	sp       spin.Model
	renderer *glamour.TermRenderer
	st       styles

	contextID string
	turns     []string         // finalized, already-rendered conversation blocks
	raw       []transcriptTurn // parallel to turns, unstyled — source for /export
	answer    strings.Builder  // in-progress assistant answer (raw markdown)
	working   bool
	dark      bool

	// overlay is "" (normal chat), "help", or "status" — a read-only reference
	// screen shown full-screen and dismissed by any keypress (see onKey).
	overlay string

	picker pickerState

	// suggestIndex is the highlighted entry in the slash-command suggestion
	// list (see currentSuggestions). Reset to 0 whenever the input text
	// changes so a fresh prefix always starts at the top match.
	suggestIndex int

	termHeight int // last WindowSizeMsg height, so View can resize vp around a variable-height suggestion block

	firstMessage string // sent automatically once, on first layout
	sentFirst    bool
	ready        bool
	width        int // full terminal width; content width subtracts padding
}

func runChatTUI(ctx context.Context, client *serviceClient, project, contextID, firstMessage, kubeconfig, baseURL string, token TokenSource) int {
	st := newStyles(false) // provisional (light) until the background is learned

	sp := spin.New(spin.WithSpinner(spin.Dot), spin.WithStyle(st.patch))

	ti := textinput.New()
	ti.Placeholder = "Message patch…"
	ti.SetVirtualCursor(true)
	ti.SetStyles(newInputStyles(false)) // provisional (light) until bg is learned

	m := &chatModel{
		ctx:          ctx,
		client:       client,
		project:      project,
		kubeconfig:   kubeconfig,
		baseURL:      baseURL,
		token:        token,
		contextID:    contextID,
		vp:           viewport.New(),
		ti:           ti,
		sp:           sp,
		st:           st,
		firstMessage: firstMessage,
	}

	p := tea.NewProgram(m, tea.WithContext(ctx))
	m.prog = p

	if _, err := p.Run(); err != nil {
		if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "patch: "+err.Error())
		return 1
	}
	return 0
}

func (m *chatModel) Init() tea.Cmd {
	// RequestBackgroundColor makes Bubble Tea own the OSC-11 query and consume
	// the terminal's reply, so it never leaks into the text input. It's a
	// Msg-returning func, so wrap it as a Cmd.
	bgColor := func() tea.Msg { return tea.RequestBackgroundColor() }
	return tea.Batch(m.ti.Focus(), textinput.Blink, bgColor)
}

func (m *chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m, m.layout(msg.Width, msg.Height)

	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.st = newStyles(m.dark)
		m.ti.SetStyles(newInputStyles(m.dark))
		if !m.working {
			m.sp = spin.New(spin.WithSpinner(spin.Dot), spin.WithStyle(m.st.patch))
		}
		if m.width > 0 {
			m.renderer = newMarkdownRenderer(m.contentWidth(), m.dark)
		}
		m.rebuildViewport()
		return m, nil

	case tea.KeyPressMsg:
		return m.onKey(msg)

	case spin.TickMsg:
		if !m.working && !m.picker.loading {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case streamChunkMsg:
		m.answer.WriteString(msg.text)
		m.rebuildViewport()
		return m, nil

	case streamDoneMsg:
		if msg.contextID != "" {
			m.contextID = msg.contextID
		}
		answer := m.renderMarkdown(m.answer.String())
		if msg.failed {
			note := msg.failMsg
			if note == "" {
				note = "the task failed"
			}
			answer = strings.TrimRight(answer, "\n") + "\n" + m.st.err.Render("⚠ "+note)
		}
		m.turns = append(m.turns, m.turnBlock(m.st.patch.Render("Patch"), answer))
		rawAnswer := m.answer.String()
		if msg.failed {
			note := msg.failMsg
			if note == "" {
				note = "the task failed"
			}
			rawAnswer = strings.TrimRight(rawAnswer, "\n") + "\n⚠ " + note
		}
		m.raw = append(m.raw, transcriptTurn{role: "assistant", content: rawAnswer})
		m.answer.Reset()
		m.working = false
		m.rebuildViewport()
		return m, nil

	case streamErrMsg:
		errText := friendlyError(msg.err, m.client.errs)
		m.turns = append(m.turns, m.st.err.Render("patch: "+errText))
		m.raw = append(m.raw, transcriptTurn{role: "system", content: "error: " + errText})
		m.answer.Reset()
		m.working = false
		m.rebuildViewport()
		return m, nil

	case compactDoneMsg:
		m.working = false
		switch {
		case msg.err == nil:
			m.turns = append(m.turns, m.st.subtle.Render("history compacted"))
			m.raw = append(m.raw, transcriptTurn{role: "system", content: "history compacted"})
		case errors.Is(msg.err, ErrNothingToCompact):
			m.turns = append(m.turns, m.st.subtle.Render("nothing to compact"))
			m.raw = append(m.raw, transcriptTurn{role: "system", content: "nothing to compact"})
		default:
			m.turns = append(m.turns, m.st.err.Render("⚠ compact failed: "+msg.err.Error()))
			m.raw = append(m.raw, transcriptTurn{role: "system", content: "compact failed: " + msg.err.Error()})
		}
		m.rebuildViewport()
		return m, nil

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case pickerListMsg:
		m.picker.loading = false
		if msg.err != nil {
			m.picker.err = msg.err.Error()
			return m, nil
		}
		m.picker.items = msg.items
		m.picker.cursor = 0
		return m, m.maybeLoadPreview()

	case pickerPreviewMsg:
		delete(m.picker.previewPending, msg.contextID)
		if msg.err != nil {
			m.picker.previewErr[msg.contextID] = msg.err.Error()
			return m, nil
		}
		m.picker.preview[msg.contextID] = msg.items
		return m, nil

	case pickerTranscriptMsg:
		m.picker.loading = false
		if msg.err != nil {
			m.picker.err = msg.err.Error()
			return m, nil
		}
		m.contextID = msg.contextID
		m.turns = m.turns[:0]
		m.raw = m.raw[:0]
		m.answer.Reset()
		for _, mm := range msg.items {
			switch mm.Role {
			case "user":
				m.turns = append(m.turns, m.turnBlock(m.st.you.Render("You"),
					m.st.userText.Width(m.contentWidth()).Render(mm.Content)))
			case "summary":
				// A compaction digest, not something the assistant said —
				// rendered de-emphasized (m.st.subtle, the same token used
				// elsewhere for placeholder/loading text) so it reads as
				// synthetic history rather than a turn of the conversation.
				m.turns = append(m.turns, m.turnBlock(m.st.subtle.Render("Summary"),
					m.st.subtle.Width(m.contentWidth()).Render(mm.Content)))
			default:
				m.turns = append(m.turns, m.turnBlock(m.st.patch.Render("Patch"), m.renderMarkdown(mm.Content)))
			}
			m.raw = append(m.raw, transcriptTurn{role: mm.Role, content: mm.Content})
		}
		m.picker = pickerState{}
		m.rebuildViewport()
		return m, nil
	}
	return m, nil
}

func (m *chatModel) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Ctrl-C/D always quits, picker open or not — a universal escape hatch
	// regardless of what overlay is showing.
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m, tea.Quit
	}
	if m.picker.open {
		return m.onPickerKey(msg)
	}
	if m.overlay != "" {
		// Any key dismisses an overlay — it's a read-only reference screen,
		// not a mode with its own input.
		m.overlay = ""
		return m, nil
	}
	switch msg.String() {
	case "enter":
		if m.working {
			return m, nil
		}
		text := strings.TrimSpace(m.ti.Value())
		if text == "" {
			return m, nil
		}
		// A highlighted (or sole) suggestion resolves on Enter, same as
		// completing with Tab first then submitting — lets "/re" + Enter run
		// /resume directly instead of being sent as a literal message.
		if matches := m.currentSuggestions(); len(matches) > 0 {
			text = matches[m.suggestIndex%len(matches)]
		}
		m.suggestIndex = 0
		switch text {
		case "/quit", "/exit":
			return m, tea.Quit
		case "/resume":
			m.ti.Reset()
			m.picker = newPickerState()
			return m, tea.Batch(m.loadPickerList(), m.sp.Tick)
		case "/clear":
			m.ti.Reset()
			m.contextID = ""
			m.turns = nil
			m.raw = nil
			m.answer.Reset()
			m.rebuildViewport()
			return m, nil
		case "/compact":
			m.ti.Reset()
			if m.contextID == "" {
				m.turns = append(m.turns, m.st.subtle.Render("nothing to compact — no conversation yet"))
				m.rebuildViewport()
				return m, nil
			}
			m.working = true
			m.rebuildViewport()
			go m.compact()
			return m, m.sp.Tick
		case "/help":
			m.ti.Reset()
			m.overlay = "help"
			return m, nil
		case "/status":
			m.ti.Reset()
			m.overlay = "status"
			return m, nil
		case "/export":
			m.ti.Reset()
			path, err := m.exportTranscript()
			if err != nil {
				m.turns = append(m.turns, m.st.err.Render("⚠ export failed: "+err.Error()))
			} else {
				m.turns = append(m.turns, m.st.subtle.Render("exported to "+path))
			}
			m.rebuildViewport()
			return m, nil
		}
		m.ti.Reset()
		return m, m.submit(text)
	case "tab":
		// Complete to the highlighted suggestion without submitting — lets the
		// user see/edit before running, and is a no-op when there's nothing to
		// suggest (already exact, or not a "/" prefix at all).
		if matches := m.currentSuggestions(); len(matches) > 0 {
			m.ti.SetValue(matches[m.suggestIndex%len(matches)])
			m.ti.CursorEnd()
		}
		return m, nil
	case "up", "down":
		// Cycle the highlighted suggestion while one is showing; otherwise fall
		// through to the same (currently a no-op) forwarding default does for
		// these keys, so behavior outside "/" typing is unchanged.
		if matches := m.currentSuggestions(); len(matches) > 0 {
			if msg.String() == "up" {
				m.suggestIndex = (m.suggestIndex - 1 + len(matches)) % len(matches)
			} else {
				m.suggestIndex = (m.suggestIndex + 1) % len(matches)
			}
			return m, nil
		}
		if msg.Text == "" && !isEditingKey(msg.String()) {
			return m, nil
		}
		var cmd tea.Cmd
		m.ti, cmd = m.ti.Update(msg)
		return m, cmd
	case "pgup", "pgdown", "shift+up", "shift+down":
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	default:
		// Only forward genuine editing input to the text field. Printable keys
		// carry non-empty .Text; editing keys (backspace, arrows, …) don't but
		// are allowlisted. Everything else (stray/unparsed escape sequences) is
		// dropped so it can't be inserted as literal garbage.
		if msg.Text == "" && !isEditingKey(msg.String()) {
			return m, nil
		}
		var cmd tea.Cmd
		m.ti, cmd = m.ti.Update(msg)
		m.suggestIndex = 0 // a new prefix invalidates the old highlighted index
		return m, cmd
	}
}

// onPickerKey handles input while the /resume conversation picker is open. It
// swallows every key (nothing reaches the text input) so typing while
// browsing can't leak into the message field.
func (m *chatModel) onPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.picker = pickerState{}
		return m, nil
	case "up", "k":
		if m.picker.cursor > 0 {
			m.picker.cursor--
		}
		return m, m.maybeLoadPreview()
	case "down", "j":
		if m.picker.cursor < len(m.picker.items)-1 {
			m.picker.cursor++
		}
		return m, m.maybeLoadPreview()
	case "enter":
		if m.picker.loading || len(m.picker.items) == 0 {
			return m, nil
		}
		id := m.picker.items[m.picker.cursor].Name
		m.picker.loading = true
		m.picker.err = ""
		return m, tea.Batch(m.loadPickerTranscript(id), m.sp.Tick)
	}
	return m, nil
}

// isEditingKey reports whether a non-printable key (empty .Text) is still a
// legitimate line-editing key the text input should handle.
func isEditingKey(s string) bool {
	switch s {
	case "backspace", "delete", "left", "right", "home", "end",
		"ctrl+a", "ctrl+e", "ctrl+u", "ctrl+k", "ctrl+w",
		"ctrl+left", "ctrl+right", "alt+left", "alt+right",
		"alt+backspace", "alt+delete", "space":
		return true
	}
	return false
}

// loadPickerList fetches the project's conversation listing in the background
// (the same kubectl path as `patch conversations list`), newest activity
// first. It captures ctx/kubeconfig/project by value at call time rather than
// reading m inside the closure — like stream(), this runs off the Update
// goroutine, so it must never touch mutable model state directly; only the
// returned Msg, handled in Update, may.
func (m *chatModel) loadPickerList() tea.Cmd {
	ctx, kubeconfig, project := m.ctx, m.kubeconfig, m.project
	return func() tea.Msg {
		out, err := kubectlJSON(ctx, kubeconfig, "get", "conversations", "-n", project, "-o", "json")
		if err != nil {
			return pickerListMsg{err: errors.New(kubectlErrorText(err))}
		}
		var list assistantv1alpha1.ConversationList
		if err := json.Unmarshal(out, &list); err != nil {
			return pickerListMsg{err: err}
		}
		sort.SliceStable(list.Items, func(i, j int) bool {
			return list.Items[i].Status.LastActiveAt.After(list.Items[j].Status.LastActiveAt.Time)
		})
		return pickerListMsg{items: list.Items}
	}
}

// fetchTranscript fetches one conversation's full transcript (the messages
// subresource) over kubectl. Shared by loadPickerTranscript (resume) and
// loadPickerPreview (the picker's preview pane) — same data, different
// destination Msg.
func fetchTranscript(ctx context.Context, kubeconfig, project, contextID string) ([]assistantv1alpha1.ConversationMessage, error) {
	path := fmt.Sprintf(
		"/apis/assistant.miloapis.com/v1alpha1/namespaces/%s/conversations/%s/messages",
		project, contextID)
	out, err := kubectlJSON(ctx, kubeconfig, "get", "--raw", path)
	if err != nil {
		return nil, errors.New(kubectlErrorText(err))
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
	ctx, kubeconfig, project := m.ctx, m.kubeconfig, m.project
	return func() tea.Msg {
		items, err := fetchTranscript(ctx, kubeconfig, project, contextID)
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
	ctx, kubeconfig, project := m.ctx, m.kubeconfig, m.project
	return func() tea.Msg {
		items, err := fetchTranscript(ctx, kubeconfig, project, contextID)
		if err != nil {
			return pickerPreviewMsg{contextID: contextID, err: err}
		}
		return pickerPreviewMsg{contextID: contextID, items: items}
	}
}

// maybeLoadPreview kicks off a background fetch for the currently
// highlighted conversation's preview, unless it is already cached or already
// in flight. Called whenever the cursor moves and when the list first loads.
func (m *chatModel) maybeLoadPreview() tea.Cmd {
	if len(m.picker.items) == 0 {
		return nil
	}
	id := m.picker.items[m.picker.cursor].Name
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

// pickerListWidth is the fixed width of the picker's left-hand conversation
// list column when a preview pane is shown alongside it.
const pickerListWidth = 44

// pickerView renders the /resume conversation picker as a full-screen
// overlay: a loading spinner, an error, "no conversations", or a
// cursor-navigable list with a live preview of the highlighted conversation.
func (m *chatModel) pickerView() tea.View {
	var b strings.Builder
	b.WriteString(m.st.header.Render("resume a conversation") + m.st.subtle.Render("  ·  project "+m.project))
	b.WriteString("\n\n")
	switch {
	case m.picker.loading:
		b.WriteString(m.sp.View() + " " + m.st.subtle.Render("loading…"))
	case m.picker.err != "":
		b.WriteString(m.st.err.Render("⚠ " + m.picker.err))
	case len(m.picker.items) == 0:
		b.WriteString(m.st.subtle.Render("no conversations in project " + m.project))
	default:
		for i, c := range m.picker.items {
			line := fmt.Sprintf("%s   %-4s  %d msgs", c.Name, ago(c.Status.LastActiveAt.Time), c.Status.MessageCount)
			line = lipgloss.NewStyle().MaxWidth(pickerListWidth).Render(line)
			if i == m.picker.cursor {
				b.WriteString(m.st.you.Render("> " + line))
			} else {
				b.WriteString("  " + m.st.subtle.Render(line))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n" + m.st.hint.Render("↑/↓ select · enter resume · esc cancel"))

	body := b.String()
	if !m.picker.loading && m.picker.err == "" && len(m.picker.items) > 0 {
		body = lipgloss.JoinHorizontal(lipgloss.Top, b.String(), "    ", m.previewPane())
	}

	v := tea.NewView(m.st.box.Render(body))
	v.AltScreen = true
	return v
}

// pickerPreviewMaxLines caps how many messages the preview pane shows: the
// conversation's opening message (what it was about) plus its most recent
// exchange (where it left off) — the two most useful signals for "do I want
// to resume this one," without needing the full transcript.
const pickerPreviewMaxLines = 6

// previewPane renders the highlighted conversation's transcript preview —
// answering "what's in this conversation" directly in the picker instead of
// forcing a resume-and-look. Truncated to pickerPreviewMaxLines messages: the
// opening message plus the most recent tail.
func (m *chatModel) previewPane() string {
	var b strings.Builder
	b.WriteString(m.st.header.Render("preview"))
	b.WriteString("\n\n")
	if len(m.picker.items) == 0 {
		return b.String()
	}
	id := m.picker.items[m.picker.cursor].Name
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
			b.WriteString(label + ": " + style.Render(previewLine(mm.Content, 64)) + "\n")
			if i == 0 && len(items) > pickerPreviewMaxLines {
				b.WriteString(m.st.subtle.Render("⋮") + "\n")
			}
		}
	}
	return b.String()
}

// previewLine collapses a message to one line and truncates it to maxRunes,
// so a multi-paragraph message doesn't blow out the preview pane's height.
func previewLine(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// helpText lists every slash command; shared by /help's overlay and the
// --help usage text (args.go) so the two can't drift out of sync in spirit,
// though they're rendered differently (plain vs styled).
var helpText = []struct{ cmd, desc string }{
	{"/resume", "browse and resume a past conversation (with a live preview)"},
	{"/clear", "start a fresh conversation, clearing this transcript"},
	{"/compact", "compact this conversation's history now"},
	{"/export", "save this transcript to a file"},
	{"/status", "show the current project, conversation, and turn count"},
	{"/help", "show this list"},
	{"/quit, /exit", "leave"},
}

// commandNames is every literal slash command the input recognizes, for
// autocomplete matching — helpText groups /quit and /exit into one display
// row, but they're two distinct completable commands here.
var commandNames = []string{"/resume", "/clear", "/compact", "/export", "/status", "/help", "/quit", "/exit"}

// commandDescriptions is what the suggestion list shows next to each command
// (see suggestionBar); unlike helpText it keys /quit and /exit separately
// since they're independently completable here.
var commandDescriptions = map[string]string{
	"/resume":  "browse and resume a past conversation (with a live preview)",
	"/clear":   "start a fresh conversation, clearing this transcript",
	"/compact": "compact this conversation's history now",
	"/export":  "save this transcript to a file",
	"/status":  "show the current project, conversation, and turn count",
	"/help":    "show the command list",
	"/quit":    "leave",
	"/exit":    "leave",
}

// isExactCommand reports whether text is already a fully-typed command, so
// Enter can run it directly without consulting suggestions.
func isExactCommand(text string) bool {
	return slices.Contains(commandNames, text)
}

// matchCommands returns every command with the given prefix, in
// commandNames' fixed order (stable across keystrokes, so the highlighted
// index in currentSuggestions doesn't jump around as the prefix narrows).
func matchCommands(prefix string) []string {
	var out []string
	for _, c := range commandNames {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// currentSuggestions returns the slash-command completions for the input's
// current value, or nil when there's nothing to suggest (not a "/" prefix,
// no matches, or already an exact command). Used by both the suggestion bar
// (View) and the tab/enter/up/down key handling (onKey), so they always
// agree on what's being offered.
func (m *chatModel) currentSuggestions() []string {
	text := strings.TrimSpace(m.ti.Value())
	if !strings.HasPrefix(text, "/") || isExactCommand(text) {
		return nil
	}
	return matchCommands(text)
}

// suggestionBar renders the slash-command suggestion list shown above the
// input box, one command per line with the highlighted entry styled
// distinctly, so ↑/↓ moves the highlight the same direction it moves on
// screen. It's windowed to maxSuggestionRows (scrolled to keep the
// highlighted entry in view) and always returns exactly m.suggestionRows()
// lines — blank ones when there's nothing to suggest — so View's layout
// never has to guess how tall this block is.
func (m *chatModel) suggestionBar() string {
	matches := m.currentSuggestions()
	rows := m.suggestionRows()
	if len(matches) == 0 {
		return strings.Repeat("\n", rows-1)
	}
	idx := m.suggestIndex % len(matches)

	start := 0
	if len(matches) > rows {
		start = max(idx-rows/2, 0)
		start = min(start, len(matches)-rows)
	}
	end := min(start+rows, len(matches))

	lines := make([]string, 0, rows)
	for i := start; i < end; i++ {
		cmd := padCommand(matches[i])
		desc := commandDescriptions[matches[i]]
		if i == idx {
			lines = append(lines, m.st.you.Render(cmd)+"  "+m.st.hint.Render(desc+"   tab complete · ↑↓ select"))
		} else {
			lines = append(lines, m.st.subtle.Render(cmd)+"  "+m.st.subtle.Render(desc))
		}
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// commandColumnWidth is the widest slash command name, so descriptions in the
// suggestion list line up in a column regardless of which commands the
// current prefix has narrowed the list down to.
var commandColumnWidth = func() int {
	w := 0
	for _, c := range commandNames {
		if len(c) > w {
			w = len(c)
		}
	}
	return w
}()

// padCommand right-pads a command name to commandColumnWidth.
func padCommand(cmd string) string {
	if pad := commandColumnWidth - len(cmd); pad > 0 {
		return cmd + strings.Repeat(" ", pad)
	}
	return cmd
}

// helpView renders the /help overlay: every slash command and what it does.
// Dismissed by any keypress (see onKey).
func (m *chatModel) helpView() tea.View {
	var b strings.Builder
	b.WriteString(m.st.header.Render("patch commands"))
	b.WriteString("\n\n")
	for _, h := range helpText {
		b.WriteString("  " + m.st.you.Render(h.cmd) + "  " + m.st.subtle.Render(h.desc) + "\n")
	}
	b.WriteString("\n" + m.st.hint.Render("any key to dismiss"))

	v := tea.NewView(m.st.box.Render(b.String()))
	v.AltScreen = true
	return v
}

// statusView renders the /status overlay: the current session's project,
// conversation id, and turn count. Dismissed by any keypress (see onKey).
func (m *chatModel) statusView() tea.View {
	ctx := m.contextID
	if ctx == "" {
		ctx = "(none yet — starts on your first message)"
	}
	var b strings.Builder
	b.WriteString(m.st.header.Render("session status"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "  %s  %s\n", m.st.you.Render("project"), m.project)
	fmt.Fprintf(&b, "  %s  %s\n", m.st.you.Render("context"), ctx)
	fmt.Fprintf(&b, "  %s  %d\n", m.st.you.Render("turns"), len(m.raw))
	b.WriteString("\n" + m.st.hint.Render("any key to dismiss"))

	v := tea.NewView(m.st.box.Render(b.String()))
	v.AltScreen = true
	return v
}

// exportTranscript writes the current conversation to a plain-text markdown
// file in the working directory, returning its name. Walks m.raw (unstyled)
// rather than m.turns (ANSI-styled for the viewport).
func (m *chatModel) exportTranscript() (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# patch conversation\n\nproject: %s\n", m.project)
	if m.contextID != "" {
		fmt.Fprintf(&b, "context: %s\n", m.contextID)
	}
	fmt.Fprintf(&b, "exported: %s\n\n", time.Now().Format(time.RFC3339))
	for _, t := range m.raw {
		label := "You"
		switch t.role {
		case "assistant":
			label = "Patch"
		case "system":
			label = "System"
		case "summary":
			label = "Summary"
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", label, t.content)
	}
	name := "patch-chat-" + time.Now().Format("20060102-150405") + ".md"
	if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return name, nil
}

func (m *chatModel) View() tea.View {
	if !m.ready {
		v := tea.NewView(m.st.box.Render("starting patch chat…"))
		v.AltScreen = true
		return v
	}
	if m.picker.open {
		return m.pickerView()
	}
	switch m.overlay {
	case "help":
		return m.helpView()
	case "status":
		return m.statusView()
	}
	var status string
	if m.working {
		status = m.sp.View() + " " + m.st.subtle.Render("thinking…")
	} else {
		status = m.st.hint.Render("enter to send · pgup/pgdn scroll · / for commands (tab completes) · ctrl+c or /quit to leave")
	}
	// One blank line between each major region for an even vertical rhythm:
	// header → blank → transcript → gap/suggestions → bordered input → status.
	// Suggestions sit directly above the box since they're about what you're
	// typing; the status/hint line reads as a footer below the box instead of
	// a caption above it. The suggestion block grows downward (vertically, so
	// ↑/↓ tracks it) up to maxSuggestionRows; the viewport shrinks by the same
	// amount so total layout height stays constant instead of pushing the
	// input off-screen.
	if h := m.viewportHeight(); h != m.vp.Height() {
		m.vp.SetHeight(h)
	}
	gap := m.suggestionBar()
	body := strings.Join([]string{
		m.st.header.Render("patch") + m.st.subtle.Render("  ·  project "+m.project),
		"", // gap below the header
		m.vp.View(),
		gap,
		m.st.inputBox.Render(m.ti.View()),
		status,
	}, "\n")

	v := tea.NewView(m.st.box.Render(body))
	v.AltScreen = true
	return v
}

// submit records the user's line, starts the spinner, and launches the
// streaming turn. It returns the spinner tick command; answer chunks arrive
// asynchronously via p.Send from the goroutine started here.
func (m *chatModel) submit(text string) tea.Cmd {
	m.turns = append(m.turns, m.turnBlock(m.st.you.Render("You"), m.st.userText.Width(m.contentWidth()).Render(text)))
	m.raw = append(m.raw, transcriptTurn{role: "user", content: text})
	m.answer.Reset()
	m.working = true
	m.rebuildViewport()

	go m.stream(text, m.contextID)
	return m.sp.Tick
}

// stream runs one turn against the a2a client, forwarding events to the model
// via p.Send. It runs in its own goroutine (a Cmd yields a single message,
// whereas a turn produces many), which is the supported Bubble Tea pattern for
// long-lived producers.
func (m *chatModel) stream(text, contextID string) {
	req := &a2a.SendMessageRequest{Message: buildMessage(text, m.project, contextID)}
	events := m.client.SendStreamingMessage(m.ctx, req)
	seen := contextID
	for ev, err := range events {
		if err != nil {
			m.prog.Send(streamErrMsg{err})
			return
		}
		if id := eventContextID(ev); id != "" {
			seen = id
		}
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			if t := textOf(e.Artifact.Parts); t != "" {
				m.prog.Send(streamChunkMsg{t})
			}
		case *a2a.Message:
			if t := textOf(e.Parts); t != "" {
				m.prog.Send(streamChunkMsg{t})
			}
		case *a2a.TaskStatusUpdateEvent:
			if e.Status.State == a2a.TaskStateFailed {
				note := ""
				if e.Status.Message != nil {
					note = textOf(e.Status.Message.Parts)
				}
				m.prog.Send(streamDoneMsg{contextID: seen, failed: true, failMsg: note})
				return
			}
		}
	}
	m.prog.Send(streamDoneMsg{contextID: seen})
}

// compact runs one manual "/compact" request against POST /v1/compact — a
// plain REST call, not the a2a client, since there is no message to answer —
// and reports the outcome back via p.Send, the same producer-in-a-goroutine
// pattern [chatModel.stream] uses.
func (m *chatModel) compact() {
	err := requestCompact(m.ctx, m.baseURL, m.token, m.project, m.contextID)
	m.prog.Send(compactDoneMsg{err: err})
}

// contentWidth is the usable inner width: terminal width minus the outer box's
// horizontal padding (2 columns each side).
func (m *chatModel) contentWidth() int {
	w := m.width - 4
	if w < 20 {
		w = 20
	}
	return w
}

// maxSuggestionRows caps how tall the vertical slash-command suggestion list
// can grow (matches are windowed around the highlighted one beyond this).
const maxSuggestionRows = 4

// suggestionRows is how many lines the row above the input currently needs:
// 1 (blank) when nothing is suggested, or one line per suggestion up to
// maxSuggestionRows. The viewport is resized around this each render so the
// suggestion list can grow downward without the input box or status line
// jumping around.
func (m *chatModel) suggestionRows() int {
	if n := len(m.currentSuggestions()); n > 0 {
		if n > maxSuggestionRows {
			return maxSuggestionRows
		}
		return n
	}
	return 1
}

// viewportHeight computes the transcript viewport's height from the last
// known terminal height, chrome (outer box padding, header, header gap,
// status line, bordered input), and the current suggestion block size.
func (m *chatModel) viewportHeight() int {
	// outer box padding (2) + header (1) + header gap (1) + status (1) +
	// bordered input (3 = border top/bottom + one text row) = 8 rows of fixed
	// chrome; suggestionRows() adds the variable row(s) above the input.
	h := m.termHeight - 8 - m.suggestionRows()
	if h < 1 {
		h = 1
	}
	return h
}

// layout (re)sizes the viewport/input and rebuilds the glamour renderer for the
// new width, then re-renders. On the first layout it fires the auto first turn.
func (m *chatModel) layout(width, height int) tea.Cmd {
	m.width = width
	m.termHeight = height
	cw := m.contentWidth()
	vpHeight := m.viewportHeight()
	// Text field width: content width minus the input box's border (2 cols) and
	// inner padding (2 cols) and the "> " prompt (2 cols), so the box stays
	// within the content column.
	tiWidth := cw - 6
	if tiWidth < 10 {
		tiWidth = 10
	}
	m.vp.SetWidth(cw)
	m.vp.SetHeight(vpHeight)
	m.ti.SetWidth(tiWidth)
	m.renderer = newMarkdownRenderer(cw, m.dark) // glamour wrap stays at content width
	m.ready = true
	m.rebuildViewport()

	if !m.sentFirst && m.firstMessage != "" {
		m.sentFirst = true
		return m.submit(m.firstMessage)
	}
	return nil
}

// turnBlock lays out one turn identically for both speakers: a bold colored
// label line, then the body on the following line(s), both flush-left at the
// same column. Consecutive turns are separated by exactly one blank line (see
// rebuildViewport).
func (m *chatModel) turnBlock(label, body string) string {
	return label + "\n" + body
}

// rebuildViewport composes the transcript (finalized turns plus any in-progress
// answer) into the viewport, one blank line between turns, pinned to the bottom.
func (m *chatModel) rebuildViewport() {
	blocks := make([]string, 0, len(m.turns)+1)
	blocks = append(blocks, m.turns...)
	if m.working {
		ans := m.renderMarkdown(m.answer.String())
		if strings.TrimSpace(ans) == "" {
			ans = m.st.subtle.Render("…")
		}
		blocks = append(blocks, m.turnBlock(m.st.patch.Render("Patch"), ans))
	}
	m.vp.SetContent(strings.Join(blocks, "\n\n"))
	m.vp.GotoBottom()
}

// renderMarkdown formats markdown through glamour, falling back to the raw text
// if the renderer is unset or errors (e.g. on a partial mid-stream chunk). It
// strips glamour's leading/trailing blank lines so answers align with labels.
func (m *chatModel) renderMarkdown(s string) string {
	if m.renderer == nil || strings.TrimSpace(s) == "" {
		return s
	}
	out, err := m.renderer.Render(s)
	if err != nil {
		return s
	}
	return strings.Trim(out, "\n")
}

// newMarkdownRenderer builds a glamour renderer with an EXPLICIT dark/light
// style (never WithAutoStyle, which would query the terminal itself and both
// leak the response into input and fall back to a low-contrast theme). The base
// style's Document margin/indent is zeroed so rendered answers sit flush-left,
// aligned with the speaker labels and the user's message text.
func newMarkdownRenderer(width int, dark bool) *glamour.TermRenderer {
	if width < 20 {
		width = 80
	}
	// Copy the shared package style (value type) so zeroing the margin doesn't
	// mutate the global; Document.Margin is a *uint, so point it at a fresh 0.
	cfg := glamourstyles.LightStyleConfig
	if dark {
		cfg = glamourstyles.DarkStyleConfig
	}
	zero := uint(0)
	cfg.Document.Margin = &zero
	cfg.Document.Indent = &zero

	r, err := glamour.NewTermRenderer(glamour.WithStyles(cfg), glamour.WithWordWrap(width))
	if err != nil {
		return nil
	}
	return r
}
