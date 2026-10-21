package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/execapproval"
)

// computeExecRunTool is the only provider tool for which an authenticated
// human turn is required by the runtime. Provider prose and model output can
// never create that turn.
const computeExecRunTool = "compute__compute_exec_run"
const execApprovalPrefix = "Approve exec "

// gateExecTools prevents the model from executing a command until the current
// user message is an exact approval of the opaque plan token. It also mints a
// server-only assertion for the provider, so a model-supplied `approval` field
// has no authority. A durable Store makes each approval single-use even when
// two assistant replicas race.
func gateExecTools(tools agentcore.ToolSet, userText, project, requesterToken string, store execapproval.Store, key []byte) agentcore.ToolSet {
	run, ok := tools[computeExecRunTool]
	if !ok {
		return tools
	}
	copy := make(agentcore.ToolSet, len(tools))
	for name, tool := range tools {
		copy[name] = tool
	}
	if store == nil || len(key) < 32 || project == "" || requesterToken == "" {
		delete(copy, computeExecRunTool)
		return copy
	}
	copy[computeExecRunTool] = &execApprovalTool{inner: run, userText: userText,
		project: project, requesterToken: requesterToken, store: store, key: key}
	return copy
}

type execApprovalTool struct {
	inner          agentcore.Tool
	userText       string
	project        string
	requesterToken string
	store          execapproval.Store
	key            []byte
}

func (t *execApprovalTool) Definition() agentcore.ToolDefinition { return t.inner.Definition() }

func (t *execApprovalTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		PlanToken string `json:"planToken"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", errors.New("invalid exec plan arguments")
	}
	if args.PlanToken == "" || len(args.PlanToken) > 4096 ||
		t.userText != execApprovalPrefix+args.PlanToken {
		return "", errors.New("exec requires a fresh, exact approval from the person who asked")
	}
	used, err := t.store.Consume(ctx, t.project, t.requesterToken, args.PlanToken)
	if err != nil {
		return "", err
	}
	if !used {
		return "", errors.New("this exec approval has already been used")
	}

	// The provider independently verifies this assertion and the plan token.
	// It is made only after the current user turn passes the gate and the
	// durable consume succeeds. A model-provided field is overwritten.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil || fields == nil {
		return "", errors.New("invalid exec plan arguments")
	}
	assertion := execApprovalAssertion(t.key, t.project, t.requesterToken, args.PlanToken)
	encoded, err := json.Marshal(assertion)
	if err != nil {
		return "", err
	}
	fields["approval"] = encoded
	approved, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return t.inner.Execute(ctx, approved)
}

func execApprovalAssertion(key []byte, project, requesterToken, planToken string) string {
	requesterHash := sha256.Sum256([]byte(requesterToken))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("assistant-exec-approval-v1\x00"))
	_, _ = mac.Write([]byte(project))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(requesterHash[:])
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(planToken))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
