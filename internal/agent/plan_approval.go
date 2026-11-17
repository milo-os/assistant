package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/basetools"
	"github.com/milo-os/assistant/internal/execapproval"
)

// maxApprovalRefLen bounds what an "Approve plan" message may carry. A plan
// token is far shorter; this only stops an arbitrary blob being compared.
const maxApprovalRefLen = 4096

// gateApplyTool hands resources_apply the approval this turn carries, if any.
// resources_plan gives the model a plan token, so the token proves which plan
// and never that a person agreed: only the person's own current message can.
// That is a click on a plan card's Approve ([Params.ApprovePlan]), or, where
// no card is drawn, a message that is exactly "Approve plan <code>" for the
// code resources_plan returned (or the plan token itself). Any other turn,
// including the one that made the plan, gets a refusal telling the model to
// show the plan and wait.
//
// With no store to record approvals in, resources_apply is left out: an
// approval nobody can remember spending could be replayed. Planning stays.
func gateApplyTool(tools agentcore.ToolSet, params Params, store execapproval.Store) agentcore.ToolSet {
	apply, ok := tools[basetools.ResourcesApplyToolName]
	if !ok {
		return tools
	}
	gated := make(agentcore.ToolSet, len(tools))
	for name, tool := range tools {
		gated[name] = tool
	}
	if store == nil {
		delete(gated, basetools.ResourcesApplyToolName)
		return gated
	}
	approval := &planApproval{
		project:   params.ProjectName,
		contextID: params.ContextID,
		store:     store,
	}
	if a := params.ApprovePlan; a != nil && a.Token != "" {
		approval.cardToken = a.Token
	}
	if ref, ok := approvedPlanRef(params.ApprovalText, params.UserText); ok {
		approval.ref = ref
	}
	gated[basetools.ResourcesApplyToolName] = &approvalGatedTool{inner: apply, approval: approval}
	return gated
}

// approvedPlanRef recognizes only a complete, unmodified "Approve plan <ref>"
// message, the same way [approvedExecToken] does for exec.
func approvedPlanRef(approvalText, userText string) (string, bool) {
	if approvalText != userText {
		return "", false
	}
	ref, ok := strings.CutPrefix(approvalText, basetools.PlanApprovalPrefix)
	if !ok || ref == "" || len(ref) > maxApprovalRefLen || strings.ContainsAny(ref, " \t\r\n") {
		return "", false
	}
	return ref, true
}

// approvalGatedTool runs resources_apply with this turn's approval on the
// context, where the tool asks it after the token checks out.
type approvalGatedTool struct {
	inner    agentcore.Tool
	approval *planApproval
}

func (t *approvalGatedTool) Definition() agentcore.ToolDefinition { return t.inner.Definition() }

func (t *approvalGatedTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	out, err := t.inner.Execute(basetools.WithApplyApproval(ctx, t.approval), input)
	if err != nil && t.approval.spent() {
		err = fmt.Errorf("%w. This approval is spent: the same plan cannot be approved again in this "+
			"conversation. Read the resource back to see what is there, then plan again from that "+
			"(a different plan gets a new approval), or start a new conversation", err)
	}
	return out, err
}

// planApproval is one turn's approval: at most one plan, applied at most once.
type planApproval struct {
	cardToken string
	ref       string
	project   string
	contextID string
	store     execapproval.Store

	mu   sync.Mutex
	used bool
	// key is what this approval was recorded under once Approve accepted it,
	// and released whether [planApproval.Release] gave it back.
	key      string
	released bool
}

// spent reports whether an apply consumed this approval and kept it.
func (a *planApproval) spent() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.key != "" && !a.released
}

// Release gives the recorded approval back, for resources_apply to call when
// nothing it allowed reached the platform. The person then approves the same
// plan again in a new message; this turn's approval stays used.
func (a *planApproval) Release(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.key == "" || a.released {
		return nil
	}
	if err := a.store.Release(ctx, a.project, a.key); err != nil {
		return err
	}
	a.released = true
	return nil
}

func (a *planApproval) Approve(ctx context.Context, planToken, approvalCode string) error {
	approved := (a.cardToken != "" && planToken == a.cardToken) ||
		(a.ref != "" && (a.ref == approvalCode || a.ref == planToken))
	if !approved {
		if a.cardToken == "" && a.ref == "" {
			return fmt.Errorf("nothing was changed: no person has approved this plan in this turn, and "+
				"only a person can. Show them the plan and ask them to approve it (where no plan card is "+
				"shown, by replying exactly %q), then stop and wait for their answer",
				basetools.PlanApprovalPrefix+approvalCode)
		}
		return errors.New("nothing was changed: the person approved a different plan in this turn. " +
			"Show them this plan and ask them to approve it before applying")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used {
		return errors.New("nothing was changed: this turn's approval was already used for one apply. " +
			"Plan again and ask the person to approve what comes back")
	}
	if a.store == nil {
		return errors.New("nothing was changed: approvals cannot be recorded right now, so none is accepted")
	}
	key := planApprovalKey(a.contextID, approvalCode)
	fresh, err := a.store.Consume(ctx, a.project, key)
	if err != nil {
		return fmt.Errorf("nothing was changed: the approval could not be recorded: %w", err)
	}
	if !fresh {
		return errors.New("nothing was changed: this approval was already used, and the same plan " +
			"cannot be approved again in this conversation. Plan again and ask the person to approve " +
			"what comes back if it differs, or start a new conversation")
	}
	a.used = true
	a.key = key
	return nil
}

// planApprovalKey is what every approval of one plan is recorded under in the
// shared ledger (which scopes it by project): the conversation and the plan's
// approval code. A card click, the typed code and the typed plan token all
// approve the same plan, so they share one key and the plan is applied at most
// once, whichever way it was approved.
//
// approvalCode is what resources_apply derived from the verified plan binding,
// never anything the model or the person passed: it is the same for every
// re-plan of the same manifests, project and replaced resource versions, while
// each re-plan mints a new token. Keying on it means an approval delivered
// again (a client, replica or A2A retry) cannot pass against a re-plan of the
// same change, and a changed plan has a different code the person has not
// approved. There is no message identity and no time in the key, so a spent
// approval stays spent at any age on every surface. The cost is that the
// person cannot approve the same plan twice in one conversation;
// resources_apply releases the approval when nothing reached the platform,
// which covers a clean retry.
func planApprovalKey(contextID, approvalCode string) string {
	return "resources_apply\x00plan\x00" + contextID + "\x00" + approvalCode
}
