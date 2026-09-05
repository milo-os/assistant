// Opt-in full-screen chat UI for the `patch` CLI, built on Bubble Tea v2. It is
// an alternative to the line-based REPL (runRepl) selected with `chat --tui`;
// the plain and interactive modes are untouched.
//
// Layout: a header, a scrollable transcript viewport, a multi-line composer,
// and a one-line footer holding the session state (spinner while the assistant
// works, else the key hint) on the left with session badges right-aligned
// against it, all wrapped in a padded container. Assistant answers stream in live
// and are rendered as markdown via glamour, so bold/bullets/tables come out
// formatted rather than as raw `**`. The composer itself (its styling, prompt
// history and paste chips) lives in chat_composer.go; this file owns its keys.
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
//
// Turn feedback: every finished turn is closed out by a "Worked for 23s" line
// under its block, the window title tracks whether a turn is running, and a
// bell (optionally an OSC 9 desktop notification — see notifyMode) says so to a
// user who has looked away.
//
// Turns: each streaming turn owns a cancellable context, so esc can stop this
// turn without touching the session's. A stopped turn is finalized in place
// (kept text plus an "interrupted" marker) and its generation is bumped, which
// is what makes the abandoned goroutine's remaining messages stale. Messages
// typed while a turn runs are queued and sent one per turn as each finishes.
package patchcli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	spin "charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/glamour"
	glamourstyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"

	"github.com/a2aproject/a2a-go/v2/a2a"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// Streaming messages delivered to the model via p.Send from the turn goroutine.
// Each carries the gen of the turn that produced it: an interrupt bumps
// chatModel.turnGen, so the abandoned goroutine's late messages are dropped
// instead of landing in the next turn (see [chatModel.interrupt]).
type (
	// streamChunkMsg is one partial answer chunk (artifact/message text).
	streamChunkMsg struct {
		gen  int
		text string
	}
	// streamActivityMsg is one tool-activity update (a tool call starting or
	// finishing) decoded from the service's working status updates.
	streamActivityMsg struct {
		gen int
		act toolActivity
	}
	// streamIDsMsg carries the task and context ids as soon as the stream
	// reveals them, rather than only at the end of the turn: an interrupt
	// needs the task id to cancel, and the context id so the conversation an
	// abandoned turn started is still threaded onto by the next one.
	streamIDsMsg struct {
		gen       int
		taskID    a2a.TaskID
		contextID string
	}
	// streamDoneMsg ends a turn; contextID threads the conversation forward.
	streamDoneMsg struct {
		gen       int
		contextID string
		failed    bool
		failMsg   string
	}
	// streamErrMsg is a transport/stream error surfaced by the a2a client.
	streamErrMsg struct {
		gen int
		err error
	}
	// compactDoneMsg ends a /compact request (POST /v1/compact, outside the
	// a2a client): err is nil on success, [ErrNothingToCompact] when the
	// server had nothing to fold, or any other error on a real failure.
	compactDoneMsg struct{ err error }
	// renameDoneMsg ends a /rename request (POST /v1/conversations/rename):
	// name is what was asked for, err nil on success.
	renameDoneMsg struct {
		name string
		err  error
	}
	// pickerListMsg delivers the picker's conversation listing (or an error).
	// See chat_picker.go for the picker itself.
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

// activityRow is one tool call shown as a collapsed line inside a Patch turn
// block. started is when this client saw the call begin, so a running row can
// count up live; elapsed is the service's own measurement, used once done.
type activityRow struct {
	id      string
	name    string
	summary string
	started time.Time
	elapsed time.Duration
	done    bool
	ok      bool
	// stopped marks a call that was still running when the user interrupted
	// the turn: finished, but neither a success nor a failure of its own.
	stopped bool
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
	rowAlt   lipgloss.Style // picker: every other row's background (zebra)
	rowSel   lipgloss.Style // picker: the cursor row's background
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
	zebra := ld(lipgloss.Color("#F1EBD8"), lipgloss.Color("#2B2924"))  // one shade off the page
	selRow := ld(lipgloss.Color("#E4DDC6"), lipgloss.Color("#3A372F")) // two shades off, under the cursor

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
		rowAlt:   lipgloss.NewStyle().Background(zebra),
		rowSel:   lipgloss.NewStyle().Background(selRow),
	}
}

// newInputStyles styles the picker's single-line search field (the chat's own
// composer is a textarea — see newComposerStyles). The bubbles defaults render
// text and placeholder faint (nearly invisible on a cream bg), so override the
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
	client  *serviceClient
	project string
	view    ReadView // how the /resume picker reaches the aggregated API
	baseURL string   // for /compact's POST /v1/compact call (outside the a2a client)
	token   TokenSource

	vp       viewport.Model
	ta       textarea.Model
	sp       spin.Model
	renderer *glamour.TermRenderer
	st       styles

	contextID string
	// convName is the user-given name of the active conversation, "" when it
	// has none. Shown by /status; set by /rename and picked up from the
	// listing when a conversation is resumed.
	convName string
	turns    []string         // finalized, already-rendered conversation blocks
	raw      []transcriptTurn // parallel to turns, unstyled — source for /export
	answer   strings.Builder  // in-progress assistant answer (raw markdown)
	working  bool
	dark     bool

	// turnStart is when the current (or last) unit of work began — stamped
	// wherever the working state is entered (see startedWorking), so the
	// end-of-turn line and the footer's counter both measure from the
	// keystroke that started it rather than from the first streamed chunk.
	turnStart time.Time

	// notify is how a finished turn announces itself; PATCH_NOTIFY chooses it.
	notify notifyMode

	// activity holds the in-progress turn's tool calls; turnActivity keeps the
	// finished ones by their index in turns, so the transcript still shows what
	// each answer did. They stay structured (not pre-rendered into the block)
	// because ctrl+o re-renders every turn's rows folded or expanded.
	activity       []activityRow
	turnActivity   map[int][]activityRow
	expandActivity bool

	// turnGen identifies the running turn, cancelTurn cancels its context and
	// taskID is the service-side task to cancel with it. An interrupt bumps
	// turnGen, which is what makes the abandoned goroutine's remaining
	// messages stale (see [chatModel.interrupt]).
	turnGen    int
	cancelTurn context.CancelFunc
	taskID     a2a.TaskID

	// queued holds messages typed while a turn was running, listed above the
	// composer and sent one per turn as each finishes. Capped at
	// maxQueuedMessages.
	queued []string

	// ctrlCAt is when ctrl+c was last pressed, so a second press within
	// ctrlCExitWindow leaves whatever the first one did instead.
	ctrlCAt time.Time

	// follow pins the transcript to the newest output. It is cleared when the
	// user scrolls up and restored when they reach the bottom again (or send a
	// new message), so a streaming answer never yanks the view away from
	// history the user is reading.
	follow bool

	// overlay is "" (normal chat), "help", or "status" — a read-only reference
	// screen shown full-screen and dismissed by any keypress (see onKey).
	overlay string

	picker pickerState

	// resumeOnStart opens the picker (resumeOnStart == pickerOnStart) or
	// loads one conversation's transcript (any other value is a context id)
	// on the first layout, for `patch resume [<context-id>]`.
	resumeOnStart string

	// suggestIndex is the highlighted entry in the slash-command suggestion
	// list (see currentSuggestions). Reset to 0 whenever the input text
	// changes so a fresh prefix always starts at the top match.
	suggestIndex int

	// history is this project's past prompts, oldest last-in, shared by ↑/↓
	// recall and ctrl+r search. histIdx == len(history) means "not recalling":
	// histDraft then holds whatever the composer had when recall started, and
	// histRecall the exact text the last recall put there (so an edit is
	// detectable — see composerRecallable).
	history    []string
	histIdx    int
	histDraft  string
	histRecall string
	histPath   string // "" when there was no config dir; recall stays in-session

	// rsearch is the ctrl+r reverse search over history: rsearchQ is what has
	// been typed into it and rsearchN how many matches back ctrl+r has stepped.
	rsearch  bool
	rsearchQ string
	rsearchN int

	// pastes holds the full text behind each "[Pasted text #N …]" chip in the
	// composer, in chip order, expanded back into the message on submit.
	pastes []string

	termHeight int // last WindowSizeMsg height, so View can resize vp around a variable-height suggestion block

	firstMessage string // sent automatically once, on first layout
	sentFirst    bool
	ready        bool
	width        int // full terminal width; content width subtracts padding
}

