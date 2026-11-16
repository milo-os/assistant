package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/milo-os/assistant/agentcore"
	"github.com/milo-os/assistant/internal/basetools"
	"github.com/milo-os/assistant/internal/execapproval"
	"github.com/milo-os/assistant/internal/plantoken"
)

// textApprovalWindow is the span of one bucket a text approval is recorded
// under when its message carries no identity (see [planApproval.consumeText]).
// Two buckets are recorded, so a replay is refused for between one and two
// of them: never longer than a plan token lives.
const textApprovalWindow = plantoken.TTL / 2

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
func gateApplyTool(tools agentcore.ToolSet, params Params, store execapproval.Store) agentcore.ToolSet {
	apply, ok := tools[basetools.ResourcesApplyToolName]
	if !ok {
		return tools
	}
	gated := make(agentcore.ToolSet, len(tools))
	for name, tool := range tools {
		gated[name] = tool
	}
	approval := &planApproval{
		project:   params.ProjectName,
		contextID: params.ContextID,
		messageID: params.MessageID,
		store:     store,
		now:       time.Now,
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
	return t.inner.Execute(basetools.WithApplyApproval(ctx, t.approval), input)
}

// planApproval is one turn's approval: at most one plan, applied at most once.
type planApproval struct {
	cardToken string
	ref       string
	project   string
	contextID string
	// messageID is the transport's identity for the message that carried a
	// text approval; empty where the transport has none.
	messageID string
	store     execapproval.Store
	now       func() time.Time

	mu   sync.Mutex
	used bool
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
	fresh, err := a.consume(ctx, planToken, approvalCode)
	if err != nil {
		return fmt.Errorf("nothing was changed: the approval could not be recorded: %w", err)
	}
	if !fresh {
		return errors.New("nothing was changed: this approval was already used. Plan again and ask the " +
			"person to approve what comes back")
	}
	a.used = true
	return nil
}

// consume records this approval in the shared ledger, reporting whether it
// had not been recorded before. A plan card approval is keyed on the plan
// token it carries: the card is drawn once per plan, and its token is spent
// by the first apply. A text approval cannot be keyed that way, because its
// code is the same for every re-plan of the same manifests while each re-plan
// mints a new token, so an "Approve plan" message delivered again (a client
// or A2A retry) would pass against the next plan. It is keyed on the message
// instead; see consumeText.
func (a *planApproval) consume(ctx context.Context, planToken, approvalCode string) (bool, error) {
	if a.cardToken != "" && planToken == a.cardToken {
		return a.store.Consume(ctx, a.project, planApprovalKey(planToken))
	}
	return a.consumeText(ctx)
}

// consumeText makes a text approval once-only per message. It keys on a.ref,
// the approval the person typed and Approve verified, never on the code the
// model passed to resources_apply: the model can pass a different code on a
// redelivery, which would otherwise make the key fresh again.
//
// Where the transport identifies the message (A2A's messageId), the key is
// the conversation, that id and the typed approval: a redelivery of the same
// message is refused however many times the plan is made again, and a new
// message from the person works, for instance after an apply that failed
// before reaching the platform.
//
// /chat/stream and the apiserver's sendmessage carry no message identity, so
// there the key is the typed approval and the time: it is recorded for the
// current [textApprovalWindow] bucket and the next, and either already being
// recorded refuses it. This narrows a replay rather than closing it: the same
// approval is refused for at least one window and at most two (7.5 to 15
// minutes), so it stops prompt client retries, but a replay arriving after
// both windows is accepted. Giving those surfaces a per-request id would close
// it. The cost meanwhile is that a person approving the same plan again within
// that span, after a failed apply, is refused too and has to wait or re-plan.
func (a *planApproval) consumeText(ctx context.Context) (bool, error) {
	if a.messageID != "" {
		return a.store.Consume(ctx, a.project, textApprovalKey(a.contextID, a.messageID, a.ref))
	}
	bucket := a.now().UnixNano() / int64(textApprovalWindow)
	for _, b := range []int64{bucket, bucket + 1} {
		fresh, err := a.store.Consume(ctx, a.project,
			textApprovalKey(a.contextID, "window:"+strconv.FormatInt(b, 10), a.ref))
		if err != nil || !fresh {
			return false, err
		}
	}
	return true, nil
}

// planApprovalKey keeps plan approvals apart from exec approvals in the
// shared ledger.
func planApprovalKey(planToken string) string { return "resources_apply\x00" + planToken }

// textApprovalKey is the ledger key of one text approval: the conversation,
// what identifies the message (or its time window), and what the person typed.
func textApprovalKey(contextID, message, typed string) string {
	return "resources_apply\x00text\x00" + contextID + "\x00" + message + "\x00" + typed
}
