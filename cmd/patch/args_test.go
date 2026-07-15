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
			name: "chat missing message",
			argv: []string{"chat", "--project", "demo"},
			want: command{kind: cmdError, errMsg: "chat: missing message argument"},
		},
		{
			name: "chat missing project",
			argv: []string{"chat", "hi"},
			want: command{kind: cmdError, errMsg: "chat: --project <name> is required"},
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
