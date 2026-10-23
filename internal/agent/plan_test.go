package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/basetools"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/history"
)

func planResultJSON(t *testing.T, out basetools.PlanOutput) string {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var validPlan = basetools.PlanOutput{
	Valid:     true,
	PlanToken: "tok-1",
	ExpiresAt: "2026-10-23T12:15:00Z",
	Results: []basetools.ManifestResult{
		{Kind: "Workload", Name: "orders-api", Action: "create", Valid: true, Manifest: "kind: Workload\n", Summary: "Runs your container"},
		{Kind: "HTTPProxy", Name: "orders-api", Action: "update", Valid: true, Manifest: "kind: HTTPProxy\n", Diff: []string{"+ hostnames: api.acme.com"}},
	},
}

func TestPlanFromResult(t *testing.T) {
	p := planFromResult(planResultJSON(t, validPlan))
	if p == nil {
		t.Fatal("a valid plan with a token must become a card")
	}
	if p.ID != planID("tok-1") || p.Token != "tok-1" || len(p.Resources) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	if r := p.Resources[0]; r.Summary != "Runs your container" || r.Manifest != "kind: Workload\n" {
		t.Fatalf("first row = %+v", r)
	}
	if r := p.Resources[1]; r.Action != "update" || len(r.Diff) != 1 {
		t.Fatalf("second row = %+v", r)
	}

	rejected := basetools.PlanOutput{Valid: false, Results: []basetools.ManifestResult{{Kind: "Workload"}}}
	if planFromResult(planResultJSON(t, rejected)) != nil {
		t.Fatal("a rejected plan has nothing to approve")
	}
	if planFromResult("not json") != nil {
		t.Fatal("an unreadable result is not a plan")
	}
}

func TestPlanIDIsStableAndDistinct(t *testing.T) {
	if planID("a") != planID("a") || planID("a") == planID("b") {
		t.Fatal("a plan's id must follow its token")
	}
	if !strings.HasPrefix(planID("a"), "plan-") {
		t.Fatalf("id = %q", planID("a"))
	}
}

func TestEndsTurnOn(t *testing.T) {
	home := &Surface{Name: surfaceHomeFirstRun}
	if endsTurnOn(nil) != nil || endsTurnOn(&Surface{Name: "settings"}) != nil {
		t.Fatal("nothing ends a turn early off the home page")
	}
	ends := endsTurnOn(home)
	tests := []struct {
		name   string
		result agentcore.ToolResult
		want   bool
	}{
		{"question", agentcore.ToolResult{Name: AskChoiceToolName, Content: "shown"}, true},
		{"failed question", agentcore.ToolResult{Name: AskChoiceToolName, IsError: true}, false},
		{"approvable plan", agentcore.ToolResult{Name: basetools.ResourcesPlanToolName, Content: planResultJSON(t, validPlan)}, true},
		{"rejected plan", agentcore.ToolResult{Name: basetools.ResourcesPlanToolName, Content: `{"valid":false,"results":[]}`}, false},
		{"other tool", agentcore.ToolResult{Name: "resources_list", Content: "{}"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ends(tt.result); got != tt.want {
				t.Fatalf("ends = %v, want %v", got, tt.want)
			}
		})
	}
}

// streamOver runs a Stream over fixed loop parts, so a test can feed it tool
// results the real tools would need a platform to produce.
func streamOver(conv *Conversation, params Params, parts ...agentcore.StreamPart) *Stream {
	return &Stream{
		ctx: context.Background(), conv: conv, params: params, turnStart: time.Now(),
		inner:       &partReader{parts: parts},
		composed:    &capability.Composed{},
		invocations: &[]capability.ProviderToolInvocation{},
		invMu:       &sync.Mutex{},
		state:       StateCompleted,
	}
}

func TestStreamShowsAPlanCardAfterItsToolResult(t *testing.T) {
	store := history.NewMemoryStore()
	conv := New(Deps{Model: &recordingModel{}, ModelMode: "mock", Emitter: noopEmitter(), History: store})
	params := Params{UserText: "deploy it", ProjectName: "demo", ContextID: "ctx-1",
		Surface: &Surface{Name: surfaceHomeFirstRun}}
	s := streamOver(conv, params,
		agentcore.StreamPart{Kind: agentcore.StreamPartTextDelta, Text: "Here's the plan."},
		agentcore.StreamPart{Kind: agentcore.StreamPartToolCall, ToolCall: &agentcore.ToolCall{ID: "c1", Name: basetools.ResourcesPlanToolName}},
		agentcore.StreamPart{Kind: agentcore.StreamPartToolResult, ToolResult: &agentcore.ToolResult{
			ToolCallID: "c1", Name: basetools.ResourcesPlanToolName, Content: planResultJSON(t, validPlan)}},
		agentcore.StreamPart{Kind: agentcore.StreamPartFinish, FinishReason: agentcore.FinishStop},
	)
	events := drainEvents(t, s)

	var kinds []EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []EventKind{EventText, EventToolCall, EventToolResult, EventPlan}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}
	if p := events[3].Plan; p == nil || p.Token != "tok-1" {
		t.Fatalf("plan event = %+v", events[3])
	}

	turns, _ := store.Turns(context.Background(), "demo", "ctx-1")
	if len(turns) != 1 || !strings.Contains(turns[0].AssistantText,
		"Plan shown for approval: create Workload orders-api, update HTTPProxy orders-api.") {
		t.Fatalf("stored answer must carry the plan for the next turn: %+v", turns)
	}
}

