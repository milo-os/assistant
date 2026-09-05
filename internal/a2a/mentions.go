package a2a

import (
	"encoding/json"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// mentionsKey is the message-metadata extension carrying the resources the user
// pointed at with "@kind/name" in the chat. It sits beside projectName, and
// like it is advisory: the text already contains the literal mention, so a
// message that arrives without this list still reads correctly.
const mentionsKey = "mentions"

// Limits on the metadata. It arrives from a client over the wire, so it is
// treated as untrusted input: a long list, or a long field inside it, would
// otherwise end up verbatim in a model prompt.
const (
	maxMentions      = 20
	maxMentionField  = 253 // an RFC 1123 object name, the longest legitimate field
	maxMentionsBytes = 16 << 10
)

// Mention is one resource the user referenced in their message. APIGroup is
// present when the client resolved it from API discovery and empty otherwise.
type Mention struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	APIGroup string `json:"apiGroup,omitempty"`
}

// Mentions extracts the referenced resources from an A2A message's metadata,
// falling back to the request-level metadata the way [ProjectName] does.
// Anything malformed or oversized yields nil rather than an error: a mention is
// a hint for the turn, never a reason to reject a message the user can see
// nothing wrong with.
func Mentions(msg *a2a.Message, paramsMeta map[string]any) []Mention {
	if msg != nil {
		if ms := decodeMentions(msg.Metadata); len(ms) > 0 {
			return ms
		}
	}
	return decodeMentions(paramsMeta)
}

// decodeMentions re-marshals the metadata entry and decodes it into the struct.
// The value arrives as []any of map[string]any over the wire and as the
// client's own slice in process, so going through JSON handles both without a
// type switch per shape.
func decodeMentions(meta map[string]any) []Mention {
	if meta == nil {
		return nil
	}
	raw, ok := meta[mentionsKey]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > maxMentionsBytes {
		return nil
	}
	var decoded []Mention
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil
	}

	out := make([]Mention, 0, min(len(decoded), maxMentions))
	for _, m := range decoded {
		m.Kind = clampMentionField(m.Kind)
		m.Name = clampMentionField(m.Name)
		m.APIGroup = clampMentionField(m.APIGroup)
		if m.Kind == "" || m.Name == "" {
			continue
		}
		out = append(out, m)
		if len(out) == maxMentions {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clampMentionField trims a field and drops it entirely when it is too long or
// carries anything that would break out of the single line it is rendered on.
func clampMentionField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxMentionField || strings.ContainsAny(s, "\n\r") {
		return ""
	}
	return s
}
