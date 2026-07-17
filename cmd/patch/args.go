// Package main implements `patch` — a thin A2A client for the Datum Cloud
// assistant service, proving the "the service is just one client away"
// architecture with a second consumer built on the official a2a-go client.
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
//	patch task get <id> [--json]
//	patch task cancel <id> [--json]
//
// Global flags (any command): --url <u>, --token <t>, --help/-h.
package main

import "strings"

// cmdKind is the discriminator for a parsed command.
type cmdKind int

const (
	cmdHelp cmdKind = iota
	cmdError
	cmdCard
	cmdChat
	cmdTaskGet
	cmdTaskCancel
)

// command is the result of parsing argv: a discriminated union flattened into
// a struct. Only the fields relevant to kind are populated.
type command struct {
	kind cmdKind

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

	// task get / cancel
	id string

	// error
	errMsg string
}

// parseArgs parses argv (excluding the program name) into a command.
func parseArgs(argv []string) command {
	flags, ferr := extractFlags(argv)
	if ferr != "" {
		return command{kind: cmdError, errMsg: ferr}
	}
	if flags.help || len(flags.positionals) == 0 {
		return command{kind: cmdHelp}
	}

	common := command{json: flags.json, url: flags.url, token: flags.token}
	name := flags.positionals[0]
	rest := flags.positionals[1:]

	switch name {
	case "card":
		common.kind = cmdCard
		return common

	case "chat":
		if !flags.interactive && !flags.tui && (len(rest) == 0 || rest[0] == "") {
			return command{kind: cmdError, errMsg: "chat: missing message argument (or use --interactive / --tui)"}
		}
		if flags.project == "" {
			return command{kind: cmdError, errMsg: "chat: --project <name> is required"}
		}
		common.kind = cmdChat
		if len(rest) > 0 {
			common.message = rest[0]
		}
		common.project = flags.project
		common.contextID = flags.contextID
		common.interactive = flags.interactive
		common.tui = flags.tui
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
			return command{kind: cmdError, errMsg: `task: expected "get" or "cancel", got "` + sub + `"`}
		}
		if id == "" {
			return command{kind: cmdError, errMsg: "task " + sub + ": missing <id> argument"}
		}
		if sub == "get" {
			common.kind = cmdTaskGet
		} else {
			common.kind = cmdTaskCancel
		}
		common.id = id
		return common

	default:
		return command{kind: cmdError, errMsg: `unknown command: "` + name + `"`}
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
  patch task get <id> [--json]
  patch task cancel <id> [--json]

Options:
  --project <name>    Milo project the task runs against (chat)
  --context-id <c>    Continue an existing conversation (chat); the service
                      replays that conversation's history into the turn
  -i, --interactive   Multi-turn chat session; the conversation id is kept
                      across turns (Ctrl-D or /quit to leave)
      --tui           Full-screen Bubble Tea chat UI: scrollable transcript,
                      live-streamed answers rendered as markdown, spinner while
                      the assistant works (Ctrl-C or /quit to leave)
  --url <url>         Service base URL (overrides PATCH_URL)
  --token <token>     Bearer token (overrides PATCH_TOKEN)
  --json              Emit raw JSON (events for chat, objects otherwise)
  -h, --help          Show this help

Environment:
  PATCH_URL          Service base URL, e.g. http://localhost:7820
  PATCH_TOKEN        Bearer token for the service

Examples:
  PATCH_URL=http://localhost:7820 PATCH_TOKEN=dev-token \
    patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project
  patch chat -i --project demo-project
  patch card --url http://localhost:7820
`
