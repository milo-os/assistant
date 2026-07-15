package mcptool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestServer builds an in-process MCP server exposing two tools over a
// stateless Streamable HTTP handler, and returns its URL.
func newTestServer(t *testing.T, calls *[]string) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-streamco", Version: "1.0.0"}, nil)

	server.AddTool(
		&mcp.Tool{
			Name:        "streams_list",
			Description: "List streams",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `["s-1"]`}}}, nil
		},
	)
	server.AddTool(
		&mcp.Tool{
			Name:        "pipeline_diagnose",
			Description: "Diagnose a pipeline",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string"}},
				"required":   []any{"id"},
			},
		},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &args)
			*calls = append(*calls, args.ID)
			out, _ := json.Marshal(map[string]any{"id": args.ID, "findings": []string{"CONSUMER_LAG"}})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
		},
	)

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSessionListsAndCallsTools(t *testing.T) {
	var calls []string
	url := newTestServer(t, &calls)

	sess, err := Connect(context.Background(), Options{Endpoint: url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	tools, err := sess.Tools(context.Background())
	if err != nil {
		t.Fatalf("tools: %v", err)
	}

	var names []string
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "pipeline_diagnose" || names[1] != "streams_list" {
		t.Fatalf("tool names = %v", names)
	}

	diagnose := tools["pipeline_diagnose"]
	def := diagnose.Definition()
	if def.Name != "pipeline_diagnose" || def.Description != "Diagnose a pipeline" {
		t.Fatalf("definition = %+v", def)
	}
	if len(def.InputSchema) == 0 {
		t.Fatal("input schema not preserved")
	}

	out, err := diagnose.Execute(context.Background(), json.RawMessage(`{"id":"p-1"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var result struct {
		ID       string   `json:"id"`
		Findings []string `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("result not json: %q (%v)", out, err)
	}
	if result.ID != "p-1" || len(result.Findings) != 1 || result.Findings[0] != "CONSUMER_LAG" {
		t.Fatalf("result = %+v", result)
	}
	// The un-namespaced tool name and args reached the wire.
	if len(calls) != 1 || calls[0] != "p-1" {
		t.Fatalf("server calls = %v", calls)
	}
}