func TestStreamShowsNoPlanCardOffTheHomePage(t *testing.T) {
	conv := New(Deps{Model: &recordingModel{}, ModelMode: "mock", Emitter: noopEmitter()})
	s := streamOver(conv, Params{UserText: "deploy it", ProjectName: "demo"},
		agentcore.StreamPart{Kind: agentcore.StreamPartToolResult, ToolResult: &agentcore.ToolResult{
			ToolCallID: "c1", Name: basetools.ResourcesPlanToolName, Content: planResultJSON(t, validPlan)}},
		agentcore.StreamPart{Kind: agentcore.StreamPartFinish, FinishReason: agentcore.FinishStop},
	)
	for _, e := range drainEvents(t, s) {
		if e.Kind == EventPlan {
			t.Fatal("a client that draws no cards must not be sent one")
		}
	}
}

func TestStreamReportsHowAnApprovedPlanWent(t *testing.T) {
	conv := New(Deps{Model: &recordingModel{}, ModelMode: "mock", Emitter: noopEmitter()})
	params := Params{UserText: "Approve the plan", ProjectName: "demo",
		Surface: &Surface{Name: surfaceHomeFirstRun}}
	input, _ := json.Marshal(map[string]any{"manifests": []string{"kind: Workload\n"}, "planToken": "tok-1"})
	applied, _ := json.Marshal(basetools.ApplyOutput{Applied: []basetools.AppliedResource{
		{Action: "create", Kind: "Workload", Name: "orders-api"}}})
	s := streamOver(conv, params,
		agentcore.StreamPart{Kind: agentcore.StreamPartToolCall, ToolCall: &agentcore.ToolCall{
			ID: "c2", Name: basetools.ResourcesApplyToolName, Input: input}},
		agentcore.StreamPart{Kind: agentcore.StreamPartToolResult, ToolResult: &agentcore.ToolResult{
			ToolCallID: "c2", Name: basetools.ResourcesApplyToolName, Content: string(applied)}},
		agentcore.StreamPart{Kind: agentcore.StreamPartFinish, FinishReason: agentcore.FinishStop},
	)
	var result *PlanResult
	for _, e := range drainEvents(t, s) {
		if e.Kind == EventPlanResult {
			result = e.PlanResult
		}
	}
	if result == nil || !result.OK || result.PlanID != planID("tok-1") || len(result.Applied) != 1 {
		t.Fatalf("plan result = %+v", result)
	}
}

// An approval turn starts with resources_apply for exactly the approved
// manifests and token, without asking the model, then hands back to it. Here
// the change path is not composed, so the apply fails as an unknown tool: the
// card still hears that it failed, and the model still gets to say so.
func TestApprovalAppliesThePlanWithoutAModelDecision(t *testing.T) {
	model := &recordingModel{}
	conv := New(Deps{Model: model, ModelMode: "mock", Emitter: noopEmitter()})
	stream := conv.Run(context.Background(), Params{
		UserText: "Approve the plan", ProjectName: "demo", TaskID: "t1",
		Surface:     &Surface{Name: surfaceHomeFirstRun},
		ApprovePlan: &PlanApproval{Token: "tok-1", Manifests: []string{"kind: Workload\n"}},
	})
	events := drainEvents(t, stream)

	var call *Event
	var result *PlanResult
	for i, e := range events {
		switch e.Kind {
		case EventToolCall:
			call = &events[i]
		case EventPlanResult:
			result = e.PlanResult
		}
	}
	if call == nil || call.ToolName != basetools.ResourcesApplyToolName {
		t.Fatalf("the turn must open with resources_apply, events = %+v", events)
	}
	var args struct {
		Manifests []string `json:"manifests"`
		PlanToken string   `json:"planToken"`
	}
	if err := json.Unmarshal(call.ToolInput, &args); err != nil || args.PlanToken != "tok-1" ||
		len(args.Manifests) != 1 || args.Manifests[0] != "kind: Workload\n" {
		t.Fatalf("apply input = %s (%v)", call.ToolInput, err)
	}
	if result == nil || result.OK || result.PlanID != planID("tok-1") || result.Error == "" {
		t.Fatalf("plan result = %+v, want a failure for this plan", result)
	}
	if len(model.requests) != 1 {
		t.Fatalf("the model should be asked once, after the apply, got %d", len(model.requests))
	}
}
