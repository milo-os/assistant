package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/auth"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/execapproval"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type captureExecTool struct {
	mu     sync.Mutex
	called int
	input  map[string]json.RawMessage
	err    error
}

func (t *captureExecTool) Definition() agentcore.ToolDefinition {
	return agentcore.ToolDefinition{Name: computeExecRunTool}
}
func (t *captureExecTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.called++
	if err := json.Unmarshal(input, &t.input); err != nil {
		return "", err
	}
	return "ran", t.err
}

func TestExecApprovalRequiresCurrentExactUserTurnAndConsumesOnce(t *testing.T) {
	ctx := context.Background()
	store := execapproval.NewMemoryStore()
	key := []byte(strings.Repeat("k", 32))
	plan := "signed-plan"
	inner := &captureExecTool{}
	base := agentcore.ToolSet{computeExecRunTool: inner}
	input := json.RawMessage(`{"planToken":"signed-plan","approval":"model-forged","command":["rm","-rf","/"],"instance":"other"}`)

	for _, text := range []string{"run it", "Approve exec other-plan", "Earlier message: Approve exec signed-plan", "Approve exec signed-plan\nplease"} {
		tool := gateExecTools(base, text, "project", "caller-token", store, key)[computeExecRunTool]
		if _, err := tool.Execute(ctx, input); err == nil {
			t.Errorf("accepted unapproved user text %q", text)
		}
	}
	if inner.called != 0 {
		t.Fatalf("provider called %d times without user approval", inner.called)
	}
	approved := gateExecTools(base, execApprovalPrefix+plan, "project", "caller-token", store, key)[computeExecRunTool]
	if _, err := approved.Execute(ctx, input); err != nil {
		t.Fatalf("approved run: %v", err)
	}
	var got string
	if err := json.Unmarshal(inner.input["approval"], &got); err != nil {
		t.Fatal(err)
	}
	if got != execApprovalAssertion(key, "project", "caller-token", plan) || got == "model-forged" {
		t.Fatal("provider did not receive the server-created approval assertion")
	}
	if len(inner.input) != 2 || inner.input["command"] != nil || inner.input["instance"] != nil {
		t.Fatalf("unapproved model fields forwarded: %v", inner.input)
	}
	if got == execApprovalAssertion(key, "other-project", "caller-token", plan) ||
		got == execApprovalAssertion(key, "project", "refreshed-token", plan) {
		t.Fatal("approval assertion was not bound to project and current bearer token")
	}
	if _, err := approved.Execute(ctx, input); err == nil || inner.called != 1 {
		t.Fatalf("replay accepted: err=%v, provider calls=%d", err, inner.called)
	}
	refreshed := gateExecTools(base, execApprovalPrefix+plan, "project", "refreshed-token", store, key)[computeExecRunTool]
	if _, err := refreshed.Execute(ctx, input); err == nil || inner.called != 1 {
		t.Fatalf("refreshed bearer replay accepted: err=%v, provider calls=%d", err, inner.called)
	}
	// Even with the same approval text, changed plan arguments are denied.
	if _, err := approved.Execute(ctx, json.RawMessage(`{"planToken":"other-plan"}`)); err == nil {
		t.Fatal("approved a changed plan")
	}
}

type failingApprovalStore struct{}

func (failingApprovalStore) Consume(context.Context, string, string) (bool, error) {
	return false, errors.New("ledger unavailable")
}

func TestExecApprovalFailsClosedOnLedgerError(t *testing.T) {
	inner := &captureExecTool{}
	tool := gateExecTools(agentcore.ToolSet{computeExecRunTool: inner}, "Approve exec plan",
		"project", "token", failingApprovalStore{}, []byte(strings.Repeat("k", 32)))[computeExecRunTool]
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"planToken":"plan"}`)); err == nil || inner.called != 0 {
		t.Fatalf("ledger error allowed provider call: err=%v, calls=%d", err, inner.called)
	}
}

func TestExecApprovalProviderFailureBurnsPlan(t *testing.T) {
	inner := &captureExecTool{err: errors.New("provider unavailable")}
	store := execapproval.NewMemoryStore()
	tool := gateExecTools(agentcore.ToolSet{computeExecRunTool: inner}, "Approve exec plan",
		"project", "token", store, []byte(strings.Repeat("k", 32)))[computeExecRunTool]
	input := json.RawMessage(`{"planToken":"plan"}`)
	if _, err := tool.Execute(context.Background(), input); err == nil || !strings.Contains(err.Error(), "create a new plan") {
		t.Fatalf("provider failure did not explain consumed approval: %v", err)
	}
	if _, err := tool.Execute(context.Background(), input); err == nil || !strings.Contains(err.Error(), "already been used") || inner.called != 1 {
		t.Fatalf("failed plan was retried: err=%v, calls=%d", err, inner.called)
	}
}

func TestExecApprovalConcurrentConsume(t *testing.T) {
	inner := &captureExecTool{}
	store := execapproval.NewMemoryStore()
	key := []byte(strings.Repeat("k", 32))
	var wg sync.WaitGroup
	for _, bearer := range []string{"original", "refreshed"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tool := gateExecTools(agentcore.ToolSet{computeExecRunTool: inner}, "Approve exec plan",
				"project", bearer, store, key)[computeExecRunTool]
			_, _ = tool.Execute(context.Background(), json.RawMessage(`{"planToken":"plan"}`))
		}()
	}
	wg.Wait()
	if inner.called != 1 {
		t.Fatalf("concurrent approval ran provider %d times", inner.called)
	}
}

func TestExecToolUnavailableWithoutDurableGate(t *testing.T) {
	base := agentcore.ToolSet{computeExecRunTool: &captureExecTool{}}
	if _, ok := gateExecTools(base, "Approve exec plan", "project", "token", nil,
		[]byte(strings.Repeat("k", 32)))[computeExecRunTool]; ok {
		t.Fatal("exec tool should be unavailable without approval store")
	}
}

type toolListModel struct{ tools []agentcore.ToolDefinition }

func (m *toolListModel) ModelID() string { return "tool-list" }
func (m *toolListModel) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.tools = req.Tools
	return &partReader{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop},
	}}, nil
}

func TestConversationHidesExecWithoutApprovalStore(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "compute", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "compute_exec_run", Description: "Run a command",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"planToken": map[string]any{"type": "string"}}}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
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
	model := &toolListModel{}
	conv := New(Deps{Model: model, Source: fakeSource{docs: []capability.CapabilityDocument{doc}},
		AllowPrivateCapabilityNetworks: true, ExecApprovalKey: []byte(strings.Repeat("k", 32)),
		Emitter: noopEmitter()})
	stream := conv.Run(auth.ContextWithBearerToken(context.Background(), "caller-token"), Params{UserText: "hi", ApprovalText: "Approve exec plan",
		ProjectName: "project", ContextID: "context"})
	drainEvents(t, stream)
	for _, def := range model.tools {
		if def.Name == computeExecRunTool {
			t.Fatal("exec tool reached model without durable approval store")
		}
	}
}
