// Run is the injectable heart of the `patch` CLI: it parses argv, resolves the
// service URL/token (flags override PATCH_URL/PATCH_TOKEN), dispatches to the
// a2a-go client, and renders the result through the injected Io. It returns the
// process exit code. main() wires the real process streams and environment.
package main

import (
	"context"
	"errors"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Run executes one CLI invocation and returns the process exit code:
//
//	0  success
//	1  a request/stream error, or a task that did not complete
//	2  a usage error (bad args, or no service URL)
func Run(ctx context.Context, argv []string, getenv func(string) string, io Io) int {
	cmd := parseArgs(argv)

	switch cmd.kind {
	case cmdHelp:
		io.Out(usage)
		return 0
	case cmdError:
		io.Err("patch: " + cmd.errMsg + "\n\n" + usage)
		return 2
	case cmdConvList:
		// Conversations are the apiserver read view (kubectl + your k8s
		// identity), not the A2A service — no PATCH_URL/PATCH_TOKEN needed.
		return runConversationsList(ctx, cmd, io)
	case cmdConvShow:
		return runConversationsShow(ctx, cmd, io)
	case cmdGapList:
		// Same apiserver read path as conversations — kubectl + k8s identity,
		// no PATCH_URL/PATCH_TOKEN.
		return runGapsList(ctx, cmd, io)
	}

	baseURL := cmd.url
	if baseURL == "" {
		baseURL = getenv("PATCH_URL")
	}
	if baseURL == "" {
		io.Err("patch: no service URL — set PATCH_URL or pass --url\n")
		return 2
	}
	token := cmd.token
	if token == "" {
		token = getenv("PATCH_TOKEN")
	}

	switch cmd.kind {
	case cmdCard:
		card, err := resolveCard(ctx, baseURL)
		if err != nil {
			return fail(io, err)
		}
		renderCard(card, cmd.json, io)
		return 0

	case cmdChat:
		client, err := newClient(ctx, baseURL, token)
		if err != nil {
			return fail(io, err)
		}
		defer client.Destroy()
		if cmd.tui {
			return runChatTUI(ctx, client, cmd.project, cmd.contextID, cmd.message, cmd.kubeconfig, baseURL, token)
		}
		if cmd.interactive {
			return runRepl(ctx, client, cmd.project, cmd.contextID, cmd.message, io)
		}
		code, contextID, streamErr := chatTurn(ctx, client, cmd.message, cmd.project, cmd.contextID, cmd.json, io)
		if streamErr != nil {
			return fail(io, streamErr)
		}
		// Tell the user how to continue this conversation (stderr, so piped
		// stdout stays clean; --json already carries contextId in the events).
		if !cmd.json && contextID != "" {
			io.Err("context: " + contextID + "  (continue with --context-id)\n")
		}
		return code

	case cmdCompact:
		err := requestCompact(ctx, baseURL, token, cmd.project, cmd.contextID)
		return renderCompactResult(err, cmd.json, io)

	case cmdTaskGet:
		client, err := newClient(ctx, baseURL, token)
		if err != nil {
			return fail(io, err)
		}
		defer client.Destroy()
		task, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(cmd.id)})
		if err != nil {
			return fail(io, err)
		}
		renderTask(task, cmd.json, io)
		return 0

	case cmdTaskCancel:
		client, err := newClient(ctx, baseURL, token)
		if err != nil {
			return fail(io, err)
		}
		defer client.Destroy()
		task, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(cmd.id)})
		if err != nil {
			return fail(io, err)
		}
		renderTask(task, cmd.json, io)
		return 0
	}

	return 0
}

// fail prints a friendly, exit-code-1 error for a request/stream failure.
func fail(io Io, err error) int {
	io.Err("patch: " + friendlyError(err) + "\n")
	return 1
}

// friendlyError turns an a2a-go error into a CLI-appropriate message, mapping
// the protocol's sentinel errors to the same wording the TS CLI used.
func friendlyError(err error) string {
	switch {
	case errors.Is(err, a2a.ErrUnauthenticated):
		return "unauthorized: " + err.Error() + " (check PATCH_TOKEN / --token)"
	case errors.Is(err, a2a.ErrUnauthorized):
		return "forbidden: " + err.Error() + " (token does not grant this project)"
	case errors.Is(err, a2a.ErrTaskNotCancelable):
		return "task cannot be canceled (it is already in a terminal state)"
	default:
		return err.Error()
	}
}
