// Package patchcli is the Datum Cloud assistant (A2A) client shared by the two
// binaries that ship it: `patch` (cmd/patch, the standalone CLI the e2e harness
// drives) and `datumctl assistant` (cmd/milo-assistant, the datumctl plugin). It is
// a thin client over the official a2a-go client, proving the "the service is
// just one client away" architecture with a second consumer.
//
// The two entrypoints differ only in how they resolve the service URL and the
// bearer token — the standalone CLI reads PATCH_URL/PATCH_TOKEN, the plugin
// takes the project from datumctl's injected environment and the token from
// datumctl's credentials helper. Everything past that seam is this package:
// [Run] is the standalone CLI's argv-driven entry, [Invocation.Execute] the
// resolved-input entry a cobra-based plugin builds by hand.
//
// This file is the pure argument parser: it turns argv into a command value
// with NO side effects, so it is unit-testable without touching the network
// or process streams. Env fallbacks (PATCH_URL, PATCH_TOKEN) are resolved by
// Run, not here.
//
// Grammar:
//
//	patch card [--json]
//	patch chat "<message>" --project <p> [--context-id <c>] [--json]
//	patch chat -i --project <p> [--context-id <c>]
//	patch resume [<context-id>] --project <p> [--kubeconfig <k>]
//	patch compact --project <p> --context-id <c> [--json]
//	patch conversations list --project <p> [--json]
//	patch conversations show <context-id> --project <p> [--json]
//	patch gaps list --project <p> [--json]
//	patch task get <id> [--json]
//	patch task cancel <id> [--json]
//
// Global flags (any command): --url <u>, --token <t>, --help/-h.
package patchcli

import "strings"

// kindHelp and kindError are parse-only outcomes — argv asked for help, or was
// malformed. They sit below the executable [Kind] values (which start at 1) so
// they can never be mistaken for a command to run.
const (
	kindHelp Kind = -1 - iota
	kindError
)

// command is the result of parsing argv: a discriminated union flattened into
// a struct. Only the fields relevant to kind are populated.
type command struct {
	kind Kind

	// common options
	json  bool
	url   string
	token string

	// chat
	message     string
	project     string
	contextID   string
	interactive bool
	tui         bool

	// conversations (kubectl against the aggregated apiserver)
	kubeconfig string

	// task get / cancel
	id string

	// error
	errMsg string
}

// parseArgs parses argv (excluding the program name) into a command.
func parseArgs(argv []string) command {
	flags, ferr := extractFlags(argv)
	if ferr != "" {
		return command{kind: kindError, errMsg: ferr}
	}
	if flags.help || len(flags.positionals) == 0 {
		return command{kind: kindHelp}
	}

	common := command{json: flags.json, url: flags.url, token: flags.token}
	name := flags.positionals[0]
	rest := flags.positionals[1:]

	switch name {
	case "card":
		common.kind = KindCard
		return common

	case "chat":
		if !flags.interactive && !flags.tui && (len(rest) == 0 || rest[0] == "") {
			return command{kind: kindError, errMsg: "chat: missing message argument (or use --interactive / --tui)"}
		}
		if flags.project == "" {
			return command{kind: kindError, errMsg: "chat: --project <name> is required"}
		}
		common.kind = KindChat
		if len(rest) > 0 {
			common.message = rest[0]
		}
		common.project = flags.project
		common.contextID = flags.contextID
		common.interactive = flags.interactive
		common.tui = flags.tui
		common.kubeconfig = flags.kubeconfig
		return common

	case "resume":
		// The full-screen chat, opened straight into the conversation
		// picker — or, with a context id, into that conversation with its
		// transcript loaded. Needs both the service (to chat) and the
		// apiserver read view (to list/load), like chat --tui's /resume.
		if flags.project == "" {
			return command{kind: kindError, errMsg: "resume: --project <name> is required"}
		}
		common.kind = KindResume
		common.project = flags.project
		common.kubeconfig = flags.kubeconfig
		if len(rest) > 0 {
			common.contextID = rest[0]
		} else {
			common.contextID = flags.contextID
		}
		return common

	case "compact":
		if flags.project == "" {
			return command{kind: kindError, errMsg: "compact: --project <name> is required"}
		}
		if flags.contextID == "" {
			return command{kind: kindError, errMsg: "compact: --context-id <c> is required"}
		}
		common.kind = KindCompact
		common.project = flags.project
		common.contextID = flags.contextID
		return common

	case "conversations", "conversation", "conv":
		var sub, id string
		if len(rest) > 0 {
			sub = rest[0]
		}
		if len(rest) > 1 {
			id = rest[1]
		}
		if sub != "list" && sub != "show" {
			return command{kind: kindError, errMsg: `conversations: expected "list" or "show", got "` + sub + `"`}
		}
		if flags.project == "" {
			return command{kind: kindError, errMsg: "conversations " + sub + ": --project <name> is required"}
		}
		common.project = flags.project
		common.kubeconfig = flags.kubeconfig
		if sub == "list" {
			common.kind = KindConvList
			return common
		}
		if id == "" {
			return command{kind: kindError, errMsg: "conversations show: missing <context-id> argument"}
		}
		common.kind = KindConvShow
		common.contextID = id
		return common

	case "gaps", "gap":
		// A provider's own read view of capability-gap reports — the project
		// here is the PROVIDER's project (spec.reportingProject), never the
		// consumer project a conversation ran in. Same apiserver read path
		// as conversations: kubectl + your k8s identity, no PATCH_TOKEN.
		var sub string
		if len(rest) > 0 {
			sub = rest[0]
		}
		if sub != "list" {
			return command{kind: kindError, errMsg: `gaps: expected "list", got "` + sub + `"`}
		}
		if flags.project == "" {
			return command{kind: kindError, errMsg: "gaps list: --project <name> is required"}
		}
		common.kind = KindGapList
		common.project = flags.project
		common.kubeconfig = flags.kubeconfig
		return common

	case "task":
		var sub, id string
		if len(rest) > 0 {
			sub = rest[0]
		}
		if len(rest) > 1 {
			id = rest[1]
		}
		if sub != "get" && sub != "cancel" {
			return command{kind: kindError, errMsg: `task: expected "get" or "cancel", got "` + sub + `"`}
		}
		if id == "" {
			return command{kind: kindError, errMsg: "task " + sub + ": missing <id> argument"}
		}
		if sub == "get" {
			common.kind = KindTaskGet
		} else {
			common.kind = KindTaskCancel
		}
		common.id = id
		return common

	default:
		return command{kind: kindError, errMsg: `unknown command: "` + name + `"`}
	}
}

