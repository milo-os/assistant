// The cobra command tree for `datumctl patch`, and the three seams where it
// differs from the standalone CLI: the project comes from datumctl's injected
// environment, the token from datumctl's credentials helper, and the service
// URL from --url/PATCH_URL.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.datum.net/datumctl/plugin"

	"github.com/milo-os/assistant/internal/patchcli"
)

// tokenTimeout bounds one call to the credentials helper. plugin.Token() takes
// no context and shells out to datumctl with no deadline of its own, so a
// wedged helper would otherwise hang the plugin indefinitely.
const tokenTimeout = 10 * time.Second

func newRootCmd() *cobra.Command {
	// --org, --project and -o/--output come from the SDK, defaulted from the
	// DATUM_* variables datumctl injects.
	root := plugin.NewRootCmd("patch", "Chat with Patch, the Datum Cloud assistant")
	root.Long = "Chat with Patch, the Datum Cloud assistant, from the terminal.\n\n" +
		"Conversations are held by the assistant service and continued with --context-id.\n" +
		"'conversations' and 'gaps' read the aggregated apiserver with your own\n" +
		"Kubernetes identity instead, so they need a kubeconfig rather than a token."
	root.SilenceUsage = true
	root.SilenceErrors = true

	root.PersistentFlags().String("url", "",
		"Assistant service base URL (defaults to PATCH_URL)")
	root.PersistentFlags().String("kubeconfig", "",
		"Kubeconfig for the conversations apiserver (defaults to KUBECONFIG)")

	root.AddCommand(
		newCardCmd(),
		newChatCmd(),
		newCompactCmd(),
		newConversationsCmd(),
		newGapsCmd(),
		newTaskCmd(),
	)
	return root
}

func newCardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "card",
		Short: "Show the assistant's A2A agent card",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := serviceInvocation(cmd, patchcli.KindCard, false)
			if err != nil {
				return err
			}
			return run(cmd, inv)
		},
	}
}

func newChatCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chat [message]",
		Short: "Send a message to the assistant",
		Long: "Send one message and stream the answer, or hold a session with\n" +
			"--interactive (line-based) or --tui (full-screen).\n\n" +
			"The conversation id is reported on stderr; pass it back with\n" +
			"--context-id to continue that conversation.",
		Args: cobra.MaximumNArgs(1),
		Example: "  datumctl patch chat \"Why is the api-backend workload not available?\"\n" +
			"  datumctl patch chat --tui\n" +
			"  datumctl patch chat \"and the edge-cache one?\" --context-id 01a05ee5-…",
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, err := serviceInvocation(cmd, patchcli.KindChat, true)
			if err != nil {
				return err
			}
			if len(args) > 0 {
				inv.Message = args[0]
			}
			inv.Interactive, _ = cmd.Flags().GetBool("interactive")
			inv.TUI, _ = cmd.Flags().GetBool("tui")
			inv.ContextID, _ = cmd.Flags().GetString("context-id")
			if inv.Message == "" && !inv.Interactive && !inv.TUI {
				return fmt.Errorf("chat: missing message argument (or use --interactive / --tui)")
			}
			return run(cmd, inv)
		},
	}
	cmd.Flags().BoolP("interactive", "i", false, "Hold a multi-turn session on one conversation")
	cmd.Flags().Bool("tui", false, "Full-screen chat UI with a scrollable, markdown-rendered transcript")
	cmd.Flags().String("context-id", "", "Continue an existing conversation")
	return cmd
}

func newCompactCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compact",
		Short: "Summarize a conversation's older history now",
		Long: "Force history compaction for one conversation instead of waiting for\n" +
			"the assistant's automatic threshold. Reports rather than fails when\n" +
			"there is nothing to compact.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := serviceInvocation(cmd, patchcli.KindCompact, true)
			if err != nil {
				return err
			}
			inv.ContextID, _ = cmd.Flags().GetString("context-id")
			if inv.ContextID == "" {
				return fmt.Errorf("compact: --context-id is required")
			}
			return run(cmd, inv)
		},
	}
	cmd.Flags().String("context-id", "", "Conversation to compact (required)")
	return cmd
}

func newConversationsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "conversations",
		Aliases: []string{"conversation", "conv"},
		Short:   "Browse your durable chat history",
		Long: "Read the conversations the assistant has stored for a project, through\n" +
			"the aggregated apiserver. This uses your own Kubernetes identity\n" +
			"(kubeconfig), not the assistant's bearer token.",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List conversations in a project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := readViewInvocation(cmd, patchcli.KindConvList)
			if err != nil {
				return err
			}
			return run(cmd, inv)
		},
	}
	show := &cobra.Command{
		Use:   "show <context-id>",
		Short: "Print one conversation's transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, err := readViewInvocation(cmd, patchcli.KindConvShow)
			if err != nil {
				return err
			}
			inv.ContextID = args[0]
			return run(cmd, inv)
		},
	}
	cmd.AddCommand(list, show)
	return cmd
}

func newGapsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "gaps",
		Aliases: []string{"gap"},
		Short:   "List capability-gap reports filed against your service",
		Long: "Capability-gap reports record where the assistant told a user it lacked\n" +
			"a tool, lookup, or piece of knowledge your service should have supplied.\n\n" +
			"--project here is the PROVIDER's own project — the one named as the\n" +
			"reporting project on its capability document — never the project the\n" +
			"conversation that hit the gap ran in.",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List a provider project's capability-gap reports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := readViewInvocation(cmd, patchcli.KindGapList)
			if err != nil {
				return err
			}
			return run(cmd, inv)
		},
	}
	cmd.AddCommand(list)
	return cmd
}

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Inspect or cancel an A2A task",
	}
	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Show one task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, err := serviceInvocation(cmd, patchcli.KindTaskGet, false)
			if err != nil {
				return err
			}
			inv.ID = args[0]
			return run(cmd, inv)
		},
	}
	cancel := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel one task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, err := serviceInvocation(cmd, patchcli.KindTaskCancel, false)
			if err != nil {
				return err
			}
			inv.ID = args[0]
			return run(cmd, inv)
		},
	}
	cmd.AddCommand(get, cancel)
	return cmd
}

// run executes a resolved invocation and folds its exit code into an error
// cobra can carry back to main.
func run(cmd *cobra.Command, inv patchcli.Invocation) error {
	if code := inv.Execute(cmd.Context(), patchcli.StdIO()); code != 0 {
		return exitError{code: code}
	}
	return nil
}

// serviceInvocation resolves the pieces every assistant-service command needs:
// output format, endpoint, token source, and — where the command is scoped to
// one — the project.
func serviceInvocation(cmd *cobra.Command, kind patchcli.Kind, needProject bool) (patchcli.Invocation, error) {
	inv := patchcli.Invocation{Kind: kind}

	jsonOut, err := jsonOutput(cmd)
	if err != nil {
		return inv, err
	}
	inv.JSON = jsonOut

	inv.BaseURL, err = serviceURL(cmd)
	if err != nil {
		return inv, err
	}
	inv.Token = tokenSource()

	if needProject {
		if inv.Project, err = project(cmd); err != nil {
			return inv, err
		}
	}
	inv.Kubeconfig, _ = cmd.Flags().GetString("kubeconfig")
	return inv, nil
}

// readViewInvocation resolves an apiserver read-view command. These reach the
// aggregated apiserver through kubectl and the caller's own Kubernetes
// identity, so they need neither the service URL nor a token.
func readViewInvocation(cmd *cobra.Command, kind patchcli.Kind) (patchcli.Invocation, error) {
	inv := patchcli.Invocation{Kind: kind}

	jsonOut, err := jsonOutput(cmd)
	if err != nil {
		return inv, err
	}
	inv.JSON = jsonOut

	if inv.Project, err = project(cmd); err != nil {
		return inv, err
	}
	inv.Kubeconfig, _ = cmd.Flags().GetString("kubeconfig")
	return inv, nil
}

// serviceURL resolves the assistant endpoint: --url, then PATCH_URL, then the
// service's own advertised address.
//
// Discovery is last so an explicit override always wins — pointing at a local
// or preview instance must not require unsetting anything. It asks the
// aggregated apiserver, which this CLI already reaches with the caller's
// Kubernetes identity for `conversations` and `gaps`: no new credential, and no
// hostname convention derived from DATUM_API_HOST, which names the Milo control
// plane rather than the assistant.
func serviceURL(cmd *cobra.Command) (string, error) {
	if url, _ := cmd.Flags().GetString("url"); url != "" {
		return url, nil
	}
	if url := os.Getenv("PATCH_URL"); url != "" {
		return url, nil
	}
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
	url, err := patchcli.DiscoverBaseURL(cmd.Context(), kubeconfig)
	if err != nil {
		// Report what discovery hit, then how to proceed anyway — a bare "set
		// PATCH_URL" hides a fixable cause (no kubeconfig, apiserver not
		// installed, PUBLIC_BASE_URL unset on the service).
		return "", fmt.Errorf("could not discover the assistant: %w\n"+
			"       pass --url or set PATCH_URL to skip discovery", err)
	}
	return url, nil
}

// project resolves the Milo project the request runs against, from --project
// (which the SDK defaults to datumctl's injected DATUM_PROJECT).
//
// The service answers a missing project with a JSON-RPC -32602 that reads as a
// malformed request rather than a missing setting, so catch it here and say
// what to do about it.
func project(cmd *cobra.Command) (string, error) {
	name, _ := cmd.Flags().GetString("project")
	if name == "" {
		return "", fmt.Errorf("no project set — pass --project or run 'datumctl context use <context>'")
	}
	return name, nil
}

// jsonOutput maps datumctl's -o flag onto the two renderings this CLI has.
func jsonOutput(cmd *cobra.Command) (bool, error) {
	switch format, _ := cmd.Flags().GetString("output"); format {
	case "json":
		return true, nil
	case "", "table", "text":
		return false, nil
	default:
		return false, fmt.Errorf("unsupported output format %q: one of json|table", format)
	}
}

// tokenSource returns a [patchcli.TokenSource] backed by datumctl's credentials
// helper, bounded by [tokenTimeout].
//
// It is resolved per request rather than once at startup because the helper
// mints short-lived tokens: a `chat --tui` session outlives the token it
// started with, and asking again is the only way to stay authenticated.
func tokenSource() patchcli.TokenSource {
	return func() (string, error) {
		type result struct {
			token string
			err   error
		}
		// Buffered, so the goroutine still completes and exits if we time out.
		done := make(chan result, 1)
		go func() {
			token, err := plugin.Token()
			done <- result{token, err}
		}()

		select {
		case r := <-done:
			if r.err != nil {
				return "", fmt.Errorf("getting credentials from datumctl: %w", r.err)
			}
			return r.token, nil
		case <-time.After(tokenTimeout):
			return "", fmt.Errorf("timed out after %s waiting for datumctl to return a token", tokenTimeout)
		}
	}
}
