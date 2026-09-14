// `patch conversations` — browse (and name) the durable chat history exposed
// by the conversations aggregated apiserver (assistant.miloapis.com).
//
// Per the apiserver design (decision #7, "an apiserver is not a chat
// transport"), listing and resuming are separate paths: this command is a
// read-only *discovery* view. It fetches raw API paths through [ReadView],
// which prefers datumctl's own identity and falls back to kubectl — see
// readview.go for why that order, and why neither transport uses client-side
// discovery. Once you have a context id, resume it with
// `patch resume <id>` (or non-interactively, `patch chat --context-id <id>`).
//
// `archive`, `unarchive` and `delete` write to that same aggregated API, through
// the same [ReadView] and so as the same identity: they change the
// conversation resource itself (spec.archived, or its existence), which is the
// apiserver's to serve. `rename` is the one subcommand that talks to the
// assistant service instead — it predates the API owning any writes, and the
// name belongs to the same conversation row the chat path owns.
package patchcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// listTitleWidth caps the NAME/TITLE column so a long opening message doesn't
// push the table past a normal terminal; the picker and `show` have the full
// text.
const listTitleWidth = 60

// runConversationsList prints a table of the caller's conversations in a
// project (id, created, last-active, message count, title), newest activity
// first — the ones not archived, or with --archived only the archive.
func runConversationsList(ctx context.Context, inv Invocation, io Io) int {
	view := ReadViewFor(inv)
	out, err := view.get(ctx, inv.Project, conversationsListPath(inv.Project, inv.Archived))
	if err != nil {
		io.Err("patch: " + readViewErrorText(view, err) + "\n")
		return 1
	}

	var list assistantv1alpha1.ConversationList
	if err := json.Unmarshal(out, &list); err != nil {
		io.Err("patch: could not parse conversations response: " + err.Error() + "\n")
		return 1
	}

	if inv.JSON {
		io.Out(string(out))
		if !strings.HasSuffix(string(out), "\n") {
			io.Out("\n")
		}
		return 0
	}

	if len(list.Items) == 0 {
		if inv.Archived {
			io.Err("no archived conversations in project " + inv.Project + "\n")
		} else {
			io.Err("no conversations in project " + inv.Project + "\n")
		}
		return 0
	}

	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].Status.LastActiveAt.After(list.Items[j].Status.LastActiveAt.Time)
	})

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CONTEXT-ID\tCREATED\tLAST-ACTIVE\tMESSAGES\tNAME/TITLE")
	for _, c := range list.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			c.Name,
			ago(c.CreationTimestamp.Time),
			ago(c.Status.LastActiveAt.Time),
			c.Status.MessageCount,
			previewLine(conversationTitle(c), listTitleWidth),
		)
	}
	_ = tw.Flush()
	io.Out(b.String())
	io.Err("\nresume one with:  patch resume <context-id> --project " + inv.Project + "\n")
	return 0
}

// runConversationsShow prints the full transcript of one conversation, fetched
// from the `messages` subresource.
func runConversationsShow(ctx context.Context, inv Invocation, io Io) int {
	view := ReadViewFor(inv)
	out, err := view.get(ctx, inv.Project, messagesPath(inv.Project, inv.ContextID))
	if err != nil {
		io.Err("patch: " + readViewErrorText(view, err) + "\n")
		return 1
	}

	if inv.JSON {
		io.Out(string(out))
		if !strings.HasSuffix(string(out), "\n") {
			io.Out("\n")
		}
		return 0
	}

	var msgs assistantv1alpha1.ConversationMessages
	if err := json.Unmarshal(out, &msgs); err != nil {
		io.Err("patch: could not parse messages response: " + err.Error() + "\n")
		return 1
	}

	io.Err(fmt.Sprintf("conversation %s  (%s, %d messages)\n\n",
		inv.ContextID, inv.Project, len(msgs.Items)))
	for _, m := range msgs.Items {
		io.Out(fmt.Sprintf("[%d] %s:\n%s\n\n", m.Seq, m.Role, strings.TrimRight(m.Content, "\n")))
	}
	io.Err("resume with:  patch resume " + inv.ContextID + " --project " + inv.Project + "\n")
	return 0
}

