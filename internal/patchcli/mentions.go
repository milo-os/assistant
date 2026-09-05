// The "@" mention syntax and its parser.
//
// A mention is `@kind/name` — two segments, no namespace. Namespaces are
// deliberately not part of the syntax: [ReadView] already scopes every request
// to one project's control plane (the projects/<p>/control-plane prefix in
// readview.go), so the project is the scope the user is in and the thing the
// service is told about. Within it the picker lists a kind across all
// namespaces, and a mention is a hint for the model to look the resource up
// rather than an address to fetch it from — so a third segment would buy
// precision nobody needs at the cost of a token nobody wants to type.
//
// Parsing is deliberately conservative. The submitted text is prose: it holds
// email addresses, code spans, and sentences that end in a full stop, and none
// of those should turn into a resource reference.
package patchcli

import (
	"regexp"
	"strings"
)

// maxMentions caps how many mentions travel with one message, so a pasted wall
// of text cannot inflate the outgoing metadata.
const maxMentions = 20

// mention is one referenced resource. The JSON tags are the wire shape of the
// `mentions` message-metadata entry the service reads (see internal/a2a).
type mention struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	APIGroup string `json:"apiGroup,omitempty"`
}

// mentionRE matches one `@kind/name` at a word boundary. The leading group is
// what keeps "wells.scot@gmail.com" out: an "@" with a word character in front
// of it is part of that word, not the start of a mention. Names may contain
// dots and dashes (they are RFC 1123 object names), so a trailing run of
// punctuation is trimmed afterwards rather than excluded here.
var mentionRE = regexp.MustCompile(`(?:^|[\s(\[{"'` + "`" + `])@([a-z][a-z0-9.-]{0,62})/([A-Za-z0-9][A-Za-z0-9._-]{0,252})`)

// parseMentions extracts the resource mentions from a submitted message, in the
// order they appear, without duplicates. Text inside code spans or fenced code
// blocks is ignored — that is where someone pastes a manifest or a command, not
// where they point at a resource.
func parseMentions(text string) []mention {
	if !strings.Contains(text, "@") {
		return nil
	}
	scan := maskCodeSpans(text)

	var (
		out  []mention
		seen = map[string]bool{}
	)
	for _, m := range mentionRE.FindAllStringSubmatch(scan, -1) {
		kind, name := m[1], trimMentionName(m[2])
		if name == "" {
			continue
		}
		key := kind + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, mention{Kind: kind, Name: name})
		if len(out) == maxMentions {
			break
		}
	}
	return out
}

// trimMentionName drops the trailing punctuation a name can legally end in but
// almost never does — "check @workload/api-backend." is a sentence, not a
// resource whose name ends in a full stop.
func trimMentionName(name string) string {
	return strings.TrimRight(name, "._-")
}

// maskCodeSpans blanks the contents of fenced blocks and inline code spans,
// keeping the text's length so nothing else has to know they were there. An
// unterminated fence or backtick masks to the end of the text, which is what
// the user sees rendered too.
func maskCodeSpans(text string) string {
	b := []byte(text)
	for i := 0; i < len(b); {
		if b[i] != '`' {
			i++
			continue
		}
		fence := 1
		for i+fence < len(b) && b[i+fence] == '`' {
			fence++
		}
		open := i
		i += fence
		start := i
		for i < len(b) {
			if b[i] == '`' && matchesFence(b, i, fence) {
				break
			}
			i++
		}
		end := min(i, len(b))
		for j := start; j < end; j++ {
			if b[j] != '\n' {
				b[j] = ' '
			}
		}
		// Blank the delimiters too, so a mention cannot straddle one.
		for j := open; j < start; j++ {
			b[j] = ' '
		}
		i = min(i+fence, len(b))
	}
	return string(b)
}

// matchesFence reports whether a run of exactly n backticks starts at i.
func matchesFence(b []byte, i, n int) bool {
	run := 0
	for i+run < len(b) && b[i+run] == '`' {
		run++
	}
	return run == n
}

// resolveMentionGroups fills in each mention's apiGroup from discovered kinds,
// so the service can name the resource unambiguously. A kind that discovery
// never saw (typed by hand, or a failed discovery) keeps an empty group rather
// than being dropped: the user meant it either way.
func resolveMentionGroups(ms []mention, kinds []resourceKind) []mention {
	if len(ms) == 0 || len(kinds) == 0 {
		return ms
	}
	group := make(map[string]string, len(kinds))
	for _, k := range kinds {
		group[k.token] = k.group
	}
	out := make([]mention, 0, len(ms))
	for _, m := range ms {
		m.APIGroup = group[m.Kind]
		out = append(out, m)
	}
	return out
}
