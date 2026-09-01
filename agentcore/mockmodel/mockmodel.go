// Package mockmodel provides a scriptable, in-process [agentcore.Model] that
// needs no API key and no network. It exists so the full chat path — a tool
// call over a real tool, the tool result folded into a final answer, and
// usage reported — is provable in tests and in MODEL_MODE=mock without a
// language-model provider.
//
// The script (ported verbatim from the TypeScript service's mock model):
//
//  1. If the latest user message mentions "diagnose" AND a tool whose name
//     matches /pipeline_diagnose/i is available, emit a single tool call to
//     it. The loop executes the tool (a real round-trip when composed
//     against a live MCP server).
//  2. On the follow-up step the prompt now carries the tool result, so the
//     mock emits final text quoting the tool's findings.
//  3. Otherwise it emits a short generic reply.
//
// Every response reports fake-but-nonzero token usage ([Usage]).
//
// PARITY CAVEAT: this is a canned script, not a language model. It proves
// plumbing and event shapes, not answer quality or real tool selection.
package mockmodel

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/milo-os/assistant/agentcore"
)

// ModelID is the model identifier reported by the mock model and recorded
// on usage attribution.
const ModelID = "patch-mock-v0"

// Usage is the fake-but-nonzero token usage every mock response reports. It
// mirrors the TypeScript mock's MOCK_USAGE (42 input, 23 output, no cache).
var Usage = agentcore.Usage{Input: 42, Output: 23}

var (
	recallRe       = regexp.MustCompile(`(?i)what did i (say|ask)`)
	skillRe        = regexp.MustCompile("(?i)use the `?([a-zA-Z0-9_-]+)`? skill")
	diagnoseRe     = regexp.MustCompile(`(?i)diagnose`)
	pipelineToolRe = regexp.MustCompile(`(?i)pipeline_diagnose`)
	explicitIDRe   = regexp.MustCompile(`(?i)\bp-[a-z0-9]+\b`)
	afterPipeRe    = regexp.MustCompile(`(?i)pipeline\s+([^\s.,;]+)`)
)

// Model is a scriptable [agentcore.Model]. The zero value is ready to use;
// construct one with [New].
type Model struct{}

// New returns a mock model.
func New() *Model { return &Model{} }

// ModelID implements [agentcore.Model].
func (m *Model) ModelID() string { return ModelID }

// Stream implements [agentcore.Model] by running the canned script over the
// request and returning the resulting parts.
func (m *Model) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	parts := m.script(req)
	return newSliceStream(parts), nil
}

func (m *Model) script(req agentcore.Request) []agentcore.StreamPart {
	if name, result, ok := latestToolResultNamed(req.Messages); ok {
		if name == "load_skill" {
			return textParts(summarizeSkill(result))
		}
		return textParts(summarizeToolResult(result))
	}

	userText := latestUserText(req.Messages)
	if recallRe.MatchString(userText) {
		return textParts(recallReply(priorUserTexts(req.Messages)))
	}
	if m := skillRe.FindStringSubmatch(userText); len(m) > 1 && hasTool(req.Tools, "load_skill") {
		return loadSkillParts(m[1])
	}
	if diagnoseRe.MatchString(userText) {
		if name, ok := findDiagnoseTool(req.Tools); ok {
			return toolCallParts(name, extractPipelineID(userText))
		}
	}
	return textParts(genericReply(userText))
}

// ── Prompt inspection ─────────────────────────────────────────

