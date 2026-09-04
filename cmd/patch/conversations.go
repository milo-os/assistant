// `patch conversations` — browse the durable chat history exposed by the
// conversations aggregated apiserver (assistant.miloapis.com).
//
// Per the apiserver design (decision #7, "an apiserver is not a chat
// transport"), listing and resuming are separate paths: this command is a
// read-only *discovery* view. It shells out to `kubectl` rather than embedding
// client-go — the `patch` binary is a deliberately thin a2a-go client with no
// k8s client dependency, and kubectl already carries the kubeconfig + TLS trust
// for the aggregated apiserver's serving cert (the verified path from Phase 3).
// The caller authenticates with their normal k8s identity (KUBECONFIG), NOT the
// A2A service's PATCH_TOKEN. Once you have a context id, resume it with
// `patch chat --context-id <id>`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	assistantv1alpha1 "github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// runConversationsList prints a table of the caller's conversations in a
// project (id, created, last-active, message count), newest activity first.
func runConversationsList(ctx context.Context, cmd command, io Io) int {
	out, err := kubectlJSON(ctx, cmd.kubeconfig,
		"get", "conversations", "-n", cmd.project, "-o", "json")
	if err != nil {
		return failKubectl(io, err, out)
	}

	var list assistantv1alpha1.ConversationList
	if err := json.Unmarshal(out, &list); err != nil {
		io.Err("patch: could not parse conversations response: " + err.Error() + "\n")
		return 1
	}

	if cmd.json {
		io.Out(string(out))
		if !strings.HasSuffix(string(out), "\n") {
			io.Out("\n")
		}
		return 0
	}

	if len(list.Items) == 0 {
		io.Err("no conversations in project " + cmd.project + "\n")
		return 0
	}

	sort.SliceStable(list.Items, func(i, j int) bool {
		return list.Items[i].Status.LastActiveAt.After(list.Items[j].Status.LastActiveAt.Time)
	})

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CONTEXT-ID\tCREATED\tLAST-ACTIVE\tMESSAGES")
	for _, c := range list.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n",
			c.Name,
			ago(c.CreationTimestamp.Time),
			ago(c.Status.LastActiveAt.Time),
			c.Status.MessageCount,
		)
	}
	_ = tw.Flush()
	io.Out(b.String())
	io.Err("\nresume one with:  patch chat --context-id <context-id> --project " + cmd.project + "\n")
	return 0
}

// runConversationsShow prints the full transcript of one conversation, fetched
// from the `messages` subresource.
func runConversationsShow(ctx context.Context, cmd command, io Io) int {
	path := fmt.Sprintf(
		"/apis/assistant.miloapis.com/v1alpha1/namespaces/%s/conversations/%s/messages",
		cmd.project, cmd.contextID)
	out, err := kubectlJSON(ctx, cmd.kubeconfig, "get", "--raw", path)
	if err != nil {
		return failKubectl(io, err, out)
	}

	if cmd.json {
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
		cmd.contextID, cmd.project, len(msgs.Items)))
	for _, m := range msgs.Items {
		io.Out(fmt.Sprintf("[%d] %s:\n%s\n\n", m.Seq, m.Role, strings.TrimRight(m.Content, "\n")))
	}
	io.Err("resume with:  patch chat --context-id " + cmd.contextID + " --project " + cmd.project + "\n")
	return 0
}

// kubectlJSON runs kubectl and returns its combined stdout. A non-empty
// kubeconfig is passed via --kubeconfig; otherwise kubectl uses its normal
// resolution (KUBECONFIG env, then ~/.kube/config).
func kubectlJSON(ctx context.Context, kubeconfig string, args ...string) ([]byte, error) {
	full := args
	if kubeconfig != "" {
		full = append([]string{"--kubeconfig", kubeconfig}, args...)
	}
	return exec.CommandContext(ctx, "kubectl", full...).Output()
}

// failKubectl renders a friendly error for a failed kubectl invocation,
// surfacing kubectl's own stderr (carried on ExitError) when present.
func failKubectl(io Io, err error, _ []byte) int {
	io.Err("patch: " + kubectlErrorText(err) + "\n")
	return 1
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
