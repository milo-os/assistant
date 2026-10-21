package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/execapproval"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestApprovedExecRunsBeforeModelCanReplan(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "compute", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "compute_exec_run", Description: "Run an approved command",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"planToken": map[string]any{"type": "string"}, "approval": map[string]any{"type": "string"}}}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var input struct {
				PlanToken string `json:"planToken"`
				Approval  string `json:"approval"`
			}
			if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
				return nil, err
			}
			if input.PlanToken != "signed-plan" || input.Approval != execApprovalAssertion(key, "project", "caller-token", "signed-plan") {
				t.Errorf("provider received unapproved input")
			}
			calls.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"instance":"test","container":"app","session":"assistant-exec-test","stdout":"ok","exitCode":0,"reason":"Completed"}`}}}, nil
		})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true}))
	defer srv.Close()
	doc := capability.CapabilityDocument{Spec: capability.CapabilitySpec{
		ServiceRef: capability.Ref{Name: "compute"}, ServiceName: "compute.datumapis.com",
		ServiceAgentRef: capability.Ref{Name: "compute-agent"}, ConfigurationVersion: "v3",
		Tools: &capability.Tools{MCPServers: []capability.MCPServer{{
			Name: "compute", Endpoint: srv.URL,
			ToolSelector: capability.ToolSelector{Include: []string{"compute_exec_run"}},
			Mutating:     []string{"compute_exec_run"},
		}}},
	}}
	store := execapproval.NewMemoryStore()
	newModel := func() *scriptModel {
		// Without the dispatch, this model would answer without running the
		// tool, as happened in staging when it incorrectly asked for a replan.
		return &scriptModel{turns: []scriptTurn{{parts: []agentcore.StreamPart{
			{Kind: agentcore.StreamPartTextDelta, Text: "Result received."},
			{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop},
		}}}}
	}
	run := func(text string) ([]Event, *scriptModel, string) {
		model := newModel()
		conv := New(Deps{Model: model, Source: fakeSource{docs: []capability.CapabilityDocument{doc}},
			ExecApprovalKey: key, ExecApprovals: store, Emitter: noopEmitter(),
			AllowPrivateCapabilityNetworks: true})
		ctx := auth.ContextWithBearerToken(context.Background(), "caller-token")
		stream := conv.Run(ctx, Params{UserText: text, ApprovalText: text, ProjectName: "project", ContextID: "context"})
		events := drainEvents(t, stream)
		if stream.Result().State != StateCompleted {
			t.Fatalf("turn state = %s: %s", stream.Result().State, stream.Result().Error)
		}
		return events, model, stream.Result().Text
	}

	run("run it")
	if calls.Load() != 0 {
		t.Fatal("vague request executed a command")
	}
	events, model, answer := run("Approve exec signed-plan")
	if calls.Load() != 1 || model.call != 0 {
		t.Fatalf("approved plan: provider calls=%d model calls=%d, want provider once without model inference", calls.Load(), model.call)
	}
	var sawCall, sawResult bool
	for _, event := range events {
		sawCall = sawCall || (event.Kind == EventToolCall && event.ToolName == computeExecRunTool)
		sawResult = sawResult || (event.Kind == EventToolResult && event.ToolName == computeExecRunTool && !event.ToolFailed)
	}
	if !sawCall || !sawResult {
		t.Fatal("approved execution did not emit tool activity")
	}
	if !strings.Contains(answer, "Exit code: 0") || !strings.Contains(answer, "stdout:\nok") {
		t.Fatalf("server-rendered exec result missing status or output: %q", answer)
	}
	run("Approve exec signed-plan")
	if calls.Load() != 1 {
		t.Fatal("replayed approval reached provider")
	}
}

func TestApprovedExecTokenRequiresExactCurrentText(t *testing.T) {
	for _, tc := range []struct {
		approval, user string
	}{
		{"Approve exec plan", "Approve exec plan\nrun it"},
		{"Approve exec plan extra", "Approve exec plan extra"},
		{"Approve exec ", "Approve exec "},
		{"Earlier: Approve exec plan", "Earlier: Approve exec plan"},
	} {
		if _, ok := approvedExecToken(tc.approval, tc.user); ok {
			t.Errorf("accepted nonexact approval %q", tc.approval)
		}
	}
	if token, ok := approvedExecToken("Approve exec signed-plan", "Approve exec signed-plan"); !ok || token != "signed-plan" {
		t.Fatalf("valid approval token=%q ok=%v", token, ok)
	}
}

func TestApprovedExecResultReportsUncertainFailure(t *testing.T) {
	messages := []agentcore.Message{{Role: agentcore.RoleTool, Content: []agentcore.ContentPart{{
		Kind: agentcore.ContentToolResult,
		ToolResult: &agentcore.ToolResult{
			Name: computeExecRunTool, IsError: true, Content: "provider connection lost",
		},
	}}}}
	answer := approvedExecResult(messages)
	if !strings.Contains(answer, "outcome is uncertain") || !strings.Contains(answer, "approval was consumed") {
		t.Fatalf("unsafe retry guidance after uncertain result: %q", answer)
	}
}

func TestApprovedExecResultDoesNotInventCommandExitBeforeSession(t *testing.T) {
	messages := []agentcore.Message{{Role: agentcore.RoleTool, Content: []agentcore.ContentPart{{
		Kind: agentcore.ContentToolResult,
		ToolResult: &agentcore.ToolResult{
			Name:    computeExecRunTool,
			Content: `{"instance":"test","container":"app","exitCode":1,"reason":"creating session: admission denied"}`,
		},
	}}}}
	answer := approvedExecResult(messages)
	if !strings.Contains(answer, "command did not run") || !strings.Contains(answer, "approval was consumed") ||
		strings.Contains(answer, "Exit code:") {
		t.Fatalf("pre-session failure reported as a command exit: %q", answer)
	}
}