func latestUserText(messages []agentcore.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != agentcore.RoleUser {
			continue
		}
		var b strings.Builder
		for _, part := range messages[i].Content {
			if part.Kind == agentcore.ContentText {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(part.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// latestToolResultNamed returns the tool name and text of the most recent
// tool result in the conversation, or ok=false if the model has not yet seen
// a tool result.
func latestToolResultNamed(messages []agentcore.Message) (string, string, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		content := messages[i].Content
		for j := len(content) - 1; j >= 0; j-- {
			if content[j].Kind == agentcore.ContentToolResult && content[j].ToolResult != nil {
				return content[j].ToolResult.Name, content[j].ToolResult.Content, true
			}
		}
	}
	return "", "", false
}

func hasTool(tools []agentcore.ToolDefinition, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// priorUserTexts returns the text of every user message BEFORE the latest
// one — i.e. what conversation-history replay put back into the prompt. A
// single-turn prompt yields nothing, which is exactly what makes the recall
// script a history-replay probe.
func priorUserTexts(messages []agentcore.Message) []string {
	var texts []string
	for _, msg := range messages {
		if msg.Role != agentcore.RoleUser {
			continue
		}
		var b strings.Builder
		for _, part := range msg.Content {
			if part.Kind == agentcore.ContentText {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(part.Text)
			}
		}
		if t := strings.TrimSpace(b.String()); t != "" {
			texts = append(texts, t)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return texts[:len(texts)-1]
}

func findDiagnoseTool(tools []agentcore.ToolDefinition) (string, bool) {
	for _, t := range tools {
		if pipelineToolRe.MatchString(t.Name) {
			return t.Name, true
		}
	}
	return "", false
}

func extractPipelineID(userText string) string {
	if m := explicitIDRe.FindString(userText); m != "" {
		return m
	}
	if m := afterPipeRe.FindStringSubmatch(userText); len(m) > 1 && m[1] != "" {
		return m[1]
	}
	return "p-1"
}

// ── Response templates ────────────────────────────────────────

func summarizeToolResult(toolResultText string) string {
	compact := strings.Join(strings.Fields(toolResultText), " ")
	if len(compact) > 800 {
		compact = compact[:800]
	}
	return "Ran the pipeline diagnosis. The provider tool reported: " + compact +
		". In short, that's the signal to chase down."
}

// recallReply answers "what did I say?" by quoting the earlier user turns the
// prompt carried. Deterministic on purpose: e2e asserts the exact quote to
// prove history replay reached the model.
func recallReply(prior []string) string {
	if len(prior) == 0 {
		return "This is the first thing you've said in this conversation — I have no earlier messages from you."
	}
	quoted := make([]string, len(prior))
	for i, t := range prior {
		quoted[i] = `"` + truncate(t, 200) + `"`
	}
	return "Earlier in this conversation you said: " + strings.Join(quoted, ", then ") + "."
}

// summarizeSkill quotes a loaded skill body — deterministic proof that the
// body round-tripped through load_skill into the prompt.
func summarizeSkill(body string) string {
	compact := strings.Join(strings.Fields(body), " ")
	if len(compact) > 800 {
		compact = compact[:800]
	}
	return "Loaded the skill. Following its procedure: " + compact
}

func genericReply(userText string) string {
	if userText == "" {
		return "I'm Patch, the Datum Cloud assistant. Ask me about this project, its resources, or a provider service entitled to it."
	}
	return `I'm Patch (running in mock mode, so this is a canned reply). You said: "` +
		truncate(userText, 200) + `". Ask me to diagnose a provider pipeline to see the tool path in action.`
}

func truncate(text string, max int) string {
	if len(text) > max {
		return text[:max] + "…"
	}
	return text
}

// ── Stream part builders ──────────────────────────────────────

// textParts renders text as incremental deltas (~6-word chunks) followed by
// a stop step-finish carrying the mock usage.
func textParts(text string) []agentcore.StreamPart {
	var parts []agentcore.StreamPart
	for _, chunk := range chunkText(text) {
		parts = append(parts, agentcore.StreamPart{Kind: agentcore.StreamPartTextDelta, Text: chunk})
	}
	parts = append(parts, agentcore.StreamPart{
		Kind:         agentcore.StreamPartStepFinish,
		Usage:        Usage,
		FinishReason: agentcore.FinishStop,
	})
	return parts
}

// loadSkillParts emits a call to the built-in skill loader.
func loadSkillParts(skillName string) []agentcore.StreamPart {
	input, _ := json.Marshal(map[string]string{"skill": skillName})
	return []agentcore.StreamPart{
		{
			Kind: agentcore.StreamPartToolCall,
			ToolCall: &agentcore.ToolCall{
				ID:    "mock-skill-call-0",
				Name:  "load_skill",
				Input: input,
			},
		},
		{
			Kind:         agentcore.StreamPartStepFinish,
			Usage:        Usage,
			FinishReason: agentcore.FinishToolCalls,
		},
	}
}

// toolCallParts emits a single tool call plus a tool-calls step-finish.
func toolCallParts(toolName, pipelineID string) []agentcore.StreamPart {
	input, _ := json.Marshal(map[string]string{"id": pipelineID})
	return []agentcore.StreamPart{
		{
			Kind: agentcore.StreamPartToolCall,
			ToolCall: &agentcore.ToolCall{
				ID:    "mock-tool-call-0",
				Name:  toolName,
				Input: input,
			},
		},
		{
			Kind:         agentcore.StreamPartStepFinish,
			Usage:        Usage,
			FinishReason: agentcore.FinishToolCalls,
		},
	}
}

// chunkText splits text into ~6-word chunks so the stream shows incremental
// text. Whitespace runs are preserved on the token that carries them.
func chunkText(text string) []string {
	tokens := splitKeepSpace(text)
	var chunks []string
	var buf strings.Builder
	wordCount := 0
	for _, tok := range tokens {
		buf.WriteString(tok)
		if strings.TrimSpace(tok) != "" {
			wordCount++
		}
		if wordCount >= 6 {
			chunks = append(chunks, buf.String())
			buf.Reset()
			wordCount = 0
		}
	}
	if buf.Len() > 0 {
		chunks = append(chunks, buf.String())
	}
	if len(chunks) == 0 {
		return []string{text}
	}
	return chunks
}

// splitKeepSpace splits text into alternating non-space and space runs,
// dropping empties, so whitespace is retained across chunk boundaries.
func splitKeepSpace(text string) []string {
	var tokens []string
	var buf strings.Builder
	var inSpace bool
	flush := func() {
		if buf.Len() > 0 {
			tokens = append(tokens, buf.String())
			buf.Reset()
		}
	}
	for i, r := range text {
		isSpace := r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
		if i == 0 {
			inSpace = isSpace
		}
		if isSpace != inSpace {
			flush()
			inSpace = isSpace
		}
		buf.WriteRune(r)
	}
	flush()
	return tokens
}
