package mockmodel

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/milo-os/assistant/agentcore"
)

func collect(t *testing.T, m *Model, req agentcore.Request) (text string, toolCalls []agentcore.ToolCall, usage agentcore.Usage) {
	t.Helper()
	s, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var b strings.Builder
	for {
		p, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch p.Kind {
		case agentcore.StreamPartTextDelta:
			b.WriteString(p.Text)
		case agentcore.StreamPartToolCall:
			toolCalls = append(toolCalls, *p.ToolCall)
		case agentcore.StreamPartStepFinish:
			usage = p.Usage
		}
	}
	return b.String(), toolCalls, usage
}

func TestDiagnoseEmitsToolCall(t *testing.T) {
	_, calls, usage := collect(t, New(), agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("please diagnose pipeline p-42")},
		Tools:    []agentcore.ToolDefinition{{Name: "streamco__pipeline_diagnose"}},
	})
	if len(calls) != 1 || calls[0].Name != "streamco__pipeline_diagnose" {
		t.Fatalf("tool calls = %+v", calls)
	}
	var args struct{ ID string }
	_ = json.Unmarshal(calls[0].Input, &args)
	if args.ID != "p-42" {
		t.Fatalf("extracted id = %q", args.ID)
	}
	if usage != Usage {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestDiagnoseWithoutToolFallsBackToText(t *testing.T) {
	// "diagnose" mentioned but no pipeline_diagnose tool available.
	text, calls, _ := collect(t, New(), agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("diagnose this")},
	})
	if len(calls) != 0 {
		t.Fatalf("expected no tool calls, got %+v", calls)
	}
	if text == "" {
		t.Fatal("expected a generic reply")
	}
}

func TestToolResultIsSummarized(t *testing.T) {
	text, calls, _ := collect(t, New(), agentcore.Request{
		Messages: []agentcore.Message{
			agentcore.UserMessage("diagnose p-1"),
			{Role: agentcore.RoleAssistant, Content: []agentcore.ContentPart{{Kind: agentcore.ContentToolCall, ToolCall: &agentcore.ToolCall{ID: "c1", Name: "streamco__pipeline_diagnose"}}}},
			{Role: agentcore.RoleTool, Content: []agentcore.ContentPart{{Kind: agentcore.ContentToolResult, ToolResult: &agentcore.ToolResult{ToolCallID: "c1", Content: `{"findings":["CONSUMER_LAG"]}`}}}},
		},
		Tools: []agentcore.ToolDefinition{{Name: "streamco__pipeline_diagnose"}},
	})
	if len(calls) != 0 {
		t.Fatalf("a tool result should yield a text summary, not another call: %+v", calls)
	}
	if !strings.Contains(text, "CONSUMER_LAG") || !strings.Contains(text, "pipeline diagnosis") {
		t.Fatalf("summary should quote findings, got: %q", text)
	}
}

func TestGenericReplyWhenNoDiagnose(t *testing.T) {
	text, calls, _ := collect(t, New(), agentcore.Request{
		Messages: []agentcore.Message{agentcore.UserMessage("hello there")},
	})
	if len(calls) != 0 {
		t.Fatalf("unexpected tool calls: %+v", calls)
	}
	if !strings.Contains(text, "Patch") {
		t.Fatalf("generic reply = %q", text)
	}
}
