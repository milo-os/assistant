package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/basetools"
)

// Plan is a change resources_plan settled, as a client draws it: one row per
// resource and the token that applies exactly these manifests. Only emitted
// where the client draws plan cards (see [drawsCards]).
type Plan struct {
	// ID names the plan in this conversation. It is derived from the token,
	// so the same plan always has the same ID and no store is needed.
	ID        string
	Token     string
	ExpiresAt string
	Resources []PlanResource
}

// PlanResource is one row of a plan, in apply order.
type PlanResource struct {
	Kind      string
	Name      string
	Namespace string
	// Action is "create" or "update".
	Action   string
	Summary  string
	Diff     []string
	Manifest string
}

// PlanResult is how applying a plan went.
type PlanResult struct {
	PlanID  string
	OK      bool
	Applied []basetools.AppliedResource
	// Error is the platform's or the token check's reason, when not OK.
	Error string
}

// PlanApproval is a person approving a plan card: the manifests and token the
// card was drawn from, handed back unchanged. Nothing here is trusted on its
// own. resources_apply re-derives the token from the manifests and refuses a
// mismatch, so an edited manifest or a token from another plan changes
// nothing, and a plan that was already applied no longer matches the
// resources it would replace.
type PlanApproval struct {
	Token     string
	Manifests []string
}

// planID names a plan by its token.
func planID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "plan-" + hex.EncodeToString(sum[:8])
}

// planFromResult reads a successful resources_plan result. It returns nil for
// a plan the platform rejected: that has no token, so there is nothing to
// approve, and the model goes on to fix it.
func planFromResult(content string) *Plan {
	var out basetools.PlanOutput
	if json.Unmarshal([]byte(content), &out) != nil || !out.Valid || out.PlanToken == "" || len(out.Results) == 0 {
		return nil
	}
	p := &Plan{ID: planID(out.PlanToken), Token: out.PlanToken, ExpiresAt: out.ExpiresAt,
		Resources: make([]PlanResource, 0, len(out.Results))}
	for _, r := range out.Results {
		p.Resources = append(p.Resources, PlanResource{
			Kind: r.Kind, Name: r.Name, Namespace: r.Namespace, Action: r.Action,
			Summary: r.Summary, Diff: r.Diff, Manifest: r.Manifest,
		})
	}
	return p
}

// applyResultOf reads a resources_apply result for the plan whose token the
// call carried.
func applyResultOf(id string, result agentcore.ToolResult) PlanResult {
	if result.IsError {
		return PlanResult{PlanID: id, Error: result.Content}
	}
	var out basetools.ApplyOutput
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		return PlanResult{PlanID: id, Error: "the apply result could not be read"}
	}
	return PlanResult{PlanID: id, OK: true, Applied: out.Applied}
}

// applyTokenOf reads the plan token a resources_apply call carries.
func applyTokenOf(input json.RawMessage) string {
	var args struct {
		PlanToken string `json:"planToken"`
	}
	_ = json.Unmarshal(input, &args)
	return args.PlanToken
}

// planText renders a plan the way conversation memory keeps it, for the same
// reason as [choicesText]: history stores prose only, and the manifests the
// card showed were never in the model's own text.
func planText(p Plan) string {
	rows := make([]string, 0, len(p.Resources))
	for _, r := range p.Resources {
		rows = append(rows, fmt.Sprintf("%s %s %s", r.Action, r.Kind, r.Name))
	}
	return "Plan shown for approval: " + strings.Join(rows, ", ") + "."
}

// endsTurnOn returns the loop's turn-ending rule for surface. Where the
// client draws cards, a question asked with ask_choice and a plan that can be
// approved both hand the turn to the person: their answer, or their click on
// Approve, is the next message. Elsewhere nothing ends a turn early.
func endsTurnOn(surface *Surface) func(agentcore.ToolResult) bool {
	if !drawsCards(surface) {
		return nil
	}
	return func(r agentcore.ToolResult) bool {
		if r.IsError {
			return false
		}
		switch r.Name {
		case AskChoiceToolName:
			return true
		case basetools.ResourcesPlanToolName:
			return planFromResult(r.Content) != nil
		}
		return false
	}
}

// showsPlans reports whether this turn reports plans as cards: where the
// client draws them, or when the turn is itself a card's approval.
func (s *Stream) showsPlans() bool {
	return drawsCards(s.params.Surface) || s.params.ApprovePlan != nil
}

// queuePlanEvents queues the plan event a resources_plan result settles, or
// the result event of a resources_apply call made for a plan, to follow the
// tool result event Recv is about to return.
func (s *Stream) queuePlanEvents(result agentcore.ToolResult) {
	if !s.showsPlans() {
		return
	}
	switch result.Name {
	case basetools.ResourcesPlanToolName:
		if result.IsError || !drawsCards(s.params.Surface) {
			return
		}
		if p := planFromResult(result.Content); p != nil {
			s.plans = append(s.plans, *p)
			s.pending = append(s.pending, Event{Kind: EventPlan, Plan: p})
		}
	case basetools.ResourcesApplyToolName:
		id, ok := s.applyCalls[result.ToolCallID]
		if !ok {
			return
		}
		r := applyResultOf(id, result)
		s.pending = append(s.pending, Event{Kind: EventPlanResult, PlanResult: &r})
	}
}

// approvedPlanModel makes a turn that approves a plan card begin with the
// resources_apply call for exactly that plan, with no model decision in
// between: the click on Approve is the explicit yes, and it is the person's
// own authenticated request, so model text can never stand in for it. Every
// later step goes to the real model, which reads the apply result and tells
// the person what happened.
type approvedPlanModel struct {
	inner    agentcore.Model
	approval PlanApproval
	mu       sync.Mutex
	sent     bool
}

func (m *approvedPlanModel) ModelID() string { return m.inner.ModelID() }

func (m *approvedPlanModel) Stream(ctx context.Context, req agentcore.Request) (agentcore.StreamReader, error) {
	m.mu.Lock()
	first := !m.sent
	m.sent = true
	m.mu.Unlock()
	if !first {
		return m.inner.Stream(ctx, req)
	}
	input, err := json.Marshal(struct {
		Manifests []string `json:"manifests"`
		PlanToken string   `json:"planToken"`
	}{Manifests: m.approval.Manifests, PlanToken: m.approval.Token})
	if err != nil {
		return nil, err
	}
	return agentcore.StreamFunc(func(send agentcore.SendFunc) {
		if !send(agentcore.StreamPart{Kind: agentcore.StreamPartToolCall,
			ToolCall: &agentcore.ToolCall{ID: "approved-plan", Name: basetools.ResourcesApplyToolName, Input: input}}) {
			return
		}
		send(agentcore.StreamPart{Kind: agentcore.StreamPartStepFinish, FinishReason: agentcore.FinishToolCalls})
	}, nil), nil
}
