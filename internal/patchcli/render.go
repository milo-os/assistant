// Terminal rendering for the `patch` CLI. All output goes through an injected
// Io (Out/Err writers) so rendering is unit-testable against scripted event
// streams without touching os.Stdout/os.Stderr.
//
// Convention: the assistant's ANSWER text goes to STDOUT; status transitions
// and decoration go to STDERR. So `patch chat … > answer.txt` captures just
// the reply and pipelines stay clean.
//
// The wire is real A2A v1.0 (a2a-go types): task states arrive as the
// TASK_STATE_* enum values, which friendlyState maps back to the lowercase
// words the TS CLI printed (submitted, working, completed, …).
package patchcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Io abstracts the two output streams so tests can capture them.
type Io interface {
	Out(text string)
	Err(text string)
}

// renderChat consumes a streaming A2A response, writing the answer to stdout
// and status transitions to stderr. It returns the exit code (0 when the task
// reached the completed state, 1 otherwise) and any stream error surfaced by
// the client (auth/validation failures), which Run maps to a friendly message.
func renderChat(events iter.Seq2[a2a.Event, error], jsonOut bool, io Io) (int, error) {
	var finalState a2a.TaskState
	wroteAnswer := false
	answerEndsWithNewline := false

	for ev, err := range events {
		if err != nil {
			return 1, err
		}
		if jsonOut {
			if b, mErr := json.Marshal(ev); mErr == nil {
				io.Out(string(b) + "\n")
			}
		}
		switch e := ev.(type) {
		case *a2a.Task:
			if !jsonOut {
				io.Err(fmt.Sprintf("» task %s (%s)\n", e.ID, friendlyState(e.Status.State)))
			}
			finalState = e.Status.State
		case *a2a.TaskStatusUpdateEvent:
			// Tool activity rides on working-state updates: it is progress
			// within a state, not a transition, so it prints an activity row
			// instead of another "» working" line. Stderr keeps piped stdout
			// to just the answer.
			if act, ok := toolActivityFrom(e.Status.Message); ok {
				if !jsonOut {
					io.Err(activityLine(act))
				}
				continue
			}
			if !jsonOut {
				io.Err(fmt.Sprintf("» %s\n", friendlyState(e.Status.State)))
			}
			finalState = e.Status.State
			if e.Status.State == a2a.TaskStateFailed && e.Status.Message != nil {
				io.Err("  " + textOf(e.Status.Message.Parts) + "\n")
			}
		case *a2a.TaskArtifactUpdateEvent:
			if text := textOf(e.Artifact.Parts); text != "" && !jsonOut {
				io.Out(text)
				wroteAnswer = true
				answerEndsWithNewline = strings.HasSuffix(text, "\n")
			}
		case *a2a.Message:
			// A bare agent message (non-task path); render its text.
			if !jsonOut {
				if text := textOf(e.Parts); text != "" {
					io.Out(text)
					wroteAnswer = true
					answerEndsWithNewline = strings.HasSuffix(text, "\n")
				}
			}
		}
	}

	// Tidy trailing newline so the shell prompt starts on its own line.
	if !jsonOut && wroteAnswer && !answerEndsWithNewline {
		io.Out("\n")
	}

	if finalState == a2a.TaskStateCompleted {
		return 0, nil
	}
	return 1, nil
}

// activityLine renders one tool-activity row for the plain (non-TUI) modes:
// a line when the call starts and a line when it ends, so a long turn shows
// progress in a log or a piped terminal without any cursor tricks.
func activityLine(act toolActivity) string {
	if act.started() {
		return "• " + act.Name + " …\n"
	}
	if !act.OK {
		return "• " + act.Name + " … failed · " + formatElapsed(act.elapsed()) + "\n"
	}
	return "• " + act.Name + " … " + formatElapsed(act.elapsed()) + "\n"
}

// formatElapsed renders a tool call's duration at glance precision: tenths of
// a second under a minute, minutes and whole seconds beyond it.
func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
}

// renderCompactResult prints the outcome of `patch compact` (POST
// /v1/compact via [requestCompact]) and returns the process exit code: 0 on
// success or "nothing to compact" (neither is a user-facing failure), 1 on a
// real error. Unlike renderChat there is no event stream — one request, one
// outcome — so this mirrors renderTask's simple pretty/--json split instead.
func renderCompactResult(err error, jsonOut bool, io Io) int {
	compacted := err == nil
	nothingToDo := errors.Is(err, ErrNothingToCompact)

	if jsonOut {
		body := map[string]any{"compacted": compacted}
		if nothingToDo {
			body["reason"] = "nothing to compact"
		} else if err != nil {
			body["error"] = err.Error()
		}
		if b, mErr := json.Marshal(body); mErr == nil {
			io.Out(string(b) + "\n")
		}
	} else {
		switch {
		case compacted:
			io.Out("history compacted\n")
		case nothingToDo:
			io.Out("nothing to compact\n")
		default:
			io.Err("patch: " + err.Error() + "\n")
		}
	}

	if err != nil && !nothingToDo {
		return 1
	}
	return 0
}

