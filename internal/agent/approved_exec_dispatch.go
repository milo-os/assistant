package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/milo-os/assistant/agentcore"
)

// approvedExecToken recognizes only the complete current user approval.
// The provider still verifies the signed plan, requester, expiry and nonce;
// this just makes the already-approved tool call deterministic across turns.
func approvedExecToken(approvalText, userText string) (string, bool) {
	if approvalText != userText {
		return "", false
	}
	token, ok := strings.CutPrefix(approvalText, execApprovalPrefix)
	if !ok || token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

// approvedExecModel emits the approved run call and renders its tool result
// without another model decision. Conversation history stores prose but not
// tool results, so a later model turn cannot reliably distinguish a real
// prior plan from text it wrote itself. The authenticated approval and
// provider checks do not depend on that model judgment.
type approvedExecModel struct {
	inner     agentcore.Model
	planToken string
	mu        sync.Mutex
	sent      bool
}

func (m *approvedExecModel) ModelID() string { return m.inner.ModelID() }

func (m *approvedExecModel) Stream(ctx context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.mu.Lock()
	first := !m.sent
	m.sent = true
	m.mu.Unlock()
	if !first {
		answer := approvedExecResult(req.Messages)
		return agentcore.StreamFunc(func(send agentcore.SendFunc) {
			if !send(agentcore.StreamPart{Kind: agentcore.StreamPartTextDelta, Text: answer}) {
				return
			}
			send(agentcore.StreamPart{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop})
		}, nil), nil
	}
	input, err := json.Marshal(struct {
		PlanToken string `json:"planToken"`
	}{PlanToken: m.planToken})
	if err != nil {
		return nil, err
	}
	return agentcore.StreamFunc(func(send agentcore.SendFunc) {
		if !send(agentcore.StreamPart{Kind: agentcore.StreamPartToolCall,
			ToolCall: &agentcore.ToolCall{ID: "approved-exec", Name: computeExecRunTool, Input: input}}) {
			return
		}
		send(agentcore.StreamPart{Kind: agentcore.StreamPartStepFinish,
			FinishReason: agentcore.FinishToolCalls})
	}, nil), nil
}

func approvedExecResult(messages []agentcore.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		for _, part := range messages[i].Content {
			result := part.ToolResult
			if part.Kind != agentcore.ContentToolResult || result == nil || result.Name != computeExecRunTool {
				continue
			}
			if result.IsError {
				return "The approved exec request failed or its outcome is uncertain: " + result.Content +
					"\nThe approval was consumed. Create a new plan before retrying."
			}
			var out struct {
				Instance        string `json:"instance"`
				Container       string `json:"container"`
				Session         string `json:"session"`
				Stdout          string `json:"stdout"`
				Stderr          string `json:"stderr"`
				OutputTruncated bool   `json:"outputTruncated"`
				ExitCode        int    `json:"exitCode"`
				Reason          string `json:"reason"`
				TimedOut        bool   `json:"timedOut"`
			}
			if err := json.Unmarshal([]byte(result.Content), &out); err != nil || out.Instance == "" || out.Container == "" {
				return "Approved exec request result:\n" + result.Content
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Approved exec request result for %s (container %s).\n", out.Instance, out.Container)
			if out.Session != "" {
				fmt.Fprintf(&b, "Session: %s\n", out.Session)
			}
			fmt.Fprintf(&b, "Exit code: %d\n", out.ExitCode)
			if out.Reason != "" {
				fmt.Fprintf(&b, "Reason: %s\n", out.Reason)
			}
			if out.TimedOut {
				b.WriteString("The command timed out.\n")
			}
			if out.OutputTruncated {
				b.WriteString("Output was truncated.\n")
			}
			if out.Stdout != "" {
				b.WriteString("stdout:\n")
				b.WriteString(out.Stdout)
				if !strings.HasSuffix(out.Stdout, "\n") {
					b.WriteByte('\n')
				}
			}
			if out.Stderr != "" {
				b.WriteString("stderr:\n")
				b.WriteString(out.Stderr)
				if !strings.HasSuffix(out.Stderr, "\n") {
					b.WriteByte('\n')
				}
			}
			return b.String()
		}
	}
	return "The approved exec request returned no tool result. Its outcome is uncertain; create a new plan before retrying."
}
