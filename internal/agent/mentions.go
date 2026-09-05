package agent

import (
	"fmt"
	"strings"
)

// Mention is one resource the user pointed at with "@kind/name" in the chat.
// It is declared here rather than reused from internal/a2a for the same reason
// [AgentRunner] lives there and not here: the orchestration layer does not
// import the transport.
type Mention struct {
	Kind     string
	Name     string
	APIGroup string
}

// maxMentionNoteEntries caps how many mentions the note names. The point is to
// tell the model what the user was pointing at, not to paginate an inventory
// into the system prompt.
const maxMentionNoteEntries = 20

// mentionNote is the one short system note a turn's mentions add: what was
// referenced, in which project, and that looking them up beats guessing. It
// deliberately grants nothing and asks for no particular tool — the model
// already has whatever the project entitles it to, and the literal "@kind/name"
// is still in the user's own message.
func mentionNote(project string, ms []Mention) string {
	labels := make([]string, 0, len(ms))
	seen := make(map[string]bool, len(ms))
	for _, m := range ms {
		kind, name := strings.TrimSpace(m.Kind), strings.TrimSpace(m.Name)
		if kind == "" || name == "" {
			continue
		}
		label := kind + "/" + name
		if group := strings.TrimSpace(m.APIGroup); group != "" {
			label += " (" + group + ")"
		}
		if seen[label] {
			continue
		}
		seen[label] = true
		labels = append(labels, label)
		if len(labels) == maxMentionNoteEntries {
			break
		}
	}
	if len(labels) == 0 {
		return ""
	}
	where := "this project"
	if p := strings.TrimSpace(project); p != "" {
		where = p
	}
	return fmt.Sprintf("The user referenced these resources in %s: %s. Prefer looking them up directly.",
		where, strings.Join(labels, ", "))
}
