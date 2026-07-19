package main

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
			want: command{kind: cmdCard},
		},
		{
			name: "card --json and global flags",
			argv: []string{"card", "--json", "--url", "http://x", "--token", "t"},
			want: command{kind: cmdCard, json: true, url: "http://x", token: "t"},
		},
		{
			name: "--flag=value form",
			argv: []string{"card", "--url=http://y", "--token=zzz"},
			want: command{kind: cmdCard, url: "http://y", token: "zzz"},
		},
		{
			name: "chat with message and project",
			argv: []string{"chat", "hello world", "--project", "demo"},
			want: command{kind: cmdChat, message: "hello world", project: "demo"},
		},
		{
			name: "chat --tui with kubeconfig",
			argv: []string{"chat", "--tui", "--project", "demo", "--kubeconfig", "/kc"},
			want: command{kind: cmdChat, project: "demo", tui: true, kubeconfig: "/kc"},
		},
		{
			name: "chat missing message",
			argv: []string{"chat", "--project", "demo"},
			want: command{kind: cmdError, errMsg: "chat: missing message argument (or use --interactive / --tui)"},
		},
		{
			name: "chat missing project",
			argv: []string{"chat", "hi"},
			want: command{kind: cmdError, errMsg: "chat: --project <name> is required"},
		},
		{
			name: "compact",
			argv: []string{"compact", "--project", "demo", "--context-id", "ctx-1"},
			want: command{kind: cmdCompact, project: "demo", contextID: "ctx-1"},
		},
		{
			name: "compact --json",
			argv: []string{"compact", "--project", "demo", "--context-id", "ctx-1", "--json"},
			want: command{kind: cmdCompact, project: "demo", contextID: "ctx-1", json: true},
		},
		{
			name: "compact missing project",
			argv: []string{"compact", "--context-id", "ctx-1"},
			want: command{kind: cmdError, errMsg: "compact: --project <name> is required"},
		},
		{
			name: "compact missing context-id",
			argv: []string{"compact", "--project", "demo"},
			want: command{kind: cmdError, errMsg: "compact: --context-id <c> is required"},
		},
		{
			name: "conversations list",
			argv: []string{"conversations", "list", "--project", "demo"},
			want: command{kind: cmdConvList, project: "demo"},
		},
		{
			name: "conversations show with id and kubeconfig",
			argv: []string{"conversations", "show", "ctx-1", "--project", "demo", "--kubeconfig", "/kc"},
			want: command{kind: cmdConvShow, project: "demo", contextID: "ctx-1", kubeconfig: "/kc"},
		},
		{
			name: "conv alias list --json",
			argv: []string{"conv", "list", "--project", "demo", "--json"},
			want: command{kind: cmdConvList, project: "demo", json: true},
		},
		{
			name: "conversations missing project",
			argv: []string{"conversations", "list"},
			want: command{kind: cmdError, errMsg: "conversations list: --project <name> is required"},
		},
		{
			name: "conversations show missing id",
			argv: []string{"conversations", "show", "--project", "demo"},
			want: command{kind: cmdError, errMsg: "conversations show: missing <context-id> argument"},
		},
		{
			name: "conversations bad subcommand",
			argv: []string{"conversations", "delete", "--project", "demo"},
			want: command{kind: cmdError, errMsg: `conversations: expected "list" or "show", got "delete"`},
		},
		{
			name: "gaps list",
			argv: []string{"gaps", "list", "--project", "streamco-platform"},
			want: command{kind: cmdGapList, project: "streamco-platform"},
		},
		{
			name: "gap alias list --json --kubeconfig",
			argv: []string{"gap", "list", "--project", "streamco-platform", "--json", "--kubeconfig", "/kc"},
			want: command{kind: cmdGapList, project: "streamco-platform", json: true, kubeconfig: "/kc"},
		},
		{
			name: "gaps missing project",
			argv: []string{"gaps", "list"},
			want: command{kind: cmdError, errMsg: "gaps list: --project <name> is required"},
		},
		{
			name: "gaps bad subcommand",
			argv: []string{"gaps", "delete", "--project", "streamco-platform"},
			want: command{kind: cmdError, errMsg: `gaps: expected "list", got "delete"`},
		},
		{
			name: "task get",
			argv: []string{"task", "get", "t-1"},
			want: command{kind: cmdTaskGet, id: "t-1"},
		},
		{
			name: "task cancel --json",
			argv: []string{"task", "cancel", "t-2", "--json"},
			want: command{kind: cmdTaskCancel, id: "t-2", json: true},
		},
		{
			name: "task bad subcommand",
			argv: []string{"task", "delete", "t"},
			want: command{kind: cmdError, errMsg: `task: expected "get" or "cancel", got "delete"`},
		},
		{
			name: "task get missing id",
			argv: []string{"task", "get"},
			want: command{kind: cmdError, errMsg: "task get: missing <id> argument"},
		},
		{
			name: "no args is help",
			argv: []string{},
			want: command{kind: cmdHelp},
		},
		{
			name: "--help",
			argv: []string{"--help"},
			want: command{kind: cmdHelp},
		},
		{
			name: "-h",
			argv: []string{"-h"},
			want: command{kind: cmdHelp},
		},
		{
			name: "unknown command",
			argv: []string{"frobnicate"},
			want: command{kind: cmdError, errMsg: `unknown command: "frobnicate"`},
		},
		{
			name: "unknown flag",
			argv: []string{"card", "--nope"},
			want: command{kind: cmdError, errMsg: "unknown flag: --nope"},
		},
		{
			name: "value flag missing value",
			argv: []string{"card", "--url"},
			want: command{kind: cmdError, errMsg: "--url requires a value"},
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
