// The chat TUI's composer: the multi-line textarea at the bottom of the
// full-screen chat (chat_tui.go drives its keys), the per-project prompt
// history that ↑/↓ and ctrl+r walk, and the paste chips that keep a large
// bracketed paste out of the visible input until the message is sent.
//
// History lives in a plain file under the user's config dir, one prompt per
// line with newlines escaped. Everything here is best effort: a missing or
// unreadable config dir costs the session its recall, never the chat.
package patchcli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"
)

// maxComposerRows caps how tall the input box grows; the transcript viewport
// gives up exactly these rows as it does (see viewportHeight).
const maxComposerRows = 8

// maxComposerContentRows is how much text the composer will hold at all.
// textarea's MaxHeight doubles as its content guard, so without this the box
// would stop accepting input at maxComposerRows instead of scrolling.
const maxComposerContentRows = 500

// newComposer builds the multi-line input: one row that grows with the content
// up to maxComposerRows, no line numbers, and a "> " gutter on the first visual
// line only so continuations line up under the first character.
func newComposer(dark bool) textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "Message patch…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MinHeight = 1
	ta.MaxHeight = maxComposerRows
	ta.MaxContentHeight = maxComposerContentRows
	ta.DynamicHeight = true
	ta.SetVirtualCursor(true)
	// SetPromptFunc also fixes the prompt width, so it has to run before the
	// first SetWidth (in layout) for the box to size itself correctly.
	ta.SetPromptFunc(2, func(pi textarea.PromptInfo) string {
		if pi.LineNumber == 0 {
			return "> "
		}
		return "  "
	})
	ta.SetStyles(newComposerStyles(dark))
	ta.SetHeight(1)
	return ta
}

// newComposerStyles is newInputStyles' counterpart for the textarea: the same
// per-background text/placeholder/prompt/cursor colors, plus a flat cursor line
// (the bubbles default paints it with a background, which reads as a highlight
// bar inside the bordered box).
func newComposerStyles(dark bool) textarea.Styles {
	ld := lipgloss.LightDark(dark)
	text := ld(lipgloss.Color("#2A2620"), lipgloss.Color("#EAE6DA"))
	placeholder := ld(lipgloss.Color("#8B8577"), lipgloss.Color("#6B6B6B"))
	prompt := ld(lipgloss.Color("#1155CC"), lipgloss.Color("#7AB7FF")) // = you blue
	cursor := ld(lipgloss.Color("#6D28D9"), lipgloss.Color("#C084FC")) // = patch violet

	s := textarea.DefaultStyles(dark)
	for _, st := range []*textarea.StyleState{&s.Focused, &s.Blurred} {
		st.Text = st.Text.Foreground(text)
		st.CursorLine = lipgloss.NewStyle().Foreground(text)
		st.Placeholder = st.Placeholder.Foreground(placeholder)
		st.Prompt = st.Prompt.Foreground(prompt)
		st.EndOfBuffer = st.EndOfBuffer.Foreground(placeholder)
	}
	s.Cursor.Color = cursor
	return s
}

// ── prompt history ────────────────────────────────────────────

const (
	// maxHistoryEntries caps the file; recall is newest-first, so the entries
	// dropped are the ones furthest out of reach.
	maxHistoryEntries = 500
	// maxHistoryEntryBytes caps one entry, so a submitted message carrying an
	// expanded paste chip can't turn the history file into a transcript store.
	maxHistoryEntryBytes = 4096
)

// historyPath is where this project's prompts are kept. It returns "" when
// there is no config dir to write into, which leaves recall in-session only.
func historyPath(project string) string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "patch", "history-"+historyFileSlug(project)+".txt")
}

// historyFileSlug reduces a project name to something safe as a filename;
// projects that collide after this share a history file, which is harmless.
func historyFileSlug(project string) string {
	if project == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range project {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// escapeHistoryLine folds a (possibly multi-line) prompt onto one line.
func escapeHistoryLine(s string) string {
	return strings.NewReplacer("\\", `\\`, "\n", `\n`, "\r", "").Replace(s)
}

// unescapeHistoryLine is escapeHistoryLine's inverse; an unknown escape keeps
// the character that followed it rather than the backslash.
func unescapeHistoryLine(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// loadHistory reads a project's prompts oldest-first. Any failure (no path, no
// file, unreadable) is an empty history, never an error the chat has to show.
func loadHistory(path string) []string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		out = append(out, unescapeHistoryLine(line))
	}
	return out
}

// saveHistory rewrites the file with the newest maxHistoryEntries prompts. It
// is deliberately silent about failures — see loadHistory.
func saveHistory(path string, entries []string) {
	if path == "" {
		return
	}
	if len(entries) > maxHistoryEntries {
		entries = entries[len(entries)-maxHistoryEntries:]
	}
	var b strings.Builder
	for _, e := range entries {
		if len(e) > maxHistoryEntryBytes {
			// Cutting at a byte offset can split a rune; drop the fragment.
			e = strings.ToValidUTF8(e[:maxHistoryEntryBytes], "")
		}
		b.WriteString(escapeHistoryLine(e))
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
}

// ── paste chips ───────────────────────────────────────────────

const (
	// pasteChipLines / pasteChipChars are the thresholds past which a paste is
	// collapsed to a chip instead of being dropped into the composer whole.
	pasteChipLines = 3
	pasteChipChars = 800
)

// pasteChipRE matches the chip token in the composer. The line count is part of
// the label but not of the identity — the index alone resolves the paste.
var pasteChipRE = regexp.MustCompile(`\[Pasted text #(\d+) \+\d+ lines?\]`)

// pasteLineCount counts the lines a paste would occupy (a paste with no
// newline is still one line).
func pasteLineCount(s string) int {
	return strings.Count(s, "\n") + 1
}

// shouldChipPaste reports whether a paste is big enough to collapse.
func shouldChipPaste(s string) bool {
	return pasteLineCount(s) > pasteChipLines || len(s) > pasteChipChars
}

// pasteChipLabel is the token shown in the composer for paste n (1-based).
func pasteChipLabel(n, lines int) string {
	unit := "lines"
	if lines == 1 {
		unit = "line"
	}
	return fmt.Sprintf("[Pasted text #%d +%d %s]", n, lines, unit)
}

// expandPasteChips substitutes every chip token back into the text it stands
// for. A token whose index has no paste behind it (the user typed or edited one
// by hand) is left alone, as literal text.
func expandPasteChips(s string, pastes []string) string {
	if len(pastes) == 0 {
		return s
	}
	return pasteChipRE.ReplaceAllStringFunc(s, func(tok string) string {
		n, err := strconv.Atoi(pasteChipRE.FindStringSubmatch(tok)[1])
		if err != nil || n < 1 || n > len(pastes) {
			return tok
		}
		return pastes[n-1]
	})
}
