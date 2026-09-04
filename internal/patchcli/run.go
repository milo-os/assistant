// The two entrypoints into this package.
//
// [Run] is the standalone `patch` CLI's: it parses argv, resolves the service
// URL/token (flags override PATCH_URL/PATCH_TOKEN), and executes. The datumctl
// plugin does its own flag parsing with cobra and its own URL/token resolution
// against datumctl's injected environment, then builds an [Invocation] and
// calls [Invocation.Execute] — the shared dispatch both share.
package patchcli

import (
	"context"
	"errors"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// Kind names the command an [Invocation] runs.
type Kind int

const (
	// KindCard fetches and renders the agent card.
	KindCard Kind = iota + 1
	// KindChat sends a chat turn (one-shot, REPL, or full-screen TUI).
	KindChat
	// KindCompact forces history compaction for one conversation.
	KindCompact
	// KindConvList lists the caller's conversations in a project.
	KindConvList
	// KindConvShow prints one conversation's transcript.
	KindConvShow
	// KindGapList lists a provider project's capability-gap reports.
	KindGapList
	// KindTaskGet fetches one task.
	KindTaskGet
	// KindTaskCancel cancels one task.
	KindTaskCancel
)

// Invocation is one fully-resolved CLI command: the caller has already decided
// the service URL and how to mint a token, so [Invocation.Execute] does no
// environment lookups of its own.
type Invocation struct {
	Kind Kind

	// JSON emits raw JSON (events for chat, objects otherwise).
	JSON bool

	// BaseURL is the assistant service's base URL. Required by KindCard,
	// KindChat, KindCompact and the two task kinds; the conversations and
	// gaps read views go to the aggregated apiserver instead and ignore it.
	BaseURL string
	// Token mints the bearer token for the service. Nil leaves requests
	// unauthenticated.
	Token TokenSource

	// Message is the chat turn's text. Empty is valid for Interactive/TUI.
	Message string
	// Project is the Milo project the task runs against — for the gaps read
	// view, the PROVIDER's own project.
	Project string
	// ContextID continues an existing conversation.
	ContextID string
	// Interactive selects the line-based REPL, TUI the full-screen chat UI.
	Interactive bool
	TUI         bool

	// Kubeconfig overrides KUBECONFIG for the apiserver read views.
	Kubeconfig string

	// ID is the task id for KindTaskGet / KindTaskCancel.
	ID string
}

// Run executes one standalone-CLI invocation and returns the process exit code:
//
//	0  success
//	1  a request/stream error, or a task that did not complete
//	2  a usage error (bad args, or no service URL)
func Run(ctx context.Context, argv []string, getenv func(string) string, io Io) int {
	cmd := parseArgs(argv)

	switch cmd.kind {
	case kindHelp:
		io.Out(usage)
		return 0
	case kindError:
		io.Err("patch: " + cmd.errMsg + "\n\n" + usage)
		return 2
	}

	inv := Invocation{
		Kind:        cmd.kind,
		JSON:        cmd.json,
		Message:     cmd.message,
		Project:     cmd.project,
		ContextID:   cmd.contextID,
		Interactive: cmd.interactive,
		TUI:         cmd.tui,
		Kubeconfig:  cmd.kubeconfig,
		ID:          cmd.id,
	}

	// The apiserver read views use kubectl and the caller's k8s identity, not
	// the A2A service — no PATCH_URL/PATCH_TOKEN needed.
	if !inv.needsService() {
		return inv.Execute(ctx, io)
	}

	inv.BaseURL = cmd.url
	if inv.BaseURL == "" {
		inv.BaseURL = getenv("PATCH_URL")
	}
	if inv.BaseURL == "" {
		io.Err("patch: no service URL — set PATCH_URL or pass --url\n")
		return 2
	}
	token := cmd.token
	if token == "" {
		token = getenv("PATCH_TOKEN")
	}
	inv.Token = StaticToken(token)

	return inv.Execute(ctx, io)
}

// needsService reports whether this command talks to the assistant service (as
// opposed to the aggregated apiserver read views, which use kubectl).
func (inv Invocation) needsService() bool {
	switch inv.Kind {
	case KindConvList, KindConvShow, KindGapList:
		return false
	}
	return true
}

// Execute runs one resolved command and returns the process exit code, using
// the same 0/1/2 convention as [Run].
func (inv Invocation) Execute(ctx context.Context, io Io) int {
	switch inv.Kind {
	case KindConvList:
		return runConversationsList(ctx, inv, io)
	case KindConvShow:
		return runConversationsShow(ctx, inv, io)
	case KindGapList:
		return runGapsList(ctx, inv, io)

	case KindCard:
		card, err := resolveCard(ctx, inv.BaseURL)
		if err != nil {
			return fail(io, err, nil)
		}
		renderCard(card, inv.JSON, io)
		return 0

	case KindChat:
		client, err := newClient(ctx, inv.BaseURL, inv.Token)
		if err != nil {
			return fail(io, err, nil)
		}
		defer client.Destroy()
		if inv.TUI {
			return runChatTUI(ctx, client, inv.Project, inv.ContextID, inv.Message, inv.Kubeconfig, inv.BaseURL, inv.Token)
		}
		if inv.Interactive {
			return runRepl(ctx, client, inv.Project, inv.ContextID, inv.Message, io)
		}
		code, contextID, streamErr := chatTurn(ctx, client, inv.Message, inv.Project, inv.ContextID, inv.JSON, io)
		if streamErr != nil {
			return fail(io, streamErr, client.errs)
		}
		// Tell the user how to continue this conversation (stderr, so piped
		// stdout stays clean; --json already carries contextId in the events).
		if !inv.JSON && contextID != "" {
			io.Err("context: " + contextID + "  (continue with --context-id)\n")
		}
		return code

	case KindCompact:
		err := requestCompact(ctx, inv.BaseURL, inv.Token, inv.Project, inv.ContextID)
		return renderCompactResult(err, inv.JSON, io)

	case KindTaskGet:
		client, err := newClient(ctx, inv.BaseURL, inv.Token)
		if err != nil {
			return fail(io, err, nil)
		}
		defer client.Destroy()
		task, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(inv.ID)})
		if err != nil {
			return fail(io, err, client.errs)
		}
		renderTask(task, inv.JSON, io)
		return 0

	case KindTaskCancel:
		client, err := newClient(ctx, inv.BaseURL, inv.Token)
		if err != nil {
			return fail(io, err, nil)
		}
		defer client.Destroy()
		task, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(inv.ID)})
		if err != nil {
			return fail(io, err, client.errs)
		}
		renderTask(task, inv.JSON, io)
		return 0
	}

	return 0
}

