package a2a

import (
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// projectNameKey is the message-metadata extension carrying the Milo project the
// task runs against. Preserved from the TS service (message.metadata.projectName,
// with a params.metadata.projectName fallback).
const projectNameKey = "projectName"

// ProjectName extracts the Milo project name from an A2A message's metadata,
// falling back to the request-level metadata. Returns "" when absent.
func ProjectName(msg *a2a.Message, paramsMeta map[string]any) string {
	if msg != nil {
		if v := metaString(msg.Metadata, projectNameKey); v != "" {
			return v
		}
	}
	return metaString(paramsMeta, projectNameKey)
}

// UserText concatenates the text parts of an A2A message (space-joined,
// trimmed), matching the TS validateMessageParams. Non-text parts are ignored.
func UserText(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	var parts []string
	for _, p := range msg.Parts {
		if t := p.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