// flags holds the split of argv into recognized flags and leftover positionals.
type flags struct {
	json        bool
	help        bool
	interactive bool
	tui         bool
	url         string
	token       string
	project     string
	contextID   string
	kubeconfig  string
	positionals []string
}

// extractFlags splits argv into flags + positionals. Both `--flag value` and
// `--flag=value` forms are accepted. It returns a non-empty error string when
// a flag is malformed or unknown.
func extractFlags(argv []string) (flags, string) {
	f := flags{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--json":
			f.json = true
		case arg == "--help" || arg == "-h":
			f.help = true
		case arg == "--interactive" || arg == "-i":
			f.interactive = true
		case arg == "--tui":
			f.tui = true
		case arg == "--url" || strings.HasPrefix(arg, "--url="):
			val, consumed, ok := valueFor(arg, argv, i)
			if !ok {
				return f, "--url requires a value"
			}
			f.url = val
			if consumed {
				i++
			}
		case arg == "--token" || strings.HasPrefix(arg, "--token="):
			val, consumed, ok := valueFor(arg, argv, i)
			if !ok {
				return f, "--token requires a value"
			}
			f.token = val
			if consumed {
				i++
			}
		case arg == "--project" || strings.HasPrefix(arg, "--project="):
			val, consumed, ok := valueFor(arg, argv, i)
			if !ok {
				return f, "--project requires a value"
			}
			f.project = val
			if consumed {
				i++
			}
		case arg == "--context-id" || strings.HasPrefix(arg, "--context-id="):
			val, consumed, ok := valueFor(arg, argv, i)
			if !ok {
				return f, "--context-id requires a value"
			}
			f.contextID = val
			if consumed {
				i++
			}
		case arg == "--kubeconfig" || strings.HasPrefix(arg, "--kubeconfig="):
			val, consumed, ok := valueFor(arg, argv, i)
			if !ok {
				return f, "--kubeconfig requires a value"
			}
			f.kubeconfig = val
			if consumed {
				i++
			}
		case strings.HasPrefix(arg, "--"):
			return f, "unknown flag: " + arg
		default:
			f.positionals = append(f.positionals, arg)
		}
	}
	return f, ""
}

// valueFor resolves a flag's value from either `--flag=value` or the next argv
// element. It reports whether the next element was consumed and whether a value
// was found at all (a following `--flag` does not count as a value).
func valueFor(arg string, argv []string, index int) (value string, consumedNext bool, ok bool) {
	if eq := strings.IndexByte(arg, '='); eq != -1 {
		return arg[eq+1:], false, true
	}
	if index+1 >= len(argv) {
		return "", false, false
	}
	next := argv[index+1]
	if strings.HasPrefix(next, "--") {
		return "", false, false
	}
	return next, true, true
}