// AuthHint is appended to authentication failures to say where this binary
// gets its token. The standalone CLI reads PATCH_TOKEN; the datumctl plugin
// asks datumctl's credentials helper, so it replaces this at startup.
var AuthHint = "check PATCH_TOKEN / --token"

// fail prints a friendly, exit-code-1 error for a request/stream failure.
func fail(io Io, err error, errs *httpErrRecorder) int {
	io.Err("patch: " + friendlyError(err, errs) + "\n")
	return 1
}

// friendlyError turns an a2a-go error into a CLI-appropriate message.
//
// Two paths reach here. Against a fake that answers in-protocol, a2a-go
// surfaces its sentinel errors and the first two cases fire. Against the real
// service, auth failures are plain HTTP 401/403 whose body a2a-go discards —
// so errs, filled in by [bearerTransport], carries the service's own message
// and the status that classifies it.
func friendlyError(err error, errs *httpErrRecorder) string {
	switch {
	case errors.Is(err, a2a.ErrUnauthenticated):
		return "unauthorized: " + err.Error() + " (" + AuthHint + ")"
	case errors.Is(err, a2a.ErrUnauthorized):
		return "forbidden: " + err.Error() + " (token does not grant this project)"
	case errors.Is(err, a2a.ErrTaskNotCancelable):
		return "task cannot be canceled (it is already in a terminal state)"
	}

	switch status, msg := errs.last(); status {
	case http.StatusUnauthorized:
		return "unauthorized: " + msg + " (" + AuthHint + ")"
	case http.StatusForbidden:
		return "forbidden: " + msg
	case 0:
		return err.Error()
	default:
		if msg == "" {
			return err.Error()
		}
		return msg
	}
}