// runConversationsArchive archives (archived=true) or unarchives one
// conversation through the aggregated API. Archiving says how to find the
// conversation again, on stderr so -o json and piped stdout stay clean: a
// conversation that vanishes from the list with no hint where it went reads
// as lost.
func runConversationsArchive(ctx context.Context, inv Invocation, io Io, archived bool) int {
	view := ReadViewFor(inv)
	_, err := view.setConversationArchived(ctx, inv.Project, inv.ContextID, archived)
	if err != nil {
		err = errors.New(readViewErrorText(view, err))
	}
	verb := "unarchived"
	if archived {
		verb = "archived"
	}
	code := renderConversationAction(verb, err, inv.ContextID, inv.JSON, io)
	if code == 0 && archived && !inv.JSON {
		io.Err("find it again with:  patch conversations list --archived --project " + inv.Project +
			"  (sending it another message unarchives it)\n")
	}
	return code
}

// runConversationsDelete permanently deletes one conversation, asking first.
//
// The confirmation is the point of this command's shape. Delete cannot be
// undone, so without --yes it names the conversation — fetched first, which
// also turns a mistyped id into a not-found before anyone is asked anything —
// and waits for an explicit y. With no terminal on stdin there is nobody to
// ask, and reading "no answer" as consent would make a pipeline the one place
// a delete never needs confirming; so that is an error that says to pass
// --yes, never a silent delete.
func runConversationsDelete(ctx context.Context, inv Invocation, io Io) int {
	view := ReadViewFor(inv)
	if !inv.Yes {
		lines, canRead := io.(LineReader)
		if !inv.StdinTerminal || !canRead {
			io.Err("patch: conversations delete: stdin is not a terminal, so the deletion cannot be confirmed — " +
				"pass --yes to delete " + inv.ContextID + " without asking\n")
			return 2
		}
		conv, err := view.getConversation(ctx, inv.Project, inv.ContextID)
		if err != nil {
			return renderConversationAction("deleted", errors.New(readViewErrorText(view, err)), inv.ContextID, inv.JSON, io)
		}
		io.Err(deletePrompt(conv))
		answer, ok := lines.ReadLine()
		if !ok || !confirmed(answer) {
			io.Err("not deleted\n")
			return 1
		}
	}
	err := view.deleteConversation(ctx, inv.Project, inv.ContextID)
	if err != nil {
		err = errors.New(readViewErrorText(view, err))
	}
	return renderConversationAction("deleted", err, inv.ContextID, inv.JSON, io)
}

// deletePrompt is the question `conversations delete` asks: the conversation's
// label, so the user recognizes what they are about to lose, and its id, so
// two conversations with the same opening message cannot be confused.
func deletePrompt(c assistantv1alpha1.Conversation) string {
	title := previewLine(conversationTitle(c), listTitleWidth)
	if title == c.Name {
		return fmt.Sprintf("delete conversation %s? this cannot be undone [y/N] ", c.Name)
	}
	return fmt.Sprintf("delete conversation %s (%s)? this cannot be undone [y/N] ", title, c.Name)
}

// confirmed reports whether a [y/N] answer is a yes. Only an explicit y/yes
// counts; the default, and anything unrecognized, is no.
func confirmed(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	}
	return false
}

// runConversationsRename names one conversation through the assistant service
// (POST /v1alpha1/conversations/rename). Unlike its sibling subcommands this is a
// write, so it needs PATCH_URL/PATCH_TOKEN rather than the apiserver read view.
func runConversationsRename(ctx context.Context, inv Invocation, io Io) int {
	err := requestRename(ctx, inv.BaseURL, inv.Token, inv.Project, inv.ContextID, inv.Name)
	return renderRenameResult(err, inv.ContextID, inv.Name, inv.JSON, io)
}

