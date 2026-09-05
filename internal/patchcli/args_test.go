package patchcli

import "testing"

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want command
	}{
		{
			name: "card with defaults",
			argv: []string{"card"},
			want: command{kind: KindCard},
		},
		{
			name: "card --json and global flags",
			argv: []string{"card", "--json", "--url", "http://x", "--token", "t"},
			want: command{kind: KindCard, json: true, url: "http://x", token: "t"},
		},
		{
			name: "--flag=value form",
			argv: []string{"card", "--url=http://y", "--token=zzz"},
			want: command{kind: KindCard, url: "http://y", token: "zzz"},
		},
		{
			name: "chat with message and project",
			argv: []string{"chat", "hello world", "--project", "demo"},
			want: command{kind: KindChat, message: "hello world", project: "demo"},
		},
		{
			name: "chat --tui with kubeconfig",
			argv: []string{"chat", "--tui", "--project", "demo", "--kubeconfig", "/kc"},
			want: command{kind: KindChat, project: "demo", tui: true, kubeconfig: "/kc"},
		},
		{
			name: "resume opens the picker",
			argv: []string{"resume", "--project", "demo", "--kubeconfig", "/kc"},
			want: command{kind: KindResume, project: "demo", kubeconfig: "/kc"},
		},
		{
			name: "resume with a context id",
			argv: []string{"resume", "01a05ee5", "--project", "demo"},
			want: command{kind: KindResume, project: "demo", contextID: "01a05ee5"},
		},
		{
			name: "resume requires a project",
			argv: []string{"resume"},
			want: command{kind: kindError, errMsg: "resume: --project <name> is required"},
		},
		{
			name: "chat missing message",
			argv: []string{"chat", "--project", "demo"},
			want: command{kind: kindError, errMsg: "chat: missing message argument (or use --interactive / --tui / --continue)"},
		},
		{
			name: "chat missing project",
			argv: []string{"chat", "hi"},
			want: command{kind: kindError, errMsg: "chat: --project <name> is required"},
		},
		{
			// -c with nothing else means "put me back in my last
			// conversation", which is the full-screen chat.
			name: "chat -c opens the full-screen chat on the last conversation",
			argv: []string{"chat", "-c", "--project", "demo"},
			want: command{kind: KindChat, project: "demo", continueLast: true, tui: true},
		},
		{
			name: "chat --continue with a message stays one-shot",
			argv: []string{"chat", "and the edge-cache one?", "--continue", "--project", "demo"},
			want: command{kind: KindChat, project: "demo", message: "and the edge-cache one?", continueLast: true},
		},
		{
			name: "chat -c -i stays the line-based session",
			argv: []string{"chat", "-c", "-i", "--project", "demo"},
			want: command{kind: KindChat, project: "demo", continueLast: true, interactive: true},
		},
		{
			name: "resume --last skips the picker",
			argv: []string{"resume", "--last", "--project", "demo"},
			want: command{kind: KindResume, project: "demo", continueLast: true},
		},
		{
			name: "resume -c is the same as --last",
			argv: []string{"resume", "-c", "--project", "demo"},
			want: command{kind: KindResume, project: "demo", continueLast: true},
		},
		{
			name: "conversations rename",
			argv: []string{"conversations", "rename", "ctx-1", "dfw quota escalation", "--project", "demo"},
			want: command{kind: KindConvRename, project: "demo", contextID: "ctx-1", name: "dfw quota escalation"},
		},
		{
			// An unquoted multi-word name arrives as several positionals; it
			// should mean what it reads as, not just its first word.
			name: "conversations rename joins unquoted words",
			argv: []string{"conversations", "rename", "ctx-1", "dfw", "quota", "escalation", "--project", "demo"},
			want: command{kind: KindConvRename, project: "demo", contextID: "ctx-1", name: "dfw quota escalation"},
		},
		{
			name: "conversations rename missing name",
			argv: []string{"conversations", "rename", "ctx-1", "--project", "demo"},
			want: command{kind: kindError, errMsg: "conversations rename: missing <name> argument"},
		},
		{
			name: "conversations rename missing id",
			argv: []string{"conversations", "rename", "--project", "demo"},
			want: command{kind: kindError, errMsg: "conversations rename: missing <context-id> argument"},
		},
		{
			name: "compact",
			argv: []string{"compact", "--project", "demo", "--context-id", "ctx-1"},
			want: command{kind: KindCompact, project: "demo", contextID: "ctx-1"},
		},
		{
			name: "compact --json",
			argv: []string{"compact", "--project", "demo", "--context-id", "ctx-1", "--json"},
			want: command{kind: KindCompact, project: "demo", contextID: "ctx-1", json: true},
		},
		{
			name: "compact missing project",
			argv: []string{"compact", "--context-id", "ctx-1"},
			want: command{kind: kindError, errMsg: "compact: --project <name> is required"},
		},
		{
			name: "compact missing context-id",
			argv: []string{"compact", "--project", "demo"},
			want: command{kind: kindError, errMsg: "compact: --context-id <c> is required"},
		},
		{
			name: "conversations list",
			argv: []string{"conversations", "list", "--project", "demo"},
			want: command{kind: KindConvList, project: "demo"},
		},
		{
			name: "conversations show with id and kubeconfig",
			argv: []string{"conversations", "show", "ctx-1", "--project", "demo", "--kubeconfig", "/kc"},
			want: command{kind: KindConvShow, project: "demo", contextID: "ctx-1", kubeconfig: "/kc"},
		},
		{
			name: "conv alias list --json",
			argv: []string{"conv", "list", "--project", "demo", "--json"},
			want: command{kind: KindConvList, project: "demo", json: true},
		},
		{
			name: "conversations missing project",
			argv: []string{"conversations", "list"},
			want: command{kind: kindError, errMsg: "conversations list: --project <name> is required"},
		},
		{
			name: "conversations show missing id",
			argv: []string{"conversations", "show", "--project", "demo"},
			want: command{kind: kindError, errMsg: "conversations show: missing <context-id> argument"},
		},
		{
			name: "conversations bad subcommand",
			argv: []string{"conversations", "delete", "--project", "demo"},
			want: command{kind: kindError, errMsg: `conversations: expected "list", "show" or "rename", got "delete"`},
		},
		{
			name: "gaps list",
			argv: []string{"gaps", "list", "--project", "streamco-platform"},
			want: command{kind: KindGapList, project: "streamco-platform"},
		},
		{
			name: "gap alias list --json --kubeconfig",
			argv: []string{"gap", "list", "--project", "streamco-platform", "--json", "--kubeconfig", "/kc"},
			want: command{kind: KindGapList, project: "streamco-platform", json: true, kubeconfig: "/kc"},
		},
		{
			name: "gaps missing project",
			argv: []string{"gaps", "list"},
			want: command{kind: kindError, errMsg: "gaps list: --project <name> is required"},
		},
		{
			name: "gaps bad subcommand",
			argv: []string{"gaps", "delete", "--project", "streamco-platform"},
			want: command{kind: kindError, errMsg: `gaps: expected "list", got "delete"`},
		},
		{
			name: "task get",
			argv: []string{"task", "get", "t-1"},
			want: command{kind: KindTaskGet, id: "t-1"},
		},
		{
			name: "task cancel --json",
			argv: []string{"task", "cancel", "t-2", "--json"},
			want: command{kind: KindTaskCancel, id: "t-2", json: true},
		},
		{
			name: "task bad subcommand",
			argv: []string{"task", "delete", "t"},
			want: command{kind: kindError, errMsg: `task: expected "get" or "cancel", got "delete"`},
		},
		{
			name: "task get missing id",
			argv: []string{"task", "get"},
			want: command{kind: kindError, errMsg: "task get: missing <id> argument"},
		},
		{
			name: "no args is help",
			argv: []string{},
			want: command{kind: kindHelp},
		},
		{
			name: "--help",
			argv: []string{"--help"},
			want: command{kind: kindHelp},
		},
		{
			name: "-h",
			argv: []string{"-h"},
			want: command{kind: kindHelp},
		},
		{
			name: "unknown command",
			argv: []string{"frobnicate"},
			want: command{kind: kindError, errMsg: `unknown command: "frobnicate"`},
		},
		{
			name: "unknown flag",
			argv: []string{"card", "--nope"},
			want: command{kind: kindError, errMsg: "unknown flag: --nope"},
		},
		{
			name: "value flag missing value",
			argv: []string{"card", "--url"},
			want: command{kind: kindError, errMsg: "--url requires a value"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseArgs(tc.argv)
			if got != tc.want {
				t.Errorf("parseArgs(%v)\n  got  %+v\n  want %+v", tc.argv, got, tc.want)
			}
		})
	}
}