const usage = `patch — Datum Cloud assistant (A2A) CLI

Usage:
  patch card [--json]
  patch chat "<message>" --project <name> [--context-id <c>] [--json]
  patch chat -i --project <name> [--context-id <c>]
  patch chat --tui --project <name> [--context-id <c>] ["<message>"]
  patch resume [<context-id>] --project <name>
  patch compact --project <name> --context-id <c> [--json]
  patch conversations list --project <name> [--json]
  patch conversations show <context-id> --project <name> [--json]
  patch gaps list --project <name> [--json]
  patch task get <id> [--json]
  patch task cancel <id> [--json]

Options:
  --project <name>    Milo project the task runs against (chat, conversations,
                      gaps — for gaps this is the PROVIDER's own project, see
                      below, not the project a conversation ran in)
  --context-id <c>    Continue an existing conversation (chat); the service
                      replays that conversation's history into the turn
  -i, --interactive   Multi-turn chat session; the conversation id is kept
                      across turns (Ctrl-D or /quit to leave)
      --tui           Full-screen Bubble Tea chat UI: scrollable transcript
                      (↑/↓, pgup/pgdn, or the mouse wheel; esc jumps back to
                      the latest), live-streamed answers rendered as markdown,
                      spinner while the assistant works, tab-completion for
                      slash commands. Slash commands: /resume (search and
                      resume a past conversation, with a live preview of each
                      one's transcript as you move the cursor), /clear (start a
                      fresh one), /compact (force history compaction now,
                      instead of waiting for the automatic threshold),
                      /export (save the transcript to a file),
                      /status (show project/conversation/turn count), /help
                      (list commands), /quit or /exit (leave; Ctrl-C also
                      leaves)
  --url <url>         Service base URL (overrides PATCH_URL)
  --token <token>     Bearer token (overrides PATCH_TOKEN)
  --kubeconfig <p>    Kubeconfig for the conversations apiserver (overrides
                      KUBECONFIG); conversations use your k8s identity, not
                      PATCH_TOKEN. Also used by --tui's /resume picker.
  --json              Emit raw JSON (events for chat, objects otherwise)
  -h, --help          Show this help

Environment:
  PATCH_URL          Service base URL, e.g. http://localhost:7820
  PATCH_TOKEN        Bearer token for the service
  KUBECONFIG         Kubeconfig used by 'conversations' (the apiserver read view)

Resume:
  'resume' opens the full-screen chat straight into the conversation picker:
  a search box over the project's conversations, newest first, each shown by
  its opening message with a live preview of the highlighted one; enter
  resumes it, esc cancels into a fresh chat. With a <context-id> it skips the
  picker and loads that conversation's transcript directly. Needs PATCH_URL/
  PATCH_TOKEN (to chat) and KUBECONFIG (to list and load, like
  'conversations'); the chat --tui has the same picker as /resume.

Compact:
  'compact' forces the assistant to summarize an existing conversation's older
  history right now, instead of waiting for the automatic threshold trigger.
  Same PATCH_URL/PATCH_TOKEN as 'chat' (it calls the assistant service, not
  the apiserver). --context-id must name an existing conversation; if there
  is nothing to compact (empty history, or already reduced to one summary)
  it reports that rather than treating it as a failure. The chat --tui has
  the same thing as /compact.

Conversations:
  'conversations' browses the durable chat history exposed by the
  conversations aggregated apiserver (assistant.miloapis.com) via kubectl —
  a read view under platform authz, separate from the chat transport. Pick a
  context id here, then resume it with 'patch chat --context-id <id>'.

Gaps:
  'gaps' lists capability-gap reports: records a provider service's own team
  can review when the assistant told a user it lacked a tool/lookup/piece of
  knowledge that service should have provided. --project here is the
  PROVIDER's own project (spec.reportingProject on its capability document),
  never the project the conversation that hit the gap ran in — a provider
  only ever sees reports attributed to itself.

Examples:
  PATCH_URL=http://localhost:7820 PATCH_TOKEN=dev-token \
    patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project
  patch chat -i --project demo-project
  patch resume --project demo-project
  patch card --url http://localhost:7820
  patch conversations list --project demo-project
  patch conversations show 019f7293-3579-7d8e-8233-4da8bc900405 --project demo-project
  patch gaps list --project streamco-platform
`