// pickerOnStart is the resumeOnStart sentinel meaning "open the picker".
const pickerOnStart = "\x00picker"

func runChatTUI(ctx context.Context, client *serviceClient, project, contextID, firstMessage, baseURL string, token TokenSource, view ReadView, resumeOnStart string) int {
	st := newStyles(false) // provisional (light) until the background is learned

	sp := spin.New(spin.WithSpinner(spin.Dot), spin.WithStyle(st.patch))

	ta := newComposer(false) // provisional (light) until bg is learned

	path := historyPath(project)
	hist := loadHistory(path)

	m := &chatModel{
		ctx:           ctx,
		client:        client,
		project:       project,
		view:          view,
		baseURL:       baseURL,
		token:         token,
		contextID:     contextID,
		vp:            viewport.New(),
		ta:            ta,
		sp:            sp,
		st:            st,
		history:       hist,
		histIdx:       len(hist),
		histPath:      path,
		firstMessage:  firstMessage,
		resumeOnStart: resumeOnStart,
		follow:        true,
		// Read here rather than threaded through Invocation: it is a display
		// preference of this one screen, and both entrypoints (patch and the
		// datumctl plugin) reach the TUI through this function.
		notify: parseNotifyMode(os.Getenv("PATCH_NOTIFY")),
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

// newView wraps a rendered body in the tea.View every screen here uses. Both
// settings must be identical across screens: the renderer diffs the mode
// against the previous frame, so a view that forgot MouseMode would disable
// mouse reporting for as long as it was showing (and re-enable it after).
func (m *chatModel) newView(body string) tea.View {
	v := tea.NewView(body)
	v.AltScreen = true
	// Bubble Tea v2 carries the title on the view (there is no SetWindowTitle
	// command); the renderer diffs it, so re-setting it every frame is free.
	v.WindowTitle = m.windowTitle()
	// Cell motion gives us wheel events for scrolling the transcript. It also
	// means the terminal, not the app, stops owning click-drag: selecting text
	// to copy needs Option (macOS) or Shift (Linux) held down.
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m *chatModel) Init() tea.Cmd {
	// RequestBackgroundColor makes Bubble Tea own the OSC-11 query and consume
	// the terminal's reply, so it never leaks into the text input. It's a
	// Msg-returning func, so wrap it as a Cmd.
	bgColor := func() tea.Msg { return tea.RequestBackgroundColor() }
	return tea.Batch(m.ta.Focus(), textarea.Blink, bgColor)
}

func (m *chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Both handlers can enter the working state (layout fires the auto
		// first message; onKey submits and runs /compact, /rename), so the
		// stamp is taken here, around them, instead of inside them.
		was := m.working
		cmd := m.layout(msg.Width, msg.Height)
		m.startedWorking(was)
		return m, cmd

	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.st = newStyles(m.dark)
		m.ta.SetStyles(newComposerStyles(m.dark))
		if m.picker.open {
			m.picker.search.SetStyles(newInputStyles(m.dark))
		}
		if !m.working {
			m.sp = spin.New(spin.WithSpinner(spin.Dot), spin.WithStyle(m.st.patch))
		}
		if m.width > 0 {
			m.renderer = newMarkdownRenderer(m.contentWidth(), m.dark)
		}
		m.rebuildViewport()
		return m, nil

	case tea.KeyPressMsg:
		was := m.working
		model, cmd := m.onKey(msg)
		m.startedWorking(was)
		return model, cmd

	case tea.PasteMsg:
		// Bracketed paste is on by default (see newView), so a paste arrives
		// whole rather than as a burst of keys. Only the composer takes them:
		// the picker and the overlays are not places to paste into.
		if m.picker.open || m.overlay != "" || m.rsearch {
			return m, nil
		}
		m.onPaste(msg.Content)
		return m, nil

	case spin.TickMsg:
		if !m.working && !m.picker.loading {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		// A running tool row counts up, so the transcript has to be redrawn on
		// the spinner's beat as well — nothing else moves it between chunks.
		if hasRunning(m.activity) {
			m.rebuildViewport()
		}
		return m, cmd

	case streamChunkMsg:
		if msg.gen != m.turnGen {
			return m, nil
		}
		m.answer.WriteString(msg.text)
		m.rebuildViewport()
		return m, nil

	case streamActivityMsg:
		if msg.gen != m.turnGen {
			return m, nil
		}
		m.applyActivity(msg.act)
		m.rebuildViewport()
		return m, nil

	case streamIDsMsg:
		if msg.gen != m.turnGen {
			return m, nil
		}
		m.taskID = msg.taskID
		if msg.contextID != "" {
			m.contextID = msg.contextID
		}
		return m, nil

	case streamDoneMsg:
		if msg.gen != m.turnGen {
			return m, nil
		}
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
		tools := len(m.activity)
		m.turns = append(m.turns, m.turnBlock(m.st.patch.Render("Patch"), answer))
		m.keepActivity(len(m.turns) - 1)
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
		m.endTurn()
		m.closeOutTurn(tools)
		m.rebuildViewport()
		return m, tea.Batch(m.notifyDone(), m.drainQueue())

	case streamErrMsg:
		if msg.gen != m.turnGen {
			return m, nil
		}
		errText := friendlyError(msg.err, m.client.errs)
		tools := len(m.activity)
		m.turns = append(m.turns, m.st.err.Render("patch: "+errText))
		m.keepActivity(len(m.turns) - 1) // what ran before the break is part of the story
		m.raw = append(m.raw, transcriptTurn{role: "system", content: "error: " + errText})
		m.answer.Reset()
		m.endTurn()
		// The queue is dropped rather than drained: the breaks that end a turn
		// this way (auth, a bad project) would fail every held message too.
		m.queued = nil
		m.closeOutTurn(tools)
		m.rebuildViewport()
		return m, m.notifyDone()

	case compactDoneMsg:
		m.working = false
		switch {
		case msg.err == nil:
			m.turns = append(m.turns, m.compactionRule())
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

	case renameDoneMsg:
		m.working = false
		if msg.err != nil {
			m.turns = append(m.turns, m.st.err.Render("⚠ rename failed: "+msg.err.Error()))
			m.raw = append(m.raw, transcriptTurn{role: "system", content: "rename failed: " + msg.err.Error()})
		} else {
			m.convName = msg.name
			m.turns = append(m.turns, m.st.subtle.Render("renamed to "+msg.name))
			m.raw = append(m.raw, transcriptTurn{role: "system", content: "renamed to " + msg.name})
		}
		m.rebuildViewport()
		return m, nil

	case tea.MouseWheelMsg:
		if m.picker.open || m.overlay != "" {
			// The transcript isn't on screen; scrolling it here would leave the
			// user somewhere they never navigated to once the picker closes.
			return m, nil
		}
		// The viewport handles wheel events itself (MouseWheelEnabled, 3 lines
		// a notch); scrollBy only re-derives follow from where it ends up.
		m.scrollBy(func() { m.vp, _ = m.vp.Update(msg) })
		return m, nil

	case pickerListMsg:
		m.picker.loading = false
		if msg.err != nil {
			m.picker.err = msg.err.Error()
			return m, nil
		}
		m.picker.items = msg.items
		m.picker.refilter()
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
		m.convName = pickerName(m.picker.items, msg.contextID)
		m.turns = m.turns[:0]
		m.raw = m.raw[:0]
		m.answer.Reset()
		m.activity, m.turnActivity = nil, nil
		for _, mm := range msg.items {
			switch mm.Role {
			case "user":
				m.turns = append(m.turns, m.turnBlock(m.st.you.Render("You"),
					m.st.userText.Width(m.contentWidth()).Render(mm.Content)))
			case "summary":
				// A compaction digest, not something the assistant said —
				// rendered de-emphasized (m.st.subtle, the same token used
				// elsewhere for placeholder/loading text) so it reads as
				// synthetic history rather than a turn of the conversation,
				// under the same rule /compact draws when it folds history live.
				m.turns = append(m.turns, m.compactionRule()+"\n"+
					m.turnBlock(m.st.subtle.Render("Summary"),
						m.st.subtle.Width(m.contentWidth()).Render(mm.Content)))
			default:
				m.turns = append(m.turns, m.turnBlock(m.st.patch.Render("Patch"), m.renderMarkdown(mm.Content)))
			}
			m.raw = append(m.raw, transcriptTurn{role: mm.Role, content: mm.Content})
		}
		// Session state, not conversation content: it closes the loaded
		// transcript in the viewport but stays out of m.raw, so /export and the
		// turn count keep describing the conversation itself.
		m.turns = append(m.turns, m.recapLine(msg.items))
		m.picker = pickerState{}
		m.follow = true
		m.rebuildViewport()
		return m, nil
	}
	return m, nil
}

func (m *chatModel) onKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// In the picker or an overlay ctrl+c/d stay the plain escape hatch they
	// always were; only the chat itself gives ctrl+c its three-way meaning.
	switch msg.String() {
	case "ctrl+c":
		if m.picker.open || m.overlay != "" {
			return m, tea.Quit
		}
		return m.onCtrlC()
	case "ctrl+d":
		// The shell rule: EOF ends the session only on an empty line, so a
		// stray ctrl+d cannot throw a half-typed message away.
		if m.picker.open || m.overlay != "" || m.ta.Value() == "" {
			return m, tea.Quit
		}
		return m, nil
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
	if m.rsearch {
		return m.onSearchKey(msg)
	}
	switch msg.String() {
	case "shift+enter", "ctrl+j", "alt+enter":
		// The three ways terminals report "newline, don't send". shift+enter
		// needs a terminal that reports modifiers on enter; ctrl+j (a bare LF)
		// always gets through, which is why it's here.
		return m, m.insertNewline()
	case "enter":
		// A backslash immediately before the cursor is the Claude Code
		// convention for continuing onto a new line; it is consumed, not sent.
		if m.backslashContinuation() {
			m.ta, _ = m.ta.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
			return m, m.insertNewline()
		}
		text := strings.TrimSpace(m.ta.Value())
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
		// Chips expand into the real paste before anything else sees the text,
		// so the transcript, the history file and the assistant all agree on
		// what was sent. Commands carry no chips, so this is a no-op for them.
		message := expandPasteChips(text, m.pastes)
		if m.working {
			// Slash commands act on the session rather than the conversation,
			// so they are not held for later — they would run against a state
			// the user can no longer see when the turn ends.
			if strings.HasPrefix(text, "/") {
				return m, nil
			}
			m.enqueue(message)
			return m, nil
		}
		m.recordHistory(message)
		// /rename is the only command that takes an argument, so it can't be a case in the exact-match switch below.
		if arg, ok := commandArg(text, "/rename"); ok {
			m.resetComposer()
			return m, m.startRename(arg)
		}
		switch text {
		case "/quit", "/exit":
			return m, tea.Quit
		case "/resume":
			m.resetComposer()
			return m, m.openPicker()
		case "/clear":
			m.resetComposer()
			m.contextID = ""
			m.convName = ""
			m.turns = nil
			m.raw = nil
			m.answer.Reset()
			m.activity, m.turnActivity = nil, nil
			m.follow = true
			m.rebuildViewport()
			return m, nil
		case "/compact":
			m.resetComposer()
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
			m.resetComposer()
			m.overlay = "help"
			return m, nil
		case "/status":
			m.resetComposer()
			m.overlay = "status"
			return m, nil
		case "/export":
			m.resetComposer()
			path, err := m.exportTranscript()
			if err != nil {
				m.turns = append(m.turns, m.st.err.Render("⚠ export failed: "+err.Error()))
			} else {
				m.turns = append(m.turns, m.st.subtle.Render("exported to "+path))
			}
			m.rebuildViewport()
			return m, nil
		}
		m.resetComposer()
		return m, m.submit(message)
	case "ctrl+r":
		m.rsearch, m.rsearchQ, m.rsearchN = true, "", 0
		return m, nil
	case "tab":
		// Complete to the highlighted suggestion without submitting — lets the
		// user see/edit before running, and is a no-op when there's nothing to
		// suggest (already exact, or not a "/" prefix at all).
		if matches := m.currentSuggestions(); len(matches) > 0 {
			m.ta.SetValue(matches[m.suggestIndex%len(matches)])
			m.ta.MoveToEnd()
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
		// A one-line composer holding nothing but (at most) the prompt the last
		// recall put there is the shell case: ↑/↓ walk history. Several lines,
		// or text the user has since edited, means the cursor moves instead.
		if !m.composerRecallable() {
			var cmd tea.Cmd
			m.ta, cmd = m.ta.Update(msg)
			return m, cmd
		}
		if m.recallHistory(msg.String() == "up") {
			return m, nil
		}
		// Nothing left to recall: scroll the transcript a line at a time, the
		// behavior these keys had before history existed.
		if msg.String() == "up" {
			m.scrollBy(func() { m.vp.ScrollUp(1) })
		} else {
			m.scrollBy(func() { m.vp.ScrollDown(1) })
		}
		return m, nil
	case "ctrl+o":
		// Expand/collapse the tool-activity rows of every turn at once: the
		// folded "Called 3 tools" line is a summary of detail the user can ask
		// for, not a different set of turns.
		m.expandActivity = !m.expandActivity
		m.rebuildViewport()
		return m, nil
	case "pgup":
		m.scrollBy(m.vp.PageUp)
		return m, nil
	case "pgdown":
		m.scrollBy(m.vp.PageDown)
		return m, nil
	case "shift+up":
		m.scrollBy(func() { m.vp.ScrollUp(1) })
		return m, nil
	case "shift+down":
		m.scrollBy(func() { m.vp.ScrollDown(1) })
		return m, nil
	case "ctrl+home":
		m.scrollBy(func() { m.vp.GotoTop() })
		return m, nil
	case "esc":
		// Newest intent first: drop what has not been sent yet, then stop what
		// is running, and only then fall back to "back to the latest" — the
		// way out of having scrolled up, without hunting for the bottom.
		if len(m.queued) > 0 {
			m.queued = nil
			return m, nil
		}
		if m.interruptible() {
			m.interrupt()
			return m, nil
		}
		m.scrollBy(func() { m.vp.GotoBottom() })
		return m, nil
	case "ctrl+end":
		m.scrollBy(func() { m.vp.GotoBottom() })
		return m, nil
	default:
		// "?" on an empty composer is the keys cheatsheet (the /help overlay,
		// which any key dismisses — so a second "?" closes it); typed into a
		// draft it is just a character.
		if msg.String() == "?" && m.ta.Value() == "" {
			m.overlay = "help"
			return m, nil
		}
		// Only forward genuine editing input to the text field. Printable keys
		// carry non-empty .Text; editing keys (backspace, arrows, …) don't but
		// are allowlisted. Everything else (stray/unparsed escape sequences) is
		// dropped so it can't be inserted as literal garbage.
		if msg.Text == "" && !isEditingKey(msg.String()) {
			return m, nil
		}
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		m.suggestIndex = 0 // a new prefix invalidates the old highlighted index
		return m, cmd
	}
}

// insertNewline feeds the composer its own InsertNewline binding, so every key
// we accept for "newline, don't send" lands on one code path.
func (m *chatModel) insertNewline() tea.Cmd {
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return cmd
}

// backslashContinuation reports whether the character just before the cursor is
// a backslash — the marker that this Enter should break the line instead.
func (m *chatModel) backslashContinuation() bool {
	lines := strings.Split(m.ta.Value(), "\n")
	row, col := m.ta.Line(), m.ta.Column()
	if row < 0 || row >= len(lines) {
		return false
	}
	r := []rune(lines[row])
	return col > 0 && col <= len(r) && r[col-1] == '\\'
}

// resetComposer clears the input and everything that hangs off its contents:
// the paste chips its text referred to, and the history cursor.
func (m *chatModel) resetComposer() {
	m.ta.Reset()
	m.pastes = nil
	m.histIdx = len(m.history)
	m.histDraft = ""
	m.histRecall = ""
}

// setComposer replaces the input with a recalled prompt and remembers it, so
// composerRecallable can tell an untouched recall from something typed over it.
func (m *chatModel) setComposer(s string) {
	m.ta.SetValue(s)
	m.ta.MoveToEnd()
	m.histRecall = s
}

// composerRecallable reports whether ↑/↓ should walk history rather than move
// the cursor: a one-line composer that is empty or still holds exactly what the
// last recall put there.
func (m *chatModel) composerRecallable() bool {
	if m.ta.LineCount() > 1 {
		return false
	}
	v := m.ta.Value()
	return v == "" || v == m.histRecall
}

// recallHistory steps one entry older (or newer) and loads it into the
// composer, reporting whether there was anything to step onto. Stepping past
// the newest entry restores the draft the recall interrupted.
func (m *chatModel) recallHistory(older bool) bool {
	if older {
		if m.histIdx <= 0 {
			return false
		}
		if m.histIdx == len(m.history) {
			m.histDraft = m.ta.Value()
		}
		m.histIdx--
		m.setComposer(m.history[m.histIdx])
		return true
	}
	if m.histIdx >= len(m.history) {
		return false
	}
	m.histIdx++
	if m.histIdx == len(m.history) {
		m.setComposer(m.histDraft)
		return true
	}
	m.setComposer(m.history[m.histIdx])
	return true
}

// recordHistory appends a sent prompt, collapsing an immediate repeat the way a
// shell does, and rewrites the per-project file. Persistence is best effort
// (see saveHistory) — a session with nowhere to write still recalls in-memory.
func (m *chatModel) recordHistory(s string) {
	if s == "" {
		return
	}
	if n := len(m.history); n > 0 && m.history[n-1] == s {
		return
	}
	m.history = append(m.history, s)
	if len(m.history) > maxHistoryEntries {
		m.history = m.history[len(m.history)-maxHistoryEntries:]
	}
	saveHistory(m.histPath, m.history)
}

// onSearchKey drives the ctrl+r reverse search over history: enter accepts the
// current match into the composer, esc leaves the composer untouched, and a
// further ctrl+r steps to the next older match. The query and match are shown
// on the row above the input (see searchBar).
func (m *chatModel) onSearchKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.rsearch = false
		return m, nil
	case "enter":
		if hit := m.searchHit(); hit != "" {
			m.setComposer(hit)
		}
		m.rsearch = false
		return m, nil
	case "ctrl+r":
		if m.searchHitAt(m.rsearchN+1) != "" {
			m.rsearchN++
		}
		return m, nil
	case "backspace":
		if r := []rune(m.rsearchQ); len(r) > 0 {
			m.rsearchQ = string(r[:len(r)-1])
			m.rsearchN = 0
		}
		return m, nil
	}
	if msg.Text != "" {
		m.rsearchQ += msg.Text
		m.rsearchN = 0 // a narrower query starts again from the newest match
	}
	return m, nil
}

// searchHitAt returns the (n+1)-th newest history entry containing the search
// query, case-insensitively, or "" when there aren't that many matches.
func (m *chatModel) searchHitAt(n int) string {
	seen := 0
	q := strings.ToLower(m.rsearchQ)
	for i := len(m.history) - 1; i >= 0; i-- {
		if q != "" && !strings.Contains(strings.ToLower(m.history[i]), q) {
			continue
		}
		if seen == n {
			return m.history[i]
		}
		seen++
	}
	return ""
}

// searchHit is the entry ctrl+r has currently landed on.
func (m *chatModel) searchHit() string { return m.searchHitAt(m.rsearchN) }

// onPaste puts a paste into the composer, collapsing a big one to a chip that
// expands again on submit so a wall of pasted text can't swallow the screen.
func (m *chatModel) onPaste(text string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == "" {
		return
	}
	if !shouldChipPaste(text) {
		m.ta.InsertString(text)
		return
	}
	m.pastes = append(m.pastes, text)
	m.ta.InsertString(pasteChipLabel(len(m.pastes), pasteLineCount(text)))
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

// helpText lists every slash command; shared by /help's overlay and the
// --help usage text (args.go) so the two can't drift out of sync in spirit,
// though they're rendered differently (plain vs styled).
var helpText = []struct{ cmd, desc string }{
	{"/resume", "search and resume a past conversation (with a live preview)"},
	{"/clear", "start a fresh conversation, clearing this transcript"},
	{"/compact", "compact this conversation's history now"},
	{"/rename <name>", "name this conversation (shown wherever it's listed)"},
	{"/export", "save this transcript to a file"},
	{"/status", "show the current project, conversation, and turn count"},
	{"/help", "show this list"},
	{"/quit, /exit", "leave"},
}

// composerHelpText documents the composer's keys for the /help overlay. The
// ↑/↓ rule needs three rows because it is genuinely three-way: history first,
// then the cursor once there is text to move through, then the transcript when
// there is no history left to recall.
var composerHelpText = []struct{ keys, desc string }{
	{"enter", "send the message — or queue it while an answer is streaming"},
	{"shift+enter, ctrl+j, alt+enter", "new line without sending (a trailing \\ before enter does too)"},
	{"esc", "interrupt the running turn — a queue is dropped first"},
	{"ctrl+c", "interrupt, else clear the draft, else leave (twice = leave)"},
	{"ctrl+d", "leave, when the composer is empty"},
	{"↑/↓", "recall past prompts while the composer is one line and unedited"},
	{"", "…move the cursor once it has several lines or you have edited it"},
	{"", "…scroll the transcript when there is no history left to recall"},
	{"ctrl+r", "search past prompts (enter accepts, esc cancels)"},
	{"paste", "over 3 lines or 800 characters collapses to a chip, expanded on send"},
	{"pgup/pgdn, mouse wheel", "scroll the transcript · esc jumps back to the latest when idle"},
	{"?", "show this help (on an empty composer; any key dismisses it)"},
}

// helpKeyColumnWidth lines the composer help's descriptions up in a column.
var helpKeyColumnWidth = func() int {
	w := 0
	for _, h := range composerHelpText {
		if n := lipgloss.Width(h.keys); n > w {
			w = n
		}
	}
	return w
}()

// padHelpKeys right-pads a composer help key column entry.
func padHelpKeys(keys string) string {
	if pad := helpKeyColumnWidth - lipgloss.Width(keys); pad > 0 {
		return keys + strings.Repeat(" ", pad)
	}
	return keys
}

// commandArg matches text against a slash command that takes the rest of the
// line as its argument. It reports whether the command was typed at all —
// bare, or with an argument — so the caller can answer "/rename" on its own
// with what it wants rather than sending it as a message.
func commandArg(text, command string) (arg string, ok bool) {
	if text == command {
		return "", true
	}
	if rest, found := strings.CutPrefix(text, command+" "); found {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

// commandNames is every literal slash command the input recognizes, for
// autocomplete matching — helpText groups /quit and /exit into one display
// row, but they're two distinct completable commands here.
var commandNames = []string{"/resume", "/clear", "/compact", "/rename", "/export", "/status", "/help", "/quit", "/exit"}

// commandDescriptions is what the suggestion list shows next to each command
// (see suggestionBar); unlike helpText it keys /quit and /exit separately
// since they're independently completable here.
var commandDescriptions = map[string]string{
	"/resume":  "search and resume a past conversation (with a live preview)",
	"/clear":   "start a fresh conversation, clearing this transcript",
	"/compact": "compact this conversation's history now",
	"/rename":  "name this conversation: /rename <name>",
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

// currentSuggestions returns the slash-command completions for the composer's
// current value, or nil when there's nothing to suggest (more than one line,
// not a "/" prefix, no matches, or already an exact command). Used by both the
// suggestion bar (View) and the tab/enter/up/down key handling (onKey), so they
// always agree on what's being offered.
func (m *chatModel) currentSuggestions() []string {
	if m.ta.LineCount() > 1 {
		// A command is a single line; a multi-line draft that happens to start
		// with "/" is a message, and ↑/↓ there belong to the cursor.
		return nil
	}
	text := strings.TrimSpace(m.ta.Value())
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

// composerBar renders the one variable-height region between the transcript
// and the input box: the queued-message list, then exactly one of
// slash-command suggestions (while a command is being typed), the ctrl+r
// search line, the paste chip legend, or the blank row suggestionBar returns.
// It is always composerBarRows() lines tall, which is what View sizes the
// viewport around.
func (m *chatModel) composerBar() string {
	rows := m.queuedBar()
	if len(m.currentSuggestions()) == 0 {
		switch {
		case m.rsearch:
			return strings.Join(append(rows, m.searchBar()), "\n")
		case len(m.pastes) > 0:
			return strings.Join(append(rows, m.chipBar()), "\n")
		}
	}
	return strings.Join(append(rows, m.suggestionBar()), "\n")
}

// maxQueuedMessages caps how many messages can wait behind a running turn.
// Past it the composer keeps the text, so the cap never eats a message.
const maxQueuedMessages = 5

// queuedBar lists the messages waiting behind the running turn, one row each,
// cut to a first line so a multi-line draft still takes a single row.
func (m *chatModel) queuedBar() []string {
	lines := make([]string, 0, len(m.queued)+1)
	for _, q := range m.queued {
		first, _, multiline := strings.Cut(q, "\n")
		if multiline {
			first += "…"
		}
		lines = append(lines, m.st.subtle.MaxWidth(m.contentWidth()).Render("↳ queued: "+first))
	}
	return lines
}

// searchBar is the ctrl+r filter line: the query and the best match so far.
func (m *chatModel) searchBar() string {
	hit := m.searchHit()
	if hit == "" {
		hit = "(no match)"
	}
	line := m.st.you.Render("(reverse-i-search)`"+m.rsearchQ+"`: ") +
		m.st.hint.Render(strings.ReplaceAll(hit, "\n", " ⏎ "))
	return lipgloss.NewStyle().MaxWidth(m.contentWidth()).Render(line)
}

// chipBar names the paste chips currently sitting in the composer. They are
// shown here rather than tinted in place because the textarea styles its whole
// buffer as one run, so a substring of it can't carry its own color.
func (m *chatModel) chipBar() string {
	labels := make([]string, 0, len(m.pastes))
	for i, p := range m.pastes {
		labels = append(labels, pasteChipLabel(i+1, pasteLineCount(p)))
	}
	return m.st.subtle.MaxWidth(m.contentWidth()).Render(strings.Join(labels, " ") + "  expands on send")
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
	b.WriteString("\n" + m.st.header.Render("composing") + "\n")
	for _, h := range composerHelpText {
		b.WriteString("  " + m.st.you.Render(padHelpKeys(h.keys)) + "  " + m.st.subtle.Render(h.desc) + "\n")
	}
	b.WriteString("\n" + m.st.hint.Render("ctrl+o expands or folds the tool activity shown with each answer"))
	b.WriteString("\n" + m.st.hint.Render("mouse reporting is on, so selecting text to copy needs option (macOS) or shift held"))
	b.WriteString("\n\n" + m.st.hint.Render("any key to dismiss"))

	return m.newView(m.st.box.Render(b.String()))
}

// statusView renders the /status overlay: the current session's project,
// conversation name and id, and turn count. Dismissed by any keypress (see
// onKey).
func (m *chatModel) statusView() tea.View {
	ctx := m.contextID
	if ctx == "" {
		ctx = "(none yet — starts on your first message)"
	}
	name := m.convName
	if name == "" {
		name = "(unnamed — /rename <name>)"
	}
	var b strings.Builder
	b.WriteString(m.st.header.Render("session status"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "  %s  %s\n", m.st.you.Render("project"), m.project)
	fmt.Fprintf(&b, "  %s     %s\n", m.st.you.Render("name"), name)
	fmt.Fprintf(&b, "  %s  %s\n", m.st.you.Render("context"), ctx)
	fmt.Fprintf(&b, "  %s  %d\n", m.st.you.Render("turns"), len(m.raw))
	b.WriteString("\n" + m.st.hint.Render("any key to dismiss"))

	return m.newView(m.st.box.Render(b.String()))
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
		return m.newView(m.st.box.Render("starting patch chat…"))
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
	// One blank line between each major region for an even vertical rhythm:
	// header → blank → transcript → gap/suggestions → bordered input → footer.
	// Suggestions sit directly above the box since they're about what you're
	// typing; the state/badges line reads as a footer below the box instead of
	// a caption above it. The suggestion block grows downward (vertically, so
	// ↑/↓ tracks it) up to maxSuggestionRows; the input box grows the same way
	// up to maxComposerRows. The viewport shrinks by whatever the two take, so
	// total layout height stays constant instead of pushing the input
	// off-screen.
	if h := m.viewportHeight(); h != m.vp.Height() {
		m.vp.SetHeight(h)
		if m.follow {
			m.vp.GotoBottom()
		} else {
			m.vp.SetYOffset(m.vp.YOffset()) // SetHeight doesn't re-clamp; this does
		}
	}
	gap := m.composerBar()
	body := strings.Join([]string{
		m.st.header.Render("patch") + m.st.subtle.Render("  ·  project "+m.project),
		"", // gap below the header
		m.vp.View(),
		gap,
		m.st.inputBox.Render(m.ta.View()),
		m.footer(),
	}, "\n")

	return m.newView(m.st.box.Render(body))
}

// ── turn and session feedback ─────────────────────────────────

// startedWorking stamps the turn's start when a handler has just entered the
// working state. It is called from Update around the two handlers that can
// enter it, so submit/stream stay untouched by the timing.
func (m *chatModel) startedWorking(before bool) {
	if !before && m.working {
		m.turnStart = time.Now()
	}
}

// windowTitle names the terminal window (see newView), so a chat left in a
// background tab still says whether its turn is still running.
func (m *chatModel) windowTitle() string {
	if m.working {
		return "patch · working…"
	}
	if m.project == "" {
		return "patch"
	}
	return "patch · " + m.project
}

// footer composes the single line under the composer: the session state on the
// left (spinner and a live counter while a turn runs, else the key hint) and
// the session badges right-aligned against contentWidth. It must stay one line
// — viewportHeight budgets exactly one row for it.
func (m *chatModel) footer() string {
	var left string
	switch {
	case m.ctrlCArmed():
		// A ctrl+c that did something other than leave has to say how to leave.
		left = m.st.hint.Render("ctrl+c again to exit")
	case m.working:
		left = m.sp.View() + " " + m.st.subtle.Render("thinking…"+m.workingFor()) +
			m.st.hint.Render("  ·  esc interrupts · type to queue")
	default:
		left = m.st.hint.Render("enter send · / commands · ? keys · ctrl+c clears, twice leaves")
	}
	right := m.st.hint.Render(strings.Join(m.sessionBadges(), " · "))
	w := m.contentWidth()
	// Two columns of breathing room, or the badges are dropped: the state is
	// what the user needs here, and the badges repeat what /status shows.
	if pad := w - lipgloss.Width(left) - lipgloss.Width(right); pad >= 2 {
		return left + strings.Repeat(" ", pad) + right
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(left)
}

// workingFor is the " 12s" the footer counts up during a turn, empty when
// there was no start to measure from (a turn adopted mid-flight in a test).
func (m *chatModel) workingFor() string {
	if m.turnStart.IsZero() {
		return ""
	}
	return " " + formatWorkedFor(time.Since(m.turnStart))
}

// sessionBadges are the right-hand side of the footer: which conversation this
// is, how far into it we are, and — only when it applies — that the transcript
// is scrolled off the bottom, with the way back.
func (m *chatModel) sessionBadges() []string {
	var out []string
	if label := m.sessionLabel(); label != "" {
		out = append(out, label)
	}
	if n := m.turnCount(); n > 0 {
		out = append(out, countLabel(n, "turn"))
	}
	if !m.follow {
		out = append(out, fmt.Sprintf("scrolled · %d%% · esc for the latest", int(m.vp.ScrollPercent()*100)))
	}
	return out
}

// sessionLabel names the conversation in a footer-sized space: its name when
// it has one, else a short id, else nothing (there is no conversation yet).
func (m *chatModel) sessionLabel() string {
	if m.convName != "" {
		return m.convName
	}
	return shortContextID(m.contextID)
}

// shortContextID trims a context id (a UUID) to a recognizable prefix — enough
// to tell two conversations apart without spending the footer's width on 36
// characters.
func shortContextID(id string) string {
	if r := []rune(id); len(r) > 8 {
		return string(r[:8])
	}
	return id
}

// turnCount is how many prompts this session has sent — one per exchange,
// which is what "3 turns" means to a reader. m.raw holds a row per message
// (plus system notes), so it can't be counted directly.
func (m *chatModel) turnCount() int {
	n := 0
	for _, t := range m.raw {
		if t.role == "user" {
			n++
		}
	}
	return n
}

// closeOutTurn appends the end-of-turn summary under the block that was just
// finalized (rather than as a block of its own, so it reads as that turn's
// footer) and records it in the transcript so /export carries it too.
func (m *chatModel) closeOutTurn(tools int) {
	if len(m.turns) == 0 {
		return
	}
	line := m.endOfTurnLine(tools)
	m.turns[len(m.turns)-1] += "\n" + m.st.subtle.Render(line)
	m.raw = append(m.raw, transcriptTurn{role: "system", content: line})
}

// endOfTurnLine words the summary: "✻ Worked for 23s · 3 tools · done 6:05 PM".
// The tools segment is dropped when nothing ran, and the elapsed one when the
// turn's start was never stamped, leaving the wall-clock time as the one thing
// always worth saying.
func (m *chatModel) endOfTurnLine(tools int) string {
	var parts []string
	if !m.turnStart.IsZero() {
		parts = append(parts, "Worked for "+formatWorkedFor(time.Since(m.turnStart)))
	}
	if tools > 0 {
		parts = append(parts, countLabel(tools, "tool"))
	}
	parts = append(parts, "done "+time.Now().Format("3:04 PM"))
	return "✻ " + strings.Join(parts, " · ")
}

// formatWorkedFor renders a turn's duration at human glance precision: whole
// seconds ("23s"), minutes and padded seconds past a minute ("1m 06s"). Tool
// calls use formatElapsed instead — tenths matter there, not here.
func formatWorkedFor(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm %02ds", secs/60, secs%60)
}

// countLabel words a count with its unit ("1 tool", "3 tools").
func countLabel(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// compactionRule is the divider that marks folded history — a subtle rule with
// the label centred in it, the shape other agents use, so a compaction reads as
// a seam in the transcript rather than as something the assistant said.
func (m *chatModel) compactionRule() string {
	const label = " history compacted "
	fill := m.contentWidth() - lipgloss.Width(label)
	if fill < 2 {
		return m.st.subtle.Render(strings.TrimSpace(label))
	}
	left := fill / 2
	return m.st.subtle.Render(strings.Repeat("─", left) + label + strings.Repeat("─", fill-left))
}

// recapLine closes a resumed transcript with where the user has landed: which
// conversation, how much of it there is, and how stale it is.
func (m *chatModel) recapLine(items []assistantv1alpha1.ConversationMessage) string {
	label := m.sessionLabel()
	if label == "" {
		label = "conversation"
	}
	parts := []string{"resumed " + label, countLabel(len(items), "message")}
	if n := len(items); n > 0 {
		if phrase := lastActivePhrase(items[n-1].CreatedAt.Time); phrase != "" {
			parts = append(parts, phrase)
		}
	}
	return m.st.subtle.Render(strings.Join(parts, " · "))
}

// lastActivePhrase words a timestamp as "last active 3h ago", or "last active
// on 2026-01-02" once ago() has fallen back to a date (the picker's rowAge
// words its own column the same way). An unset timestamp has nothing to say.
func lastActivePhrase(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	a := ago(t)
	if strings.Contains(a, "-") {
		return "last active on " + a
	}
	return "last active " + a + " ago"
}

// notifyMode is how a finished turn announces itself. Bell-only is the default:
// every terminal has a bell (even if the user has muted it), while OSC 9 is
// ignored by terminals that don't implement it and can raise an OS banner in
// the ones that do — so the desktop notification is opt-in.
type notifyMode int

const (
	notifyBell    notifyMode = iota // the terminal bell alone
	notifyOff                       // say nothing
	notifyDesktop                   // bell plus an OSC 9 desktop notification
)

// parseNotifyMode reads PATCH_NOTIFY. Anything unrecognized (including an
// unset variable) is the default — a typo'd env var must not cost a chat.
func parseNotifyMode(s string) notifyMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none":
		return notifyOff
	case "desktop":
		return notifyDesktop
	default:
		return notifyBell
	}
}

// notifyDone tells a user who has looked away that their turn is finished.
// tea.Raw writes through the program's own output, which is the only correct
// way to emit an escape sequence while the renderer owns the terminal.
func (m *chatModel) notifyDone() tea.Cmd {
	switch m.notify {
	case notifyOff:
		return nil
	case notifyDesktop:
		return tea.Raw("\a" + ansi.Notify("Patch finished"))
	default:
		return tea.Raw("\a")
	}
}

// submit records the user's line, starts the spinner, and launches the
// streaming turn under a context of its own (see [chatModel.interrupt]). It
// returns the spinner tick command; answer chunks arrive asynchronously via
// p.Send from the goroutine started here.
func (m *chatModel) submit(text string) tea.Cmd {
	m.turns = append(m.turns, m.turnBlock(m.st.you.Render("You"), m.st.userText.Width(m.contentWidth()).Render(text)))
	m.raw = append(m.raw, transcriptTurn{role: "user", content: text})
	m.answer.Reset()
	m.activity = nil
	m.taskID = ""
	m.working = true
	m.follow = true
	m.rebuildViewport()

	// Each turn streams under its own cancellable context, so esc can stop
	// this turn without tearing down the session's.
	ctx, cancel := context.WithCancel(m.baseContext())
	m.cancelTurn = cancel
	go m.stream(ctx, m.turnGen, text, m.contextID)
	return m.sp.Tick
}

// stream runs one turn against the a2a client, forwarding events to the model
// via p.Send. It runs in its own goroutine (a Cmd yields a single message,
// whereas a turn produces many), which is the supported Bubble Tea pattern for
// long-lived producers.
func (m *chatModel) stream(ctx context.Context, gen int, text, contextID string) {
	req := &a2a.SendMessageRequest{Message: buildMessage(text, m.project, contextID)}
	events := m.client.SendStreamingMessage(ctx, req)
	seen := contextID
	var task, sentTask a2a.TaskID
	sentCtx := contextID
	for ev, err := range events {
		if err != nil {
			m.prog.Send(streamErrMsg{gen: gen, err: err})
			return
		}
		if id := eventContextID(ev); id != "" {
			seen = id
		}
		if id := ev.TaskInfo().TaskID; id != "" {
			task = id
		}
		if task != sentTask || seen != sentCtx {
			sentTask, sentCtx = task, seen
			m.prog.Send(streamIDsMsg{gen: gen, taskID: task, contextID: seen})
		}
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			if t := textOf(e.Artifact.Parts); t != "" {
				m.prog.Send(streamChunkMsg{gen: gen, text: t})
			}
		case *a2a.Message:
			if t := textOf(e.Parts); t != "" {
				m.prog.Send(streamChunkMsg{gen: gen, text: t})
			}
		case *a2a.TaskStatusUpdateEvent:
			// Tool activity rides on working-state updates (see
			// internal/a2a/activity.go); everything else is a state change.
			if act, ok := toolActivityFrom(e.Status.Message); ok {
				m.prog.Send(streamActivityMsg{gen: gen, act: act})
				continue
			}
			if e.Status.State == a2a.TaskStateFailed {
				note := ""
				if e.Status.Message != nil {
					note = textOf(e.Status.Message.Parts)
				}
				m.prog.Send(streamDoneMsg{gen: gen, contextID: seen, failed: true, failMsg: note})
				return
			}
		}
	}
	m.prog.Send(streamDoneMsg{gen: gen, contextID: seen})
}

// interruptedMarker ends a turn the user stopped. It is rendered subtle, not
// in the error style: an interrupt is something the user did, not a failure.
const interruptedMarker = "⏹ interrupted"

// cancelTaskTimeout bounds the best-effort task cancellation an interrupt
// sends; it runs off the UI path, so a slow service can never stall a keypress.
const cancelTaskTimeout = 10 * time.Second

// baseContext is the session's context, or a background one for a model built
// without one (the tests that drive Update directly).
func (m *chatModel) baseContext() context.Context {
	if m.ctx == nil {
		return context.Background()
	}
	return m.ctx
}

// interruptible reports whether there is a streaming turn to stop. /compact
// and /rename also set working, but have no turn context behind them.
func (m *chatModel) interruptible() bool { return m.working && m.cancelTurn != nil }

// interrupt abandons the running turn: whatever text arrived is kept and
// finalized with interruptedMarker, running tool rows are closed out, and the
// generation is bumped so the goroutine's remaining messages are dropped
// rather than bleeding into the next turn. The service is told to drop the
// task too, in the background — the local turn is over either way.
func (m *chatModel) interrupt() {
	m.turnGen++
	task := m.taskID
	m.stopRunning()

	answer := strings.TrimRight(m.renderMarkdown(m.answer.String()), "\n")
	if answer != "" {
		answer += "\n"
	}
	m.turns = append(m.turns, m.turnBlock(m.st.patch.Render("Patch"), answer+m.st.subtle.Render(interruptedMarker)))
	m.keepActivity(len(m.turns) - 1)
	rawAnswer := strings.TrimRight(m.answer.String(), "\n")
	if rawAnswer != "" {
		rawAnswer += "\n"
	}
	m.raw = append(m.raw, transcriptTurn{role: "assistant", content: rawAnswer + interruptedMarker})
	m.answer.Reset()
	m.endTurn()
	m.follow = true
	m.rebuildViewport()

	if task != "" && m.client != nil {
		go m.cancelTask(task)
	}
}

// endTurn clears the per-turn state a finished or abandoned turn leaves
// behind. Cancelling even a completed turn's context is what releases it.
func (m *chatModel) endTurn() {
	m.working = false
	m.taskID = ""
	if m.cancelTurn != nil {
		m.cancelTurn()
		m.cancelTurn = nil
	}
}

// stopRunning closes out the rows still in flight when a turn is abandoned, so
// the transcript stops counting them up and nothing keeps re-rendering.
func (m *chatModel) stopRunning() {
	for i := range m.activity {
		if r := &m.activity[i]; !r.done {
			r.done, r.stopped, r.elapsed = true, true, time.Since(r.started)
		}
	}
}

// cancelTask asks the service to stop a task we have stopped watching. It is
// best effort: the turn is already finalized locally, and a service that never
// hears about it simply finishes the work unobserved.
func (m *chatModel) cancelTask(id a2a.TaskID) {
	ctx, cancel := context.WithTimeout(m.baseContext(), cancelTaskTimeout)
	defer cancel()
	_, _ = m.client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: id})
}

// enqueue holds a message typed during a running turn. A full queue leaves the
// text in the composer instead, so the cap can never swallow a message.
func (m *chatModel) enqueue(text string) {
	if len(m.queued) >= maxQueuedMessages {
		return
	}
	m.queued = append(m.queued, text)
	m.recordHistory(text)
	m.resetComposer()
}

// drainQueue starts the oldest queued message as its own turn, so a queue is
// sent one turn at a time rather than glued into a single prompt.
func (m *chatModel) drainQueue() tea.Cmd {
	if len(m.queued) == 0 || m.working {
		return nil
	}
	next := m.queued[0]
	m.queued = m.queued[1:]
	return m.submit(next)
}

// ctrlCExitWindow is how long after a ctrl+c a second press leaves outright,
// whatever the first one did.
const ctrlCExitWindow = 2 * time.Second

// ctrlCArmed reports whether a second ctrl+c would leave right now.
func (m *chatModel) ctrlCArmed() bool {
	return !m.ctrlCAt.IsZero() && time.Since(m.ctrlCAt) < ctrlCExitWindow
}

// onCtrlC is the three-way ctrl+c: stop the running turn, else clear an unsent
// draft, else leave. Anything but leaving arms ctrlCExitWindow, so the plain
// escape hatch is always one more press away.
func (m *chatModel) onCtrlC() (tea.Model, tea.Cmd) {
	if m.ctrlCArmed() {
		return m, tea.Quit
	}
	switch {
	case m.interruptible():
		m.ctrlCAt = time.Now()
		m.queued = nil // one "stop" gesture stops everything pending
		m.interrupt()
		return m, nil
	case m.rsearch || m.ta.Value() != "":
		m.ctrlCAt = time.Now()
		m.rsearch = false
		m.resetComposer()
		return m, nil
	}
	return m, tea.Quit
}

// compact runs one manual "/compact" request against POST /v1/compact — a
// plain REST call, not the a2a client, since there is no message to answer —
// and reports the outcome back via p.Send, the same producer-in-a-goroutine
// pattern [chatModel.stream] uses.
func (m *chatModel) compact() {
	err := requestCompact(m.ctx, m.baseURL, m.token, m.project, m.contextID)
	m.prog.Send(compactDoneMsg{err: err})
}

// startRename validates "/rename <name>" and kicks off the request. The two
// ways to get it wrong — no name, or no conversation to name yet — are
// answered in the transcript rather than by a request the service would only
// reject, since neither needs the server to know it is wrong.
func (m *chatModel) startRename(name string) tea.Cmd {
	switch {
	case name == "":
		m.turns = append(m.turns, m.st.subtle.Render("usage: /rename <name>"))
	case len([]rune(name)) > maxConversationNameLen:
		m.turns = append(m.turns, m.st.subtle.Render(fmt.Sprintf("name is too long (max %d characters)", maxConversationNameLen)))
	case m.contextID == "":
		m.turns = append(m.turns, m.st.subtle.Render("nothing to rename — no conversation yet"))
	default:
		m.working = true
		m.rebuildViewport()
		go m.rename(name)
		return m.sp.Tick
	}
	m.rebuildViewport()
	return nil
}

// rename runs one "/rename" request against POST /v1/conversations/rename, the
// same plain-REST, producer-in-a-goroutine shape [chatModel.compact] uses.
func (m *chatModel) rename(name string) {
	err := requestRename(m.ctx, m.baseURL, m.token, m.project, m.contextID, name)
	m.prog.Send(renameDoneMsg{name: name, err: err})
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

// composerBarRows is the whole height of the region between the transcript and
// the input box: one row per queued message, plus the suggestion/search/chip
// bar's own rows.
func (m *chatModel) composerBarRows() int { return len(m.queued) + m.suggestionRows() }

// composerRows is how many text rows the input box currently needs: the
// textarea's own dynamic height, clamped to what the layout budgets for it.
func (m *chatModel) composerRows() int {
	h := m.ta.Height()
	if h < 1 {
		return 1
	}
	if h > maxComposerRows {
		return maxComposerRows
	}
	return h
}

// viewportHeight computes the transcript viewport's height from the last
// known terminal height, chrome (outer box padding, header, header gap,
// status line, input border), and the two variable blocks under it: the
// queued/suggestion/search/chip rows and the composer itself.
func (m *chatModel) viewportHeight() int {
	// outer box padding (2) + header (1) + header gap (1) + status (1) +
	// input box border (2 = top/bottom) = 7 rows of fixed chrome.
	h := m.termHeight - 7 - m.composerRows() - m.composerBarRows()
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
	// Composer width: content width minus the input box's border (2 cols) and
	// inner padding (2 cols), so the box stays within the content column.
	// textarea.SetWidth accounts for its own "> " prompt on top of that.
	taWidth := cw - 4
	if taWidth < 10 {
		taWidth = 10
	}
	// Width first: it re-wraps the composer, which is what its height (and so
	// the viewport's) is computed from.
	m.ta.SetWidth(taWidth)
	m.vp.SetWidth(cw)
	m.vp.SetHeight(m.viewportHeight())
	m.renderer = newMarkdownRenderer(cw, m.dark) // glamour wrap stays at content width
	m.ready = true
	m.follow = true // the resize re-wraps every line, so the old offset is meaningless
	m.rebuildViewport()

	if !m.sentFirst && m.firstMessage != "" {
		m.sentFirst = true
		return m.submit(m.firstMessage)
	}
	if r := m.resumeOnStart; r != "" {
		m.resumeOnStart = ""
		if r == pickerOnStart {
			return m.openPicker()
		}
		return m.resumeDirect(r)
	}
	return nil
}

// applyActivity folds one tool-activity update into the in-progress turn's
// rows: a started call appends a row, a finished one closes the matching row
// out. Calls are matched by id, falling back to the newest unfinished row with
// the same name for providers that assign no tool-call id.
func (m *chatModel) applyActivity(act toolActivity) {
	if act.started() {
		m.activity = append(m.activity, activityRow{
			id: act.ID, name: act.Name, summary: act.Summary, started: time.Now(),
		})
		return
	}
	for i := len(m.activity) - 1; i >= 0; i-- {
		row := &m.activity[i]
		if row.done {
			continue
		}
		if (act.ID != "" && row.id == act.ID) || (act.ID == "" && row.name == act.Name) {
			row.done, row.ok, row.elapsed = true, act.OK, act.elapsed()
			return
		}
	}
	// A finish with no matching start (a reconnect, or a dropped event) still
	// belongs in the transcript.
	m.activity = append(m.activity, activityRow{
		id: act.ID, name: act.Name, summary: act.Summary,
		done: true, ok: act.OK, elapsed: act.elapsed(),
	})
}

// keepActivity moves the finished turn's rows onto the turn at index i, so the
// transcript keeps showing what that answer did.
func (m *chatModel) keepActivity(i int) {
	rows := m.activity
	m.activity = nil
	if len(rows) == 0 || i < 0 {
		return
	}
	if m.turnActivity == nil {
		m.turnActivity = map[int][]activityRow{}
	}
	m.turnActivity[i] = rows
}

// hasRunning reports whether any row is still in flight.
func hasRunning(rows []activityRow) bool {
	for _, r := range rows {
		if !r.done {
			return true
		}
	}
	return false
}

// activityLines renders one turn's rows as collapsed one-line entries, in the
// style of Claude Code and Codex: a bullet, a verb, and the tool's name.
// folded (an answer is already streaming, or the turn is finalized) reduces a
// run of completed calls to a single count line — ctrl+o expands it back to a
// line per call, with each call's argument summary.
func (m *chatModel) activityLines(rows []activityRow, folded bool) []string {
	if len(rows) == 0 {
		return nil
	}
	if folded && !m.expandActivity && len(rows) > 1 && !hasRunning(rows) {
		var total time.Duration
		for _, r := range rows {
			total += r.elapsed
		}
		return []string{m.st.subtle.Render(fmt.Sprintf("• Called %d tools · %s", len(rows), formatElapsed(total)))}
	}

	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		switch {
		case !r.done:
			lines = append(lines, m.st.patch.Render(fmt.Sprintf("• Running %s… %s", r.name, formatElapsed(time.Since(r.started)))))
		case r.stopped:
			lines = append(lines, m.st.subtle.Render(fmt.Sprintf("• Stopped %s · %s", r.name, formatElapsed(r.elapsed))))
		case !r.ok:
			lines = append(lines, m.st.err.Render(fmt.Sprintf("• Failed %s · %s", r.name, formatElapsed(r.elapsed))))
		default:
			line := fmt.Sprintf("• Ran %s · %s", r.name, formatElapsed(r.elapsed))
			if m.expandActivity && r.summary != "" {
				line += "  " + r.summary
			}
			lines = append(lines, m.st.subtle.Render(line))
		}
	}
	return lines
}

// withActivity slots activity rows into a rendered turn block, just under the
// speaker label so they read as part of that turn (turnBlock lays a block out
// as label\nbody). A label-less block — a bare error line — takes them above.
func withActivity(block string, lines []string) string {
	if len(lines) == 0 {
		return block
	}
	rows := strings.Join(lines, "\n")
	label, body, ok := strings.Cut(block, "\n")
	if !ok {
		return rows + "\n" + block
	}
	return label + "\n" + rows + "\n" + body
}

// turnBlock lays out one turn identically for both speakers: a bold colored
// label line, then the body on the following line(s), both flush-left at the
// same column. Consecutive turns are separated by exactly one blank line (see
// rebuildViewport).
func (m *chatModel) turnBlock(label, body string) string {
	return label + "\n" + body
}

// rebuildViewport composes the transcript (finalized turns plus any in-progress
// answer) into the viewport, one blank line between turns. It only pins to the
// bottom while following; if the user has scrolled up, their position is kept
// so streamed chunks accumulate below instead of dragging the view down.
func (m *chatModel) rebuildViewport() {
	blocks := make([]string, 0, len(m.turns)+1)
	for i, turn := range m.turns {
		// Activity rows sit above the answer they produced; they are rendered
		// here rather than baked into the turn so ctrl+o can re-fold them.
		blocks = append(blocks, withActivity(turn, m.activityLines(m.turnActivity[i], true)))
	}
	if m.working {
		ans := m.renderMarkdown(m.answer.String())
		if strings.TrimSpace(ans) == "" {
			ans = m.st.subtle.Render("…")
		}
		// Fold only once the answer has started: while tools are the only thing
		// happening, the rows ARE the progress the user is watching.
		lines := m.activityLines(m.activity, m.answer.Len() > 0)
		blocks = append(blocks, withActivity(m.turnBlock(m.st.patch.Render("Patch"), ans), lines))
	}
	m.vp.SetContent(strings.Join(blocks, "\n\n"))
	if m.follow {
		m.vp.GotoBottom()
	} else {
		// SetContent keeps yOffset as-is; re-set it so the viewport re-clamps
		// against the new line count (SetYOffset clamps, plain assignment
		// wouldn't).
		m.vp.SetYOffset(m.vp.YOffset())
	}
}

// scrollBy runs a viewport movement and re-derives follow from where it landed:
// scrolling off the bottom stops auto-follow, scrolling back to it resumes.
// Every scroll path (keys and wheel) goes through here so the two can't drift.
func (m *chatModel) scrollBy(move func()) {
	move()
	m.follow = m.vp.AtBottom()
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
