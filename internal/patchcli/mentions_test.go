package patchcli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMentions(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string // "kind/name" pairs, in order
	}{
		{"none", "why is the api backend unhappy?", nil},
		{"one", "look at @workload/api-backend please", []string{"workload/api-backend"}},
		{"at start", "@httpproxy/edge is 502ing", []string{"httpproxy/edge"}},
		{"several", "compare @workload/a and @httpproxy/b", []string{"workload/a", "httpproxy/b"}},
		{"duplicates collapse", "@workload/a vs @workload/a", []string{"workload/a"}},
		{"dotted name", "@workload/api.backend-1 status", []string{"workload/api.backend-1"}},

		// The edge cases that make an "@" in prose not a mention.
		{"email address", "mail wells.scot@gmail.com about it", nil},
		{"handle mid-word", "see foo@bar/baz", nil},
		{"trailing full stop", "check @workload/api-backend.", []string{"workload/api-backend"}},
		{"trailing comma", "@workload/a, @httpproxy/b!", []string{"workload/a", "httpproxy/b"}},
		{"in parentheses", "(@workload/api-backend) is down", []string{"workload/api-backend"}},
		{"inline code span", "run `kubectl get @workload/api-backend` first", nil},
		{"fenced block", "```\n@workload/api-backend\n```", nil},
		{"outside a code span", "`kubectl` then @workload/api-backend", []string{"workload/api-backend"}},
		{"no name", "what does @workload/ mean", nil},
		{"kind only", "what is a @workload anyway", nil},
		{"uppercase kind", "@Workload/api-backend", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMentions(tc.text)
			var labels []string
			for _, m := range got {
				labels = append(labels, m.Kind+"/"+m.Name)
			}
			if strings.Join(labels, ",") != strings.Join(tc.want, ",") {
				t.Errorf("parseMentions(%q) = %v, want %v", tc.text, labels, tc.want)
			}
		})
	}
}

// A pasted wall of references must not turn into unbounded metadata.
func TestParseMentionsCapped(t *testing.T) {
	var b strings.Builder
	for i := range maxMentions + 10 {
		b.WriteString("@workload/w")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(string(rune('a' + i/26)))
		b.WriteByte(' ')
	}
	if got := len(parseMentions(b.String())); got != maxMentions {
		t.Fatalf("got %d mentions, want the cap of %d", got, maxMentions)
	}
}

func TestResolveMentionGroups(t *testing.T) {
	kinds := []resourceKind{
		{token: "workload", group: "compute.datumapis.com"},
		{token: "httpproxy", group: "networking.datumapis.com"},
	}
	got := resolveMentionGroups(parseMentions("@workload/a @httpproxy/b @unknown/c"), kinds)
	if len(got) != 3 {
		t.Fatalf("got %d mentions, want 3", len(got))
	}
	if got[0].APIGroup != "compute.datumapis.com" || got[1].APIGroup != "networking.datumapis.com" {
		t.Errorf("groups not resolved: %+v", got)
	}
	// A kind discovery never saw is still the user's intent; it keeps its name
	// and loses only the group.
	if got[2].Kind != "unknown" || got[2].APIGroup != "" {
		t.Errorf("unknown kind should survive without a group: %+v", got[2])
	}
}

// The outgoing message carries the mentions beside projectName, and keeps the
// literal "@kind/name" in the text so the transcript reads as it was typed.
func TestBuildMessageCarriesMentions(t *testing.T) {
	text := "why is @workload/api-backend down?"
	msg := buildMessage(text, "demo-project", "ctx-1", parseMentions(text))

	if msg.Metadata["projectName"] != "demo-project" {
		t.Fatalf("projectName metadata lost: %+v", msg.Metadata)
	}
	if got := msg.Parts[0].Text(); got != text {
		t.Errorf("text = %q, want the literal mention preserved", got)
	}

	raw, err := json.Marshal(msg.Metadata["mentions"])
	if err != nil {
		t.Fatalf("mentions metadata is not JSON-encodable: %v", err)
	}
	var decoded []mention
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode mentions: %v", err)
	}
	if len(decoded) != 1 || decoded[0].Kind != "workload" || decoded[0].Name != "api-backend" {
		t.Fatalf("mentions = %+v", decoded)
	}
}

// No mentions means no key at all, so an ordinary message's metadata is exactly
// what it was before this feature existed.
func TestBuildMessageOmitsEmptyMentions(t *testing.T) {
	msg := buildMessage("plain question", "demo-project", "", nil)
	if _, ok := msg.Metadata["mentions"]; ok {
		t.Fatalf("empty mentions should not be sent: %+v", msg.Metadata)
	}
}
