// Package anthropic adapts the official anthropics/anthropic-sdk-go into an
// [agentcore.Model]. It targets the Anthropic Messages API directly
// (MODEL_MODE=anthropic).
//
// The adapter owns no policy: it forwards whatever per-request headers the
// loop supplies and sends an Authorization header only when an API key is
// configured. Attribution headers and credential isolation are decided by
// the caller, keeping this adapter provider-faithful and reusable.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/milo-os/assistant/agentcore"
)

// DefaultMaxTokens is the max_tokens sent when a request does not specify
// one (the Anthropic API requires the field).
const DefaultMaxTokens = 4096

// Options configures a [Model]. ModelID is required. APIKey enables the
// standard Anthropic endpoint; leave it empty (and set BaseURL) to route
// through a gateway that injects the upstream credential itself, in which
// case no Authorization header is sent.
type Options struct {
	// ModelID is the Anthropic model id (e.g. "claude-sonnet-4-6").
	ModelID string
	// APIKey is the Anthropic API key. Empty means no Authorization header.
	APIKey string
	// BaseURL overrides the API base URL. Empty uses the SDK default.
	BaseURL string
	// HTTPClient overrides the HTTP client (custom CA, TLS settings). Nil
	// uses the SDK default.
	HTTPClient *http.Client
}

// Model is an [agentcore.Model] backed by the Anthropic Messages API.
type Model struct {
	client  anthropic.Client
	modelID string
}

// New constructs a [Model] from opts.
func New(opts Options) *Model {
	// Disable the SDK's own retry loop: retry policy is owned by the agentcore
	// loop, which classifies failures, honors Retry-After, applies our backoff,
	// and is bounded by the per-turn deadline. Leaving the SDK's default (2)
	// enabled would compound with ours and multiply the attempt count.
	reqOpts := []option.RequestOption{option.WithMaxRetries(0)}
	if opts.APIKey != "" {
		reqOpts = append(reqOpts, option.WithAPIKey(opts.APIKey))
	} else {
		// Keyless (gateway) posture: explicitly clear any environment-derived
		// key and install a passthrough middleware. The SDK otherwise refuses
		// to send a credential-less request; with the key cleared and an extra
		// middleware present it sends no Authorization/X-Api-Key header, which
		// is exactly what a gateway that injects the upstream credential wants.
		reqOpts = append(reqOpts, option.WithAPIKey(""), option.WithMiddleware(passthroughMiddleware))
	}
	if opts.BaseURL != "" {
		reqOpts = append(reqOpts, option.WithBaseURL(opts.BaseURL))
	}
	if opts.HTTPClient != nil {
		reqOpts = append(reqOpts, option.WithHTTPClient(opts.HTTPClient))
	}
	return &Model{client: anthropic.NewClient(reqOpts...), modelID: opts.ModelID}
}

// passthroughMiddleware forwards the request unchanged. It exists only to
// satisfy the SDK's keyless-request guard (see [New]).
func passthroughMiddleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	return next(req)
}

// ModelID implements [agentcore.Model].
func (m *Model) ModelID() string { return m.modelID }