// renderRenameResult prints the outcome of `patch conversations rename` (POST
// /v1/conversations/rename via [requestRename]) and returns the process exit
// code. Simpler than renderCompactResult: there is no "nothing to do" outcome
// to soften — a rename either landed or failed.
func renderRenameResult(err error, contextID, name string, jsonOut bool, io Io) int {
	if jsonOut {
		body := map[string]any{"renamed": err == nil, "contextId": contextID, "name": name}
		if err != nil {
			body["error"] = err.Error()
		}
		if b, mErr := json.Marshal(body); mErr == nil {
			io.Out(string(b) + "\n")
		}
	} else if err != nil {
		io.Err("patch: " + err.Error() + "\n")
	} else {
		io.Out("renamed " + contextID + " to " + name + "\n")
	}

	if err != nil {
		return 1
	}
	return 0
}

// renderCard prints an agent card, either pretty or as raw JSON.
func renderCard(card *a2a.AgentCard, jsonOut bool, io Io) {
	if jsonOut {
		if b, err := json.MarshalIndent(card, "", "  "); err == nil {
			io.Out(string(b) + "\n")
		}
		return
	}
	iface := primaryInterface(card)
	lines := []string{
		fmt.Sprintf("%s  (A2A protocol %s, v%s)", card.Name, protocolVersion(iface), card.Version),
		card.Description,
		"",
		fmt.Sprintf("Endpoint:   %s  [%s]", endpointURL(iface), transport(iface)),
		fmt.Sprintf("Provider:   %s", describeProvider(card)),
		fmt.Sprintf("Streaming:  %s", yesNo(card.Capabilities.Streaming)),
		fmt.Sprintf("Auth:       %s", describeSecurity(card)),
		fmt.Sprintf("Skills:     %s", describeSkills(card)),
	}
	io.Out(strings.Join(lines, "\n") + "\n")
}

// renderTask prints a task's state and answer, either pretty or as raw JSON.
func renderTask(task *a2a.Task, jsonOut bool, io Io) {
	if jsonOut {
		if b, err := json.MarshalIndent(task, "", "  "); err == nil {
			io.Out(string(b) + "\n")
		}
		return
	}
	lines := []string{
		"Task " + string(task.ID),
		"  context: " + task.ContextID,
		"  state:   " + friendlyState(task.Status.State),
	}
	if task.Status.Message != nil {
		lines = append(lines, "  message: "+textOf(task.Status.Message.Parts))
	}
	var answer strings.Builder
	for _, a := range task.Artifacts {
		answer.WriteString(textOf(a.Parts))
	}
	if answer.Len() > 0 {
		lines = append(lines, "", answer.String())
	}
	io.Out(strings.Join(lines, "\n") + "\n")
}

// friendlyState maps an A2A v1.0 TASK_STATE_* enum to the lowercase word the
// TS CLI printed.
func friendlyState(s a2a.TaskState) string {
	switch s {
	case a2a.TaskStateSubmitted:
		return "submitted"
	case a2a.TaskStateWorking:
		return "working"
	case a2a.TaskStateInputRequired:
		return "input-required"
	case a2a.TaskStateCompleted:
		return "completed"
	case a2a.TaskStateCanceled:
		return "canceled"
	case a2a.TaskStateFailed:
		return "failed"
	case a2a.TaskStateRejected:
		return "rejected"
	case a2a.TaskStateAuthRequired:
		return "auth-required"
	default:
		return "unknown"
	}
}

// textOf concatenates the text of all text parts, ignoring non-text parts.
func textOf(parts a2a.ContentParts) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text())
	}
	return b.String()
}

// primaryInterface returns the first advertised transport interface, or nil.
// A2A v1.0 moved URL/transport/protocolVersion into SupportedInterfaces.
func primaryInterface(card *a2a.AgentCard) *a2a.AgentInterface {
	if len(card.SupportedInterfaces) == 0 {
		return nil
	}
	return card.SupportedInterfaces[0]
}

func endpointURL(iface *a2a.AgentInterface) string {
	if iface == nil {
		return "(none)"
	}
	return iface.URL
}

func transport(iface *a2a.AgentInterface) string {
	if iface == nil {
		return "unknown"
	}
	return string(iface.ProtocolBinding)
}

func protocolVersion(iface *a2a.AgentInterface) string {
	if iface == nil {
		return "unknown"
	}
	return string(iface.ProtocolVersion)
}

func describeProvider(card *a2a.AgentCard) string {
	if card.Provider == nil {
		return "(none)"
	}
	return fmt.Sprintf("%s  %s", card.Provider.Org, card.Provider.URL)
}

// describeSecurity renders the card's HTTP auth scheme as "http <scheme>"
// (e.g. "http bearer"), falling back to the joined scheme names.
func describeSecurity(card *a2a.AgentCard) string {
	if len(card.SecuritySchemes) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(card.SecuritySchemes))
	for name, scheme := range card.SecuritySchemes {
		switch s := scheme.(type) {
		case a2a.HTTPAuthSecurityScheme:
			return "http " + strings.ToLower(s.Scheme)
		case *a2a.HTTPAuthSecurityScheme:
			return "http " + strings.ToLower(s.Scheme)
		}
		names = append(names, string(name))
	}
	return strings.Join(names, ", ")
}

func describeSkills(card *a2a.AgentCard) string {
	if len(card.Skills) == 0 {
		return "(none)"
	}
	ids := make([]string, len(card.Skills))
	for i, s := range card.Skills {
		ids[i] = s.ID
	}
	return strings.Join(ids, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
