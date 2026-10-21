package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/execapproval"
)

type captureExecTool struct {
	called int
	input  map[string]json.RawMessage
}

func (t *captureExecTool) Definition() agentcore.ToolDefinition {
	return agentcore.ToolDefinition{Name: computeExecRunTool}
}
func (t *captureExecTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	t.called++
	if err := json.Unmarshal(input, &t.input); err != nil {
		return "", err
	}
	return "ran", nil
}

func TestExecApprovalRequiresCurrentExactUserTurnAndConsumesOnce(t *testing.T) {
	ctx := context.Background()
	store := execapproval.NewMemoryStore()
	key := []byte(strings.Repeat("k", 32))
	plan := "signed-plan"
	inner := &captureExecTool{}
	base := agentcore.ToolSet{computeExecRunTool: inner}
	input := json.RawMessage(`{"planToken":"signed-plan","approval":"model-forged"}`)

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
	if got == execApprovalAssertion(key, "other-project", "caller-token", plan) ||
		got == execApprovalAssertion(key, "project", "refreshed-token", plan) {
		t.Fatal("approval assertion was not bound to project and current bearer token")
	}
	if _, err := approved.Execute(ctx, input); err == nil || inner.called != 1 {
		t.Fatalf("replay accepted: err=%v, provider calls=%d", err, inner.called)
	}
	// Even with the same approval text, changed plan arguments are denied.
	if _, err := approved.Execute(ctx, json.RawMessage(`{"planToken":"other-plan"}`)); err == nil {
		t.Fatal("approved a changed plan")
	}
}

func TestExecToolUnavailableWithoutDurableGate(t *testing.T) {
	base := agentcore.ToolSet{computeExecRunTool: &captureExecTool{}}
	if _, ok := gateExecTools(base, "Approve exec plan", "project", "token", nil,
		[]byte(strings.Repeat("k", 32)))[computeExecRunTool]; ok {
		t.Fatal("exec tool should be unavailable without approval store")
	}
}
