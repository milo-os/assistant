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
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	spin "charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	glamourstyles "github.com/charmbracelet/glamour/styles"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
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
)

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
	ctx     context.Context
	prog    *tea.Program
	client  *a2aclient.Client
	project string

	vp       viewport.Model
	ti       textinput.Model
	sp       spin.Model
	renderer *glamour.TermRenderer
	st       styles

	contextID string
	turns     []string        // finalized, already-rendered conversation blocks
	answer    strings.Builder // in-progress assistant answer (raw markdown)
	working   bool
	dark      bool

	firstMessage string // sent automatically once, on first layout
	sentFirst    bool
	ready        bool
	width        int // full terminal width; content width subtracts padding
}

func runChatTUI(ctx context.Context, client *a2aclient.Client, project, contextID, firstMessage string) int {
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
		if !m.working {
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
		m.answer.Reset()
		m.working = false
		m.rebuildViewport()
		return m, nil

	case streamErrMsg:
		m.turns = append(m.turns, m.st.err.Render("patch: "+friendlyError(msg.err)))
		m.answer.Reset()
		m.working = false
		m.rebuildViewport()
		return m, nil

	case tea.MouseWheelMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *chatModel) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m, tea.Quit
	case "enter":
		if m.working {
			return m, nil
		}
		text := strings.TrimSpace(m.ti.Value())
		if text == "" {
			return m, nil
		}
		if text == "/quit" || text == "/exit" {
			return m, tea.Quit
		}
		m.ti.Reset()
		return m, m.submit(text)
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
		return m, cmd
	}
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

func (m *chatModel) View() tea.View {
	if !m.ready {
		v := tea.NewView(m.st.box.Render("starting patch chat…"))
		v.AltScreen = true
		return v
	}
	var status string
	if m.working {
		status = m.sp.View() + " " + m.st.subtle.Render("thinking…")
	} else {
		status = m.st.hint.Render("enter to send · pgup/pgdn scroll · ctrl+c or /quit to leave")
	}
	// One blank line between each major region for an even vertical rhythm:
	// header → blank → transcript → status → blank → bordered input.
	body := strings.Join([]string{
		m.st.header.Render("patch") + m.st.subtle.Render("  ·  project "+m.project),
		"", // gap below the header
		m.vp.View(),
		status,
		"", // gap above the input box
		m.st.inputBox.Render(m.ti.View()),
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

// contentWidth is the usable inner width: terminal width minus the outer box's
// horizontal padding (2 columns each side).
func (m *chatModel) contentWidth() int {
	w := m.width - 4
	if w < 20 {
		w = 20
	}
	return w
}

// layout (re)sizes the viewport/input and rebuilds the glamour renderer for the
// new width, then re-renders. On the first layout it fires the auto first turn.
func (m *chatModel) layout(width, height int) tea.Cmd {
	m.width = width
	cw := m.contentWidth()
	// Vertical budget: outer box padding (2) + header (1) + header gap (1) +
	// status (1) + separator (1) + bordered input (3 = border top/bottom + one
	// text row) = 9 rows of chrome; the rest is the transcript viewport.
	vpHeight := height - 9
	if vpHeight < 1 {
		vpHeight = 1
	}
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