// Stream implements [agentcore.Model]. It opens a streaming Messages
// request, emits text deltas as they arrive, then emits one tool call per
// tool_use block and a terminal step-finish carrying normalized usage.
func (m *Model) Stream(ctx context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	params, err := buildParams(m.modelID, req)
	if err != nil {
		return nil, err
	}

	reqOpts := make([]option.RequestOption, 0, len(req.Headers)+1)
	for k, v := range req.Headers {
		reqOpts = append(reqOpts, option.WithHeader(k, v))
	}
	// Capture the raw response: a misrouted request (e.g. a proxy that serves
	// HTML or ignores stream:true and returns a JSON object with HTTP 200)
	// yields an SSE parser that finds no events and ends cleanly — which would
	// otherwise masquerade as a completed, empty, zero-token turn. Empty
	// answers must be errors, not silent successes.
	var httpRes *http.Response
	reqOpts = append(reqOpts, option.WithResponseInto(&httpRes))

	stream := m.client.Messages.NewStreaming(ctx, params, reqOpts...)

	return agentcore.StreamFunc(func(send agentcore.SendFunc) {
		acc := anthropic.Message{}
		sawEvent := false
		for stream.Next() {
			sawEvent = true
			event := stream.Current()
			if err := acc.Accumulate(event); err != nil {
				send(agentcore.StreamPart{Kind: agentcore.StreamPartError, FinishReason: agentcore.FinishError, Err: err})
				return
			}
			if delta, ok := textDelta(event); ok {
				if !send(agentcore.StreamPart{Kind: agentcore.StreamPartTextDelta, Text: delta}) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			send(agentcore.StreamPart{Kind: agentcore.StreamPartError, FinishReason: agentcore.FinishError, Err: classify(err)})
			return
		}
		if !sawEvent {
			status := "unknown"
			if httpRes != nil {
				status = httpRes.Status
			}
			send(agentcore.StreamPart{
				Kind:         agentcore.StreamPartError,
				FinishReason: agentcore.FinishError,
				Err:          fmt.Errorf("anthropic: upstream returned no stream events (HTTP %s) — check the base URL routes /v1/messages and honors stream:true", status),
			})
			return
		}

		for _, block := range acc.Content {
			if block.Type != "tool_use" {
				continue
			}
			tu := block.AsToolUse()
			if !send(agentcore.StreamPart{
				Kind:     agentcore.StreamPartToolCall,
				ToolCall: &agentcore.ToolCall{ID: tu.ID, Name: tu.Name, Input: tu.Input},
			}) {
				return
			}
		}

		send(agentcore.StreamPart{
			Kind:         agentcore.StreamPartStepFinish,
			Usage:        mapUsage(acc.Usage),
			FinishReason: mapStopReason(acc.StopReason),
		})
	}, func() { _ = stream.Close() }), nil
}

// textDelta extracts an incremental text fragment from a stream event, if
// the event is a content_block_delta carrying text.
func textDelta(event anthropic.MessageStreamEventUnion) (string, bool) {
	cbd, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent)
	if !ok {
		return "", false
	}
	if td, ok := cbd.Delta.AsAny().(anthropic.TextDelta); ok {
		return td.Text, true
	}
	return "", false
}

// mapUsage normalizes Anthropic usage into [agentcore.Usage]. Input is the
// total prompt tokens INCLUSIVE of cache reads (uncached input plus
// cache-read); cache creation is reported as CacheWrite.
func mapUsage(u anthropic.Usage) agentcore.Usage {
	return agentcore.Usage{
		Input:      u.InputTokens + u.CacheReadInputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.CacheReadInputTokens,
		CacheWrite: u.CacheCreationInputTokens,
	}
}

// classify turns a raw Anthropic SDK / transport error into an
// [agentcore.ModelError] tagged with the retry class the loop needs. A
// cancellation is returned unwrapped so the loop maps it to FinishCanceled
// rather than retrying it. An HTTP error is bucketed by status code (429/503/
// 529 retryable; 401/403 auth; 413 or a length-flavored 400 context-length;
// other 400 invalid-request); a bare transport failure (connection reset,
// timeout, unexpected EOF) is treated as transient and retryable.
func classify(err error) error {
	if err == nil || isCancellation(err) {
		return err
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		retryAfter := agentcore.RetryAfterFromHeader(headerOf(apiErr.Response))
		class := agentcore.ClassifyStatus(apiErr.StatusCode, looksLikeContextLength(err.Error()))
		if class == agentcore.ErrClassContextLength {
			return agentcore.NewModelError(class, retryAfter,
				fmt.Errorf("anthropic: request exceeds the model context window (HTTP %d) — trim the conversation history: %w", apiErr.StatusCode, err))
		}
		return agentcore.NewModelError(class, retryAfter, err)
	}
	if isTransientNetwork(err) {
		return agentcore.NewModelError(agentcore.ErrClassTransient, 0, err)
	}
	return agentcore.NewModelError(agentcore.ErrClassUpstream, 0, err)
}

// isCancellation reports a context cancellation or deadline expiry, which the
// loop must not retry.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isTransientNetwork reports a transport-level failure worth retrying: a net
// timeout, a connection reset, or an unexpected EOF before any output.
func isTransientNetwork(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "EOF")
}

