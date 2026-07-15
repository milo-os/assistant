// Package openaicompat adapts the official openai/openai-go v3 client,
// speaking the Chat Completions API, into an [agentcore.Model]. It targets
// an OpenAI-compatible endpoint — in this service, the Envoy AI Gateway
// (MODEL_MODE=gateway).
//
// In gateway mode the service holds no upstream credential: the gateway
// injects it. The adapter therefore sends no Authorization header when no
// API key is configured, and forwards whatever per-request attribution
// headers the loop supplies.
package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/milo-os/assistant/agentcore"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Options configures a [Model]. ModelID and BaseURL are required. APIKey is
// optional: when empty (the gateway posture) no Authorization header is
// sent.
type Options struct {
	// ModelID is the model name sent on each request.
	ModelID string
	// BaseURL is the OpenAI-compatible endpoint base URL (the gateway).
	BaseURL string
	// APIKey is an optional bearer credential. Empty means no Authorization
	// header (the gateway injects the upstream key).
	APIKey string
	// HTTPClient overrides the HTTP client (custom CA, TLS settings). Nil
	// uses the default.
	HTTPClient *http.Client
}

// Model is an [agentcore.Model] backed by the Chat Completions API.
type Model struct {
	client  openai.Client
	modelID string
}

// New constructs a [Model] from opts.
func New(opts Options) *Model {
	reqOpts := []option.RequestOption{option.WithBaseURL(opts.BaseURL)}
	if opts.APIKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(opts.APIKey))
	} else {
		// Gateway posture: clear any environment-derived key and strip the
		// Authorization header outright so the service sends no upstream
		// credential.
		reqOpts = append(reqOpts, option.WithAPIKey(""), option.WithHeaderDel("Authorization"))
	}
	if opts.HTTPClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(opts.HTTPClient))
	}
	return &Model{client: openai.NewClient(reqOpts...), modelID: opts.ModelID}
}

// ModelID implements [agentcore.Model].
func (m *Model) ModelID() string { return m.modelID }

// Stream implements [agentcore.Model]. It opens a streaming Chat Completions
// request (with usage reporting enabled), emits text deltas as they arrive,
// then emits one tool call per requested function call and a terminal
// step-finish carrying normalized usage.
func (m *Model) Stream(ctx context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	params := buildParams(m.modelID, req)

	reqOpts := make([]option.RequestOption, 0, len(req.Headers))
	for k, v := range req.Headers {
		reqOpts = append(reqOpts, option.WithHeader(k, v))
	}

	stream := m.client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)

	return agentcore.StreamFunc(func(send agentcore.SendFunc) {
		acc := openai.ChatCompletionAccumulator{}
		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				if !send(agentcore.StreamPart{Kind: agentcore.StreamPartTextDelta, Text: chunk.Choices[0].Delta.Content}) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			send(agentcore.StreamPart{Kind: agentcore.StreamPartError, FinishReason: agentcore.FinishError, Err: err})
			return
		}

		reason := agentcore.FinishStop
		if len(acc.Choices) > 0 {
			for _, tc := range acc.Choices[0].Message.ToolCalls {
				// The accumulator fills the union's fields directly (no raw
				// JSON), so read Function off the union rather than AsFunction().
				if !send(agentcore.StreamPart{
					Kind:     agentcore.StreamPartToolCall,
					ToolCall: &agentcore.ToolCall{ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments)},
				}) {
					return
				}
			}
			reason = mapFinishReason(acc.Choices[0].FinishReason)
		}

		send(agentcore.StreamPart{
			Kind:         agentcore.StreamPartStepFinish,
			Usage:        mapUsage(acc.Usage),
			FinishReason: reason,
		})
	}, func() { _ = stream.Close() }), nil
}

// mapUsage normalizes Chat Completions usage into [agentcore.Usage].
// prompt_tokens already includes cached tokens, so Input maps to it
// directly; the prompt-token cache breakdown supplies CacheRead/CacheWrite.
func mapUsage(u openai.CompletionUsage) agentcore.Usage {
	return agentcore.Usage{
		Input:      u.PromptTokens,
		Output:     u.CompletionTokens,
		CacheRead:  u.PromptTokensDetails.CachedTokens,
		CacheWrite: u.PromptTokensDetails.CacheWriteTokens,
	}
}

// mapFinishReason maps a Chat Completions finish_reason to a unified reason.
func mapFinishReason(reason string) agentcore.FinishReason {
	switch reason {
	case "tool_calls", "function_call":
		return agentcore.FinishToolCalls
	case "length":
		return agentcore.FinishLength
	default:
		return agentcore.FinishStop
	}
}

func buildParams(modelID string, req agentcore.Request) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model:         shared.ChatModel(modelID),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	if req.MaxOutputTokens > 0 {
		params.MaxTokens = openai.Int(int64(req.MaxOutputTokens))
	}
	if req.System != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(req.System))
	}
	for _, def := range req.Tools {
		params.Tools = append(params.Tools, toolParam(def))
	}
	for _, msg := range req.Messages {
		params.Messages = append(params.Messages, messageParams(msg)...)
	}
	return params
}

// messageParams converts one [agentcore.Message] into one or more Chat
// Completions message params. A tool-results message expands to one tool
// message per result (Chat Completions requires a message per tool_call_id).
func messageParams(msg agentcore.Message) []openai.ChatCompletionMessageParamUnion {
	switch msg.Role {
	case agentcore.RoleAssistant:
		return []openai.ChatCompletionMessageParamUnion{assistantMessage(msg.Content)}
	case agentcore.RoleTool:
		var out []openai.ChatCompletionMessageParamUnion
		for _, part := range msg.Content {
			if part.Kind == agentcore.ContentToolResult && part.ToolResult != nil {
				out = append(out, openai.ToolMessage(part.ToolResult.Content, part.ToolResult.ToolCallID))
			}
		}
		return out
	default: // RoleUser
		return []openai.ChatCompletionMessageParamUnion{openai.UserMessage(textOf(msg.Content))}
	}
}

// assistantMessage builds an assistant message, attaching any tool calls.
func assistantMessage(content []agentcore.ContentPart) openai.ChatCompletionMessageParamUnion {
	var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
	for _, part := range content {
		if part.Kind == agentcore.ContentToolCall && part.ToolCall != nil {
			toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: part.ToolCall.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      part.ToolCall.Name,
						Arguments: string(part.ToolCall.Input),
					},
				},
			})
		}
	}
	if len(toolCalls) == 0 {
		return openai.AssistantMessage(textOf(content))
	}
	assistant := openai.ChatCompletionAssistantMessageParam{ToolCalls: toolCalls}
	if text := textOf(content); text != "" {
		assistant.Content.OfString = openai.String(text)
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}
}

func textOf(content []agentcore.ContentPart) string {
	var s string
	for _, part := range content {
		if part.Kind == agentcore.ContentText {
			if s != "" {
				s += " "
			}
			s += part.Text
		}
	}
	return s
}

// toolParam converts a tool definition into a Chat Completions function
// tool, using the raw JSON schema as the function parameters.
func toolParam(def agentcore.ToolDefinition) openai.ChatCompletionToolUnionParam {
	var params shared.FunctionParameters
	if len(def.InputSchema) > 0 {
		_ = json.Unmarshal(def.InputSchema, &params)
	}
	fn := shared.FunctionDefinitionParam{Name: def.Name, Parameters: params}
	if def.Description != "" {
		fn.Description = openai.String(def.Description)
	}
	return openai.ChatCompletionFunctionTool(fn)
}
