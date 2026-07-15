package capability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestComposeDefaultConnector_RealMCPRoundTrip exercises the real MCP client
// path (no connector override): connect, list, allow-list filter, namespace,
// and call a tool end-to-end against an in-process go-sdk server.
func TestComposeDefaultConnector_RealMCPRoundTrip(t *testing.T) {
	var calls []string
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-streamco", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "streams_list", Description: "List streams",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "[]"}}}, nil
		})
	server.AddTool(&mcp.Tool{Name: "pipeline_diagnose", Description: "Diagnose",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []any{"id"}}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &args)
			calls = append(calls, args.ID)
			out, _ := json.Marshal(map[string]any{"id": args.ID, "findings": []string{"CONSUMER_LAG"}})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
		})
	server.AddTool(&mcp.Tool{Name: "not_allow_listed", Description: "hidden",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "nope"}}}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	var invoked []string
	doc := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Knowledge = nil
		d.Spec.Tools.MCPServers[0].Endpoint = srv.URL
	})

	composed, err := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		OnToolInvocation: func(inv ProviderToolInvocation) { invoked = append(invoked, inv.NamespacedToolName) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer composed.Close()

	got := keys(composed.Tools)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "streamco__pipeline_diagnose" || got[1] != "streamco__streams_list" {
		t.Fatalf("tools = %v (not_allow_listed must be filtered)", got)
	}

	out, err := composed.Tools["streamco__pipeline_diagnose"].Execute(context.Background(), json.RawMessage(`{"id":"p-1"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var result struct {
		ID       string   `json:"id"`
		Findings []string `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("result not json: %q", out)
	}
	if result.ID != "p-1" || len(result.Findings) != 1 || result.Findings[0] != "CONSUMER_LAG" {
		t.Fatalf("result = %+v", result)
	}
	if len(calls) != 1 || calls[0] != "p-1" {
		t.Fatalf("wire calls = %v (un-namespaced name must reach the server)", calls)
	}
	if len(invoked) != 1 || invoked[0] != "streamco__pipeline_diagnose" {
		t.Fatalf("metering = %v", invoked)
	}
}
