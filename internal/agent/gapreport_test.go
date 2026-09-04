package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/gapreport"
)

// gapReportCallingModel calls the named tool once (a canned gap report),
// then answers with plain text once it sees the tool's result — enough of a
// script to prove the composed tool actually executes and writes.
type gapReportCallingModel struct{ toolName string }

func (m *gapReportCallingModel) ModelID() string { return "gap-report-test-model" }

func (m *gapReportCallingModel) Stream(_ context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	if _, ok := latestToolResult(req.Messages); ok {
		return &partReader{parts: []agentcore.StreamPart{
			{Kind: agentcore.StreamPartTextDelta, Text: "reported it"},
			{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishStop, Usage: agentcore.Usage{Input: 10, Output: 5}},
		}}, nil
	}
	input, _ := json.Marshal(map[string]string{"capability": "list pipelines", "summary": "user needed a pipeline id"})
	return &partReader{parts: []agentcore.StreamPart{
		{Kind: agentcore.StreamPartToolCall, ToolCall: &agentcore.ToolCall{ID: "call-0", Name: m.toolName, Input: input}},
		{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishToolCalls, Usage: agentcore.Usage{Input: 10, Output: 5}},
	}}, nil
}

func latestToolResult(messages []agentcore.Message) (string, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		for _, part := range messages[i].Content {
			if part.Kind == agentcore.ContentToolResult && part.ToolResult != nil {
				return part.ToolResult.Content, true
			}
		}
	}
	return "", false
}

func reportingDoc(reportingProject string) capability.CapabilityDocument {
	return capability.CapabilityDocument{
		Spec: capability.CapabilitySpec{
			ServiceRef:           capability.Ref{Name: "streamco"},
			ServiceName:          "streaming.streamco.example",
			ServiceAgentRef:      capability.Ref{Name: "streamco-agent"},
			ConfigurationVersion: "v1",
			ReportingProject:     reportingProject,
		},
	}
}

// TestGapReportNilDepsComposesNoTool pins the default: no Deps.GapReports,
// no report_capability_gap tool composed — entirely opt-in.
func TestGapReportNilDepsComposesNoTool(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{
		Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		Source: fakeSource{docs: []capability.CapabilityDocument{reportingDoc("streamco-platform")}},
	})
	runTurn(t, conv, Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-1"})

	req := model.requests[0]
	if hasTool(req.Tools, capability.GapReportToolName("streamco")) {
		t.Fatal("gap-report tool composed with Deps.GapReports nil")
	}
}

// TestGapReportDepsSetComposesToolAndWritesToProviderProject proves the
// wiring end to end: the composed tool writes to the PROVIDER project named
// in the document (spec.reportingProject), never to Params.ProjectName (the
// consumer project the conversation ran in).
func TestGapReportDepsSetComposesToolAndWritesToProviderProject(t *testing.T) {
	store := gapreport.NewMemoryStore()
	toolName := capability.GapReportToolName("streamco")
	conv := New(Deps{
		Model: &gapReportCallingModel{toolName: toolName}, ModelMode: "mock", Emitter: noopEmitter(),
		Source:     fakeSource{docs: []capability.CapabilityDocument{reportingDoc("streamco-platform")}},
		GapReports: store,
	})
	result := runTurn(t, conv, Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-1"})
	if result.State != StateCompleted {
		t.Fatalf("state = %s (err=%s)", result.State, result.Error)
	}

	providerReports, err := store.List(context.Background(), "streamco-platform")
	if err != nil || len(providerReports) != 1 {
		t.Fatalf("List(providerProject) = %v, %v; want 1 report", providerReports, err)
	}
	if providerReports[0].ConsumerProject != "demo-project" || providerReports[0].ContextID != "conv-1" {
		t.Fatalf("unexpected provenance: %+v", providerReports[0])
	}

	consumerReports, err := store.List(context.Background(), "demo-project")
	if err != nil || len(consumerReports) != 0 {
		t.Fatalf("List(consumerProject) = %v, %v; want empty — must not land under the consumer project", consumerReports, err)
	}
}

// TestGapReportNoReportingProjectMeansNoTool: a document that declares
// tools but no reportingProject simply gets no gap-report tool, not an error.
func TestGapReportNoReportingProjectMeansNoTool(t *testing.T) {
	store := gapreport.NewMemoryStore()
	model := &recordingModel{}
	conv := New(Deps{
		Model: model, ModelMode: "mock", Emitter: noopEmitter(),
		Source:     fakeSource{docs: []capability.CapabilityDocument{reportingDoc("")}},
		GapReports: store,
	})
	runTurn(t, conv, Params{UserText: "hi", ProjectName: "demo-project", ContextID: "conv-1"})

	req := model.requests[0]
	if hasTool(req.Tools, capability.GapReportToolName("streamco")) {
		t.Fatal("gap-report tool composed for a document with no reportingProject")
	}
}
