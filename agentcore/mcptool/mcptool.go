// Package mcptool adapts a Model Context Protocol (MCP) server into
// [agentcore.Tool] values, using the official modelcontextprotocol/go-sdk
// client over the Streamable HTTP transport.
//
// It is a thin, policy-free bridge: [Connect] opens a session, [Session.Tools]
// lists the server's tools as executable [agentcore.Tool]s keyed by their
// un-namespaced names, and each tool's Execute performs a CallTool round-trip
// and returns the joined text content. Higher-level policy — allow-list
// filtering, namespacing, collision handling, connect timeouts, and metering —
// belongs to the composition layer that builds on this package.
package mcptool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/milo-os/assistant/agentcore"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultClientName is the MCP client name announced during initialization.
const DefaultClientName = "datum-assistant-service"

// Options configures a [Connect] call. Endpoint is required.
type Options struct {
	// Endpoint is the Streamable HTTP endpoint of the MCP server.
	Endpoint string
	// HTTPClient overrides the HTTP client used for the transport. Nil uses a
	// client whose transport adds the Accept header some stateless servers
	// require (see [acceptRoundTripper]).
	HTTPClient *http.Client
	// ClientName overrides the announced client name. Empty uses
	// [DefaultClientName].
	ClientName string
	// ClientVersion is the announced client version. Empty uses "0".
	ClientVersion string
	// EnableStandaloneSSE opts into the server-initiated SSE stream. It is off
	// by default because the per-request, request/response usage here needs no
	// server-initiated notifications, and some stateless servers reject the
	// standalone GET.
	EnableStandaloneSSE bool
}

// Session is a connected MCP client session. It is not safe for concurrent
// use. Close it when done.
type Session struct {
	cs *mcp.ClientSession
}

// Connect opens a session to the MCP server described by opts.
func Connect(ctx context.Context, opts Options) (*Session, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("mcptool: Endpoint is required")
	}
	name := opts.ClientName
	if name == "" {
		name = DefaultClientName
	}
	version := opts.ClientVersion
	if version == "" {
		version = "0"
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: &acceptRoundTripper{next: http.DefaultTransport}}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: version}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             opts.Endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: !opts.EnableStandaloneSSE,
	}
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcptool: connect %s: %w", opts.Endpoint, err)
	}
	return &Session{cs: cs}, nil
}

// Tools lists the server's tools as executable [agentcore.Tool]s keyed by
// their un-namespaced names. The listing auto-paginates.
func (s *Session) Tools(ctx context.Context) (map[string]agentcore.Tool, error) {
	tools := map[string]agentcore.Tool{}
	for tool, err := range s.cs.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcptool: list tools: %w", err)
		}
		tools[tool.Name] = &toolAdapter{
			cs:  s.cs,
			def: definition(tool),
		}
	}
	return tools, nil
}

// Close ends the session.
func (s *Session) Close() error { return s.cs.Close() }

// toolAdapter is an [agentcore.Tool] backed by one MCP tool on a session.
type toolAdapter struct {
	cs  *mcp.ClientSession
	def agentcore.ToolDefinition
}

func (t *toolAdapter) Definition() agentcore.ToolDefinition { return t.def }

// Execute calls the MCP tool and returns its joined text content. A tool that
// reports an error (CallToolResult.IsError) is returned as a Go error so the
// loop feeds it back to the model as an error result; the error text carries
// the tool's own message.
func (t *toolAdapter) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args any
	if len(input) > 0 {
		args = json.RawMessage(input)
	}
	res, err := t.cs.CallTool(ctx, &mcp.CallToolParams{Name: t.def.Name, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("mcptool: call %s: %w", t.def.Name, err)
	}
	text := joinText(res.Content)
	if res.IsError {
		return text, errors.New(text)
	}
	return text, nil
}

// definition converts an MCP tool into an [agentcore.ToolDefinition],
// preserving the raw input schema as JSON.
func definition(tool *mcp.Tool) agentcore.ToolDefinition {
	var schema json.RawMessage
	if tool.InputSchema != nil {
		if raw, err := json.Marshal(tool.InputSchema); err == nil {
			schema = raw
		}
	}
	return agentcore.ToolDefinition{
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: schema,
	}
}

// joinText concatenates the text of every text content block in a result,
// falling back to a JSON dump of non-text content so nothing is silently lost.
func joinText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		switch v := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, v.Text)
		default:
			if raw, err := json.Marshal(c); err == nil {
				parts = append(parts, string(raw))
			}
		}
	}
	return strings.Join(parts, "")
}

// acceptRoundTripper adds the Accept header some stateless MCP servers
// require on POSTs (both JSON and SSE media types) before delegating.
type acceptRoundTripper struct {
	next http.RoundTripper
}

func (rt *acceptRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	return rt.next.RoundTrip(req)
}
