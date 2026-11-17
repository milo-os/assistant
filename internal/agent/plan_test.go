package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/basetools"
	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/execapproval"
	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/internal/plantoken"
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

// fakeApply stands in for resources_apply: it takes the plan's approval code
// from its input (the real tool derives it from the manifests) and writes
// only when the turn's approval agrees.
type fakeApply struct{ writes int }

func (f *fakeApply) Definition() agentcore.ToolDefinition {
	return agentcore.ToolDefinition{Name: basetools.ResourcesApplyToolName}
}

func (f *fakeApply) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		PlanToken string `json:"planToken"`
		Code      string `json:"code"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", err
	}
	if err := basetools.CheckApplyApproval(ctx, args.PlanToken, args.Code); err != nil {
		return "", err
	}
	f.writes++
	return `{"applied":[]}`, nil
}

func applyArgs(token, code string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"planToken": token, "code": code})
	return raw
}

func gatedApply(params Params, store execapproval.Store, inner *fakeApply) agentcore.Tool {
	return gateApplyTool(agentcore.ToolSet{basetools.ResourcesApplyToolName: inner}, params, store)[basetools.ResourcesApplyToolName]
}

// The turn that made the plan holds its token, but nobody has agreed to it.
func TestApplyInTheTurnThatPlannedIsRefused(t *testing.T) {
	inner := &fakeApply{}
	tool := gatedApply(Params{UserText: "scale api to 3", ApprovalText: "scale api to 3", ProjectName: "demo"},
		execapproval.NewMemoryStore(), inner)

	_, err := tool.Execute(context.Background(), applyArgs("tok-1", "plan-abc"))
	if err == nil || inner.writes != 0 {
		t.Fatalf("applied without approval: err=%v writes=%d", err, inner.writes)
	}
	if !strings.Contains(err.Error(), `"Approve plan plan-abc"`) {
		t.Fatalf("refusal must tell the model how to ask for approval: %v", err)
	}
}

func TestPlanCardApprovalAppliesOnceAndAReplayIsRefused(t *testing.T) {
	store := execapproval.NewMemoryStore()
	inner := &fakeApply{}
	params := Params{UserText: "Approve the plan", ProjectName: "demo",
		ApprovePlan: &PlanApproval{Token: "tok-1", Manifests: []string{"kind: Workload\n"}}}

	tool := gatedApply(params, store, inner)
	if _, err := tool.Execute(context.Background(), applyArgs("tok-2", "plan-other")); err == nil {
		t.Fatal("a card approval applied a different plan")
	}
	if _, err := tool.Execute(context.Background(), applyArgs("tok-1", "plan-abc")); err != nil {
		t.Fatalf("approved apply: %v", err)
	}
	if _, err := tool.Execute(context.Background(), applyArgs("tok-1", "plan-abc")); err == nil {
		t.Fatal("the same turn applied twice")
	}
	replay := gatedApply(params, store, inner)
	if _, err := replay.Execute(context.Background(), applyArgs("tok-1", "plan-abc")); err == nil {
		t.Fatal("a replayed approval applied again")
	}
	if inner.writes != 1 {
		t.Fatalf("writes = %d, want exactly one", inner.writes)
	}
}

func TestExactApprovalMessageAppliesThatPlan(t *testing.T) {
	store := execapproval.NewMemoryStore()
	approve := basetools.PlanApprovalPrefix + "plan-abc"

	for _, tc := range []struct {
		name         string
		user, raw    string
		token, code  string
		wantApproved bool
	}{
		{"exact code", approve, approve, "tok-1", "plan-abc", true},
		{"exact token", basetools.PlanApprovalPrefix + "tok-2", basetools.PlanApprovalPrefix + "tok-2", "tok-2", "plan-xyz", true},
		{"other plan", approve, approve, "tok-3", "plan-other", false},
		{"extra words", approve + " please", approve + " please", "tok-4", "plan-abc", false},
		{"joined parts", approve, "", "tok-5", "plan-abc", false},
		{"quoted inside", "ok: " + approve, "ok: " + approve, "tok-6", "plan-abc", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := &fakeApply{}
			tool := gatedApply(Params{UserText: tc.user, ApprovalText: tc.raw, ProjectName: "demo"}, store, inner)
			_, err := tool.Execute(context.Background(), applyArgs(tc.token, tc.code))
			if got := err == nil && inner.writes == 1; got != tc.wantApproved {
				t.Fatalf("approved=%v (err=%v), want %v", got, err, tc.wantApproved)
			}
		})
	}
}

// A text approval is spent per message. The same message delivered again (a
// client, replica or A2A retry) is refused even against a re-plan of the same
// manifests, which carries a new token but the same code. A new message from
// the person with that code still works, as after an apply that failed
// before it reached the platform.
func TestTextApprovalIsOncePerMessage(t *testing.T) {
	store := execapproval.NewMemoryStore()
	inner := &fakeApply{}
	approve := basetools.PlanApprovalPrefix + "plan-abc"
	message := func(id string) Params {
		return Params{UserText: approve, ApprovalText: approve, ProjectName: "demo",
			ContextID: "conv-1", MessageID: id}
	}

	if _, err := gatedApply(message("msg-1"), store, inner).Execute(context.Background(), applyArgs("tok-1", "plan-abc")); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	if _, err := gatedApply(message("msg-1"), store, inner).Execute(context.Background(), applyArgs("tok-2", "plan-abc")); err == nil {
		t.Fatal("a redelivered approval message applied a re-plan of the same manifests")
	}
	if _, err := gatedApply(message("msg-2"), store, inner).Execute(context.Background(), applyArgs("tok-3", "plan-abc")); err != nil {
		t.Fatalf("a new approval message with the same code: %v", err)
	}
	if inner.writes != 2 {
		t.Fatalf("writes = %d, want 2 (one per approval message)", inner.writes)
	}
}

// The ledger keys a text approval on what the person typed, not on the code
// the model passes to resources_apply. Otherwise a redelivered message could
// apply again just because the model passed a different code the second time.
func TestTextApprovalReplayIsRefusedWhateverCodeTheModelPasses(t *testing.T) {
	approve := basetools.PlanApprovalPrefix + "tok-1"
	for _, tc := range []struct {
		name      string
		messageID string
	}{
		{"with a message id", "msg-1"},
		{"without a message id", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := execapproval.NewMemoryStore()
			inner := &fakeApply{}
			params := Params{UserText: approve, ApprovalText: approve, ProjectName: "demo",
				ContextID: "conv-1", MessageID: tc.messageID}

			if _, err := gatedApply(params, store, inner).Execute(context.Background(), applyArgs("tok-1", "")); err != nil {
				t.Fatalf("first approval: %v", err)
			}
			if _, err := gatedApply(params, store, inner).Execute(context.Background(), applyArgs("tok-1", "plan-abc")); err == nil {
				t.Fatal("a redelivered approval applied again because the model passed a different code")
			}
			if inner.writes != 1 {
				t.Fatalf("writes = %d, want 1", inner.writes)
			}
		})
	}
}

type agingStore struct {
	now   time.Time
	spent map[execapproval.Key]agingRecord
}

type agingRecord struct {
	at        time.Time
	permanent bool
}

func newAgingStore(now time.Time) *agingStore {
	return &agingStore{now: now, spent: map[execapproval.Key]agingRecord{}}
}

func (s *agingStore) Consume(_ context.Context, project, token string) (bool, error) {
	return s.consume(project, token, false), nil
}

func (s *agingStore) ConsumePermanently(_ context.Context, project, key string) (bool, error) {
	return s.consume(project, key, true), nil
}

func (s *agingStore) consume(project, token string, permanent bool) bool {
	key := execapproval.MakeKey(project, token)
	if r, ok := s.spent[key]; ok && (r.permanent || s.now.Sub(r.at) < 24*time.Hour) {
		return false
	}
	s.spent[key] = agingRecord{at: s.now, permanent: permanent}
	return true
}

func TestTextApprovalReplayIsRefusedAtAnyAge(t *testing.T) {
	approve := basetools.PlanApprovalPrefix + "plan-abc"
	for _, tc := range []struct {
		name      string
		messageID string
	}{
		{"with a message id", "msg-1"},
		{"without a message id", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1_000_000, 0)
			store := newAgingStore(start)
			inner := &fakeApply{}
			params := Params{UserText: approve, ApprovalText: approve, ProjectName: "demo",
				ContextID: "conv-1", MessageID: tc.messageID}

			if _, err := gatedApply(params, store, inner).Execute(context.Background(), applyArgs("tok-1", "plan-abc")); err != nil {
				t.Fatalf("first approval: %v", err)
			}
			for i, age := range []time.Duration{time.Second, plantoken.TTL / 2, plantoken.TTL + time.Second,
				25 * time.Hour, 365 * 24 * time.Hour} {
				store.now = start.Add(age)
				_, err := gatedApply(params, store, inner).Execute(context.Background(),
					applyArgs("tok-replay-"+strconv.Itoa(i), "plan-abc"))
				if err == nil {
					t.Fatalf("a replay %v later applied a re-plan of the same manifests", age)
				}
			}
			if inner.writes != 1 {
				t.Fatalf("writes = %d, want 1", inner.writes)
			}
		})
	}
}

func TestSpentTextApprovalCanBeRenewedWithThePlanToken(t *testing.T) {
	store := execapproval.NewMemoryStore()
	inner := &fakeApply{}
	byCode := basetools.PlanApprovalPrefix + "plan-abc"
	text := func(typed string) Params {
		return Params{UserText: typed, ApprovalText: typed, ProjectName: "demo", ContextID: "conv-1"}
	}

	if _, err := gatedApply(text(byCode), store, inner).Execute(context.Background(), applyArgs("tok-1", "plan-abc")); err != nil {
		t.Fatalf("first approval: %v", err)
	}
	_, err := gatedApply(text(byCode), store, inner).Execute(context.Background(), applyArgs("tok-2", "plan-abc"))
	if err == nil {
		t.Fatal("a spent code applied again")
	}
	if !strings.Contains(err.Error(), `"`+basetools.PlanApprovalPrefix+`tok-2"`) {
		t.Fatalf("refusal must offer approval by the new plan's token: %v", err)
	}
	byToken := basetools.PlanApprovalPrefix + "tok-2"
	if _, err := gatedApply(text(byToken), store, inner).Execute(context.Background(), applyArgs("tok-2", "plan-abc")); err != nil {
		t.Fatalf("approval by the new plan's token: %v", err)
	}
	if inner.writes != 2 {
		t.Fatalf("writes = %d, want 2", inner.writes)
	}
}

func TestChangeToolsAreLeftOutWithoutAnApprovalStore(t *testing.T) {
	tools := agentcore.ToolSet{
		basetools.ResourcesValidateToolName: &fakeApply{},
		basetools.ResourcesPlanToolName:     &fakeApply{},
		basetools.ResourcesApplyToolName:    &fakeApply{},
	}
	got := gateApplyTool(tools, Params{}, nil)
	if _, ok := got[basetools.ResourcesApplyToolName]; ok {
		t.Fatal("resources_apply offered without an approval store")
	}
	if _, ok := got[basetools.ResourcesPlanToolName]; ok {
		t.Fatal("resources_plan offered without an approval store")
	}
	if _, ok := got[basetools.ResourcesValidateToolName]; !ok {
		t.Fatal("resources_validate removed; it changes nothing")
	}
	if len(tools) != 3 {
		t.Fatal("the caller's tool set was modified")
	}
}

func TestApplyIsUnchangedWhenTheToolIsAbsent(t *testing.T) {
	tools := agentcore.ToolSet{"other": &fakeApply{}}
	if got := gateApplyTool(tools, Params{}, execapproval.NewMemoryStore()); len(got) != 1 || got["other"] != tools["other"] {
		t.Fatalf("tools changed: %v", got)
	}
}