// latestConversation returns the id of the project's most recently active
// conversation, or "" when the project has none — what -c/--continue resumes.
// The apiserver already serves the listing newest-activity first, but this
// re-derives the maximum rather than trusting row order, the same way the list
// view and the picker re-sort what they render. It asks for the default
// listing, which the apiserver serves without archived conversations — so an
// archived conversation is never what "continue my last one" picks up, however
// recently it was active.
func latestConversation(ctx context.Context, view ReadView, project string) (string, error) {
	out, err := view.get(ctx, project, conversationsPath(project))
	if err != nil {
		return "", errors.New(readViewErrorText(view, err))
	}
	var list assistantv1alpha1.ConversationList
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("could not parse conversations response: %w", err)
	}
	var (
		newest string
		at     time.Time
	)
	for _, c := range list.Items {
		if t := c.Status.LastActiveAt.Time; newest == "" || t.After(at) {
			newest, at = c.Name, t
		}
	}
	return newest, nil
}

// continueContextID resolves -c/--continue (and `resume --last`) to a context
// id, reporting on stderr and returning "" when there is nothing to continue or
// the listing could not be read. Neither is fatal: a fresh conversation is a
// far better answer to "continue my last one" than a refusal to start at all.
func continueContextID(ctx context.Context, inv Invocation, io Io) string {
	id, err := latestConversation(ctx, ReadViewFor(inv), inv.Project)
	switch {
	case err != nil:
		io.Err("patch: could not look up the last conversation: " + err.Error() + "\n")
	case id == "":
		io.Err("no conversations in project " + inv.Project + " yet — starting a new one\n")
	}
	return id
}

// conversationsPath, conversationsListPath, conversationPath and messagesPath
// build the group-relative paths both transports use. Kept together so the
// list view and the TUI picker cannot drift apart.
func conversationsPath(project string) string {
	return fmt.Sprintf("/apis/assistant.miloapis.com/v1alpha1/namespaces/%s/conversations", project)
}

// archivedFieldSelector is the query that lists the archive instead of the
// everyday listing — the one field selector the conversations API serves.
var archivedFieldSelector = "?fieldSelector=" + url.QueryEscape("spec.archived=true")

// conversationsListPath is the listing: the default (not archived) view, or
// with archived the archive. The default is sent with no selector at all
// rather than spec.archived=false, so this client keeps working against an
// apiserver that predates the field.
func conversationsListPath(project string, archived bool) string {
	if archived {
		return conversationsPath(project) + archivedFieldSelector
	}
	return conversationsPath(project)
}

// conversationPath addresses one conversation. Unlike the read paths, the id
// is escaped: this is the path delete is sent to, and an id that could climb
// out of its segment must not be able to aim an irreversible verb anywhere
// else.
func conversationPath(project, contextID string) string {
	return conversationsPath(project) + "/" + url.PathEscape(contextID)
}

func messagesPath(project, contextID string) string {
	return fmt.Sprintf(
		"/apis/assistant.miloapis.com/v1alpha1/namespaces/%s/conversations/%s/messages",
		project, contextID)
}

// kubectlJSON runs kubectl and returns its combined stdout. A non-empty
// kubeconfig is passed via --kubeconfig; otherwise kubectl uses its normal
// resolution (KUBECONFIG env, then ~/.kube/config).
func kubectlJSON(ctx context.Context, kubeconfig string, args ...string) ([]byte, error) {
	return kubectlRun(ctx, kubeconfig, nil, args...)
}

// kubectlRun is kubectlJSON with an optional stdin (the object `replace --raw
// -f -` reads). A variable so tests can see the exact kubectl invocation the
// archive and delete fallbacks make without a kubectl or a cluster.
var kubectlRun = func(ctx context.Context, kubeconfig string, stdin []byte, args ...string) ([]byte, error) {
	full := args
	if kubeconfig != "" {
		full = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.Output()
}

// kubectlErrorText renders a failed kubectl invocation as a one-line message,
// surfacing kubectl's own stderr (carried on ExitError) when present. Shared
// by failKubectl (the `conversations` subcommand) and the chat TUI's picker,
// so both report the same friendly text for the same failure.
func kubectlErrorText(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return "kubectl: " + strings.TrimRight(string(exit.Stderr), "\n")
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "kubectl not found on PATH — this needs kubectl to reach the apiserver"
	}
	return "kubectl: " + err.Error()
}

// ago renders a timestamp as a compact relative age (kubectl-style: 5m, 3h,
// 2d), falling back to a date for older or zero times.
func ago(t time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("2006-01-02")
	}
}