// looksLikeContextLength recognizes a length/oversize failure a 400 does not
// reveal by status code alone.
func looksLikeContextLength(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "context") && strings.Contains(m, "length") ||
		strings.Contains(m, "prompt is too long") ||
		strings.Contains(m, "maximum") && strings.Contains(m, "token")
}

// headerOf safely reads the header off a possibly-nil response.
func headerOf(res *http.Response) http.Header {
	if res == nil {
		return nil
	}
	return res.Header
}

// mapStopReason maps an Anthropic stop reason to a unified finish reason. A
// tool_use stop becomes tool-calls; a max_tokens stop becomes length;
// everything else (end_turn, stop_sequence, …) becomes stop.
func mapStopReason(reason anthropic.StopReason) agentcore.FinishReason {
	switch reason {
	case anthropic.StopReasonToolUse:
		return agentcore.FinishToolCalls
	case anthropic.StopReasonMaxTokens:
		return agentcore.FinishLength
	default:
		return agentcore.FinishStop
	}
}

// buildParams converts an [agentcore.Request] into Anthropic message params.
func buildParams(modelID string, req agentcore.Request) (anthropic.MessageNewParams, error) {
	maxTokens := int64(req.MaxOutputTokens)
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(modelID),
		MaxTokens: maxTokens,
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	for _, def := range req.Tools {
		tool, err := toolParam(def)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	for _, msg := range req.Messages {
		params.Messages = append(params.Messages, messageParam(msg))
	}
	return params, nil
}

// messageParam converts one [agentcore.Message] into an Anthropic
// MessageParam. Tool-result messages are sent, per the Anthropic wire
// format, as user-role messages carrying tool_result blocks.
func messageParam(msg agentcore.Message) anthropic.MessageParam {
	switch msg.Role {
	case agentcore.RoleAssistant:
		return anthropic.NewAssistantMessage(assistantBlocks(msg.Content)...)
	case agentcore.RoleTool:
		return anthropic.NewUserMessage(toolResultBlocks(msg.Content)...)
	default: // RoleUser
		return anthropic.NewUserMessage(userBlocks(msg.Content)...)
	}
}

func userBlocks(content []agentcore.ContentPart) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(content))
	for _, part := range content {
		if part.Kind == agentcore.ContentText {
			blocks = append(blocks, anthropic.NewTextBlock(part.Text))
		}
	}
	return blocks
}

func assistantBlocks(content []agentcore.ContentPart) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(content))
	for _, part := range content {
		switch part.Kind {
		case agentcore.ContentText:
			blocks = append(blocks, anthropic.NewTextBlock(part.Text))
		case agentcore.ContentToolCall:
			if part.ToolCall != nil {
				blocks = append(blocks, anthropic.NewToolUseBlock(part.ToolCall.ID, part.ToolCall.Input, part.ToolCall.Name))
			}
		}
	}
	return blocks
}

func toolResultBlocks(content []agentcore.ContentPart) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(content))
	for _, part := range content {
		if part.Kind == agentcore.ContentToolResult && part.ToolResult != nil {
			blocks = append(blocks, anthropic.NewToolResultBlock(part.ToolResult.ToolCallID, part.ToolResult.Content, part.ToolResult.IsError))
		}
	}
	return blocks
}

// toolParam converts a tool definition into an Anthropic ToolParam,
// extracting the JSON-Schema properties/required from the raw input schema.
func toolParam(def agentcore.ToolDefinition) (anthropic.ToolParam, error) {
	var schema struct {
		Properties any      `json:"properties"`
		Required   []string `json:"required"`
	}
	if len(def.InputSchema) > 0 {
		if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
			return anthropic.ToolParam{}, err
		}
	}
	tool := anthropic.ToolParam{
		Name:        def.Name,
		InputSchema: anthropic.ToolInputSchemaParam{Properties: schema.Properties, Required: schema.Required},
	}
	if def.Description != "" {
		tool.Description = anthropic.String(def.Description)
	}
	return tool, nil
}
