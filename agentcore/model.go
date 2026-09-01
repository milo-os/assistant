package agentcore

import (
	"context"
	"encoding/json"
)

// Role identifies the author of a [Message] in the conversation sent to a
// [Model].
type Role string

const (
	// RoleUser is a message from the end user.
	RoleUser Role = "user"
	// RoleAssistant is a message produced by the model, possibly carrying
	// tool calls.
	RoleAssistant Role = "assistant"
	// RoleTool carries the results of tool executions back to the model. The
	// loop emits exactly one RoleTool message per step (all results from a
	// step are batched into it).
	RoleTool Role = "tool"
)

// Message is one turn in the conversation handed to a [Model]. A message's
// Content is a heterogeneous list of parts; a user or assistant message
// typically holds text and/or tool calls, while a tool message holds tool
// results.
type Message struct {
	Role    Role
	Content []ContentPart
}

// ContentPartKind discriminates the variants of [ContentPart].
type ContentPartKind string

const (
	// ContentText is a plain-text fragment; see ContentPart.Text.
	ContentText ContentPartKind = "text"
	// ContentToolCall is a model request to invoke a tool; see
	// ContentPart.ToolCall.
	ContentToolCall ContentPartKind = "tool-call"
	// ContentToolResult is the outcome of a tool execution; see
	// ContentPart.ToolResult.
	ContentToolResult ContentPartKind = "tool-result"
)

// ContentPart is a single element of a [Message]'s content. Exactly one of
// the typed fields is populated, selected by Kind.
type ContentPart struct {
	Kind       ContentPartKind
	Text       string
	ToolCall   *ToolCall
	ToolResult *ToolResult
}

// TextPart returns a text content part.
func TextPart(text string) ContentPart {
	return ContentPart{Kind: ContentText, Text: text}
}

// UserMessage is a convenience constructor for a single-text user message.
func UserMessage(text string) Message {
	return Message{Role: RoleUser, Content: []ContentPart{TextPart(text)}}
}

// ToolCall is a model's request to invoke a named tool with the given JSON
// input. ID is the provider-assigned identifier that correlates the call
// with its [ToolResult].
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult is the outcome of executing a [ToolCall]. Content is the
// textual payload fed back to the model; IsError marks the result as an
// error so the model can react (see loop rule 5). ToolCallID and Name
// correlate the result with the originating call.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

// ToolDefinition is the model-facing description of a tool: the name the
// model uses to call it, a natural-language description, and a JSON Schema
// (as raw JSON) describing the input. It carries no executable behavior —
// that lives on [Tool].
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Tool is an executable capability the model may invoke. Definition is the
// model-facing schema; Execute runs the tool and returns its textual
// output. A non-nil error from Execute is converted by the loop into an
// error [ToolResult] fed back to the model rather than aborting the run
// (loop rule 5).
type Tool interface {
	Definition() ToolDefinition
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// ToolSet is the collection of tools available to a run, keyed by tool
// name (the same name that appears in [ToolDefinition].Name and in a
// [ToolCall]). A nil or empty ToolSet runs the model with no tools.
type ToolSet map[string]Tool

// Definitions returns the model-facing definitions of every tool in the
// set. Order is unspecified.
func (ts ToolSet) Definitions() []ToolDefinition {
	if len(ts) == 0 {
		return nil
	}
	defs := make([]ToolDefinition, 0, len(ts))
	for _, t := range ts {
		defs = append(defs, t.Definition())
	}
	return defs
}

// Request is a single model inference request. It is what a [Model]
// receives for one step of the loop; the loop rebuilds it with the growing
// message history on each iteration.
type Request struct {
	// System is the system prompt. Empty means no system prompt.
	System string
	// Messages is the conversation so far, oldest first.
	Messages []Message
	// Tools are the model-facing tool definitions available this step.
	Tools []ToolDefinition
	// MaxOutputTokens caps the generated tokens. Zero lets the adapter use
	// its provider default.
	MaxOutputTokens int
	// Headers are extra HTTP headers to attach to this request (used for
	// gateway attribution). Adapters that are not HTTP-based ignore them.
	Headers map[string]string
}

// FinishReason explains why a model step, or the whole run, stopped.
type FinishReason string

const (
	// FinishStop is a normal end of turn (the model chose to stop).
	FinishStop FinishReason = "stop"
	// FinishToolCalls means the step ended because the model emitted tool
	// calls. It appears on a per-step [StreamPartStepFinish]; the loop then
	// executes the tools and continues.
	FinishToolCalls FinishReason = "tool-calls"
	// FinishLength means the step hit its output-token limit. If tool calls
	// were also pending they may be truncated, so the loop finishes
	// explicitly rather than executing them (loop rule 1).
	FinishLength FinishReason = "length"
	// FinishStepLimit means the loop reached its configured step limit and
	// stopped gracefully (loop rule 3).
	FinishStepLimit FinishReason = "step-limit"
	// FinishCanceled means the run's context was canceled between or during
	// steps.
	FinishCanceled FinishReason = "canceled"
	// FinishError means the run ended because of an error; the terminal
	// [StreamPart] of kind [StreamPartError] carries the cause.
	FinishError FinishReason = "error"
)

// Model is a streaming language model. It is the single seam every provider
// adapter implements. Implementations MUST be safe for the loop's usage
// pattern (one in-flight Stream at a time per run) and MUST NOT read
// environment or global state — everything they need is injected at
// construction.
type Model interface {
	// Stream runs one inference over req and returns a reader over the
	// resulting parts. Adapters emit [StreamPartTextDelta] and
	// [StreamPartToolCall] parts as they arrive and MUST emit exactly one
	// terminal [StreamPartStepFinish] carrying the step's [Usage] and
	// [FinishReason]. The caller closes the returned [StreamReader].
	Stream(ctx context.Context, req Request) (StreamReader, error)

	// ModelID reports the concrete model identifier, recorded on usage
	// attribution by callers.
	ModelID() string
}
