// Tool-activity events: the structured "what is the assistant doing right
// now" signal clients render while a turn is in flight.
//
// They ride the protocol's existing shapes — a working-state
// TaskStatusUpdateEvent whose Status.Message carries a DataPart — rather than
// a custom channel, so a client that does not understand them simply sees the
// task working, and text streaming is untouched.
//
// Tool arguments are the reason [SummarizeToolInput] exists: a tool's input is
// model-authored and may carry anything the user typed, so only a short,
// redacted, length-capped line ever leaves the service.
//
// Granularity is per tool call (never per token) on purpose: the library
// records every status update against the stored task, so each event here is a
// task-store write.
package a2a

import (
	"encoding/gob"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// toolActivityKind is the discriminator on the data part, so a client can tell
// this payload apart from any other structured status message.
const toolActivityKind = "tool_call"

// Tool activity phases.
const (
	toolPhaseStarted  = "started"
	toolPhaseFinished = "finished"
)

// The server library deep-copies a stored task through gob, and a data part's
// payload travels as an interface value — so this concrete type has to be
// registered or every task carrying one fails to save.
func init() { gob.Register(toolActivityData{}) }

// toolActivityData is the JSON payload of a tool-activity data part. Field
// names are the client-facing contract (see internal/patchcli).
type toolActivityData struct {
	Kind      string `json:"kind"`
	Phase     string `json:"phase"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Summary   string `json:"summary,omitempty"`
	OK        bool   `json:"ok"`
	ElapsedMs int64  `json:"elapsedMs"`
}

// Argument-summary limits. A summary is a glance, not a record: a few keys,
// short values, and a hard cap on the whole line.
const (
	maxSummaryKeys     = 3
	maxSummaryValueLen = 32
	maxSummaryLen      = 96
)

// redactedValue replaces a value whose key looks like a credential.
const redactedValue = "[redacted]"

// secretKeyHints are substrings that make an argument name credential-shaped.
// Matching is on the lowercased key, so "apiKey" and "API_KEY" both hit.
var secretKeyHints = []string{"secret", "token", "password", "passwd", "credential", "auth", "key", "cookie", "session"}

// SummarizeToolInput renders a tool call's JSON arguments as one short line
// like `project=demo, region=us-east`, for display next to the tool's name.
//
// It is deliberately lossy: only top-level scalars are shown (nested objects
// and arrays are elided), credential-shaped keys are redacted, values and the
// whole line are truncated. Anything it cannot parse yields "" rather than
// echoing raw input back to the client.
func SummarizeToolInput(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable output: the same call always summarizes the same way

	parts := make([]string, 0, maxSummaryKeys)
	for _, k := range keys {
		if len(parts) == maxSummaryKeys {
			break
		}
		v, ok := scalarValue(args[k])
		if !ok {
			continue
		}
		if isSecretKey(k) {
			v = redactedValue
		}
		parts = append(parts, truncate(k, maxSummaryValueLen)+"="+truncate(v, maxSummaryValueLen))
	}
	return truncate(strings.Join(parts, ", "), maxSummaryLen)
}

// scalarValue renders a JSON scalar for display, reporting ok=false for
// objects, arrays, and nulls (structure a one-line summary cannot show).
func scalarValue(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), true
	case bool:
		return fmt.Sprintf("%t", t), true
	case float64:
		// json numbers decode as float64; render integers without ".0".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t)), true
		}
		return fmt.Sprintf("%g", t), true
	default:
		return "", false
	}
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, hint := range secretKeyHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// truncate shortens s to at most n characters (runes), marking the cut with an
// ellipsis so a clipped value never reads as a complete one.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
