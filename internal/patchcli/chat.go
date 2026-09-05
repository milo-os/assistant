// Chat-turn plumbing shared by the one-shot and interactive (REPL) chat
// modes: sending one turn, learning the conversation's contextId from the
// event stream, and the line-based REPL loop itself.
package patchcli

import (
	"context"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// LineReader is the optional extension of [Io] that interactive mode needs:
// it reads one line of user input, reporting ok=false on end of input. The
// real CLI backs it with stdin; tests script it.
type LineReader interface {
	ReadLine() (line string, ok bool)
}

// chatTurn sends one message and renders the streamed response. It returns
// the exit code, the conversation's contextId as reported by the service
// (so a follow-up turn can continue the conversation), and any stream error.
func chatTurn(ctx context.Context, client *serviceClient, text, project, contextID string, jsonOut bool, io Io) (int, string, error) {
	// The line-based modes have no picker, but "@kind/name" typed by hand (or
	// piped in) still reaches the service the same way the TUI's does.
	req := &a2a.SendMessageRequest{Message: buildMessage(text, project, contextID, parseMentions(text))}
	events := client.SendStreamingMessage(ctx, req)
	seen := contextID
	code, err := renderChat(captureContextID(events, &seen), jsonOut, io)
	return code, seen, err
}

// captureContextID tees an event stream, recording the contextId carried by
// the events into *dst as they pass through to the renderer.
func captureContextID(events iter.Seq2[a2a.Event, error], dst *string) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range events {
			if id := eventContextID(ev); id != "" {
				*dst = id
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

func eventContextID(ev a2a.Event) string {
	switch e := ev.(type) {
	case *a2a.Task:
		return e.ContextID
	case *a2a.Message:
		return e.ContextID
	case *a2a.TaskStatusUpdateEvent:
		return e.ContextID
	case *a2a.TaskArtifactUpdateEvent:
		return e.ContextID
	}
	return ""
}

// runRepl is the interactive chat session: each entered line is one turn, and
// the contextId learned from the first turn is sent on every later one, so
// the whole session is a single conversation with memory. firstMessage, when
// non-empty, is sent before the first prompt. Exits 0 on Ctrl-D or /quit;
// a failed turn ends the session with its exit code (consistent with the
// one-shot mode, and typical failures — auth, bad project — would fail every
// subsequent turn the same way).
func runRepl(ctx context.Context, client *serviceClient, project, contextID, firstMessage string, io Io) int {
	lines, ok := io.(LineReader)
	if !ok {
		io.Err("patch: interactive mode is not available on this input\n")
		return 2
	}

	io.Err("patch interactive chat — project " + project + " (Ctrl-D or /quit to leave)\n")
	message := firstMessage
	for {
		if message != "" {
			code, seen, err := chatTurn(ctx, client, message, project, contextID, false, io)
			if err != nil {
				return fail(io, err, client.errs)
			}
			if code != 0 {
				return code
			}
			if contextID == "" && seen != "" {
				contextID = seen
				io.Err("context: " + contextID + "\n")
			}
		}

		io.Err("\n> ")
		line, more := lines.ReadLine()
		if !more || line == "/quit" || line == "/exit" {
			io.Err("\n")
			return 0
		}
		message = line
	}
}
