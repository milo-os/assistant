package basetools

import (
	"context"
	"errors"
)

// ApplyApproval decides whether the person approved applying one plan in the
// current turn. resources_apply asks it only after the plan token has been
// checked against the manifests and the platform has accepted them again, so
// a mistaken call never uses up an approval. Approve returns nil to let the
// writes go ahead, and an error the model sees otherwise.
type ApplyApproval interface {
	Approve(ctx context.Context, planToken, approvalCode string) error
}

type applyApprovalKey struct{}

// WithApplyApproval returns ctx carrying a, for resources_apply to consult.
func WithApplyApproval(ctx context.Context, a ApplyApproval) context.Context {
	return context.WithValue(ctx, applyApprovalKey{}, a)
}

// errNoApproval is what resources_apply says when nothing in the turn can
// approve a write. The plan token alone is never enough: the model receives
// it, so it proves which plan, not that anybody agreed to it.
var errNoApproval = errors.New("nothing was changed: no person has approved this plan in this turn, " +
	"and only a person can. Show them the plan, ask them to approve it, and stop until they answer")

// CheckApplyApproval asks the approval carried on ctx; with none, it
// refuses. resources_apply calls it before its first write.
func CheckApplyApproval(ctx context.Context, planToken, approvalCode string) error {
	a, _ := ctx.Value(applyApprovalKey{}).(ApplyApproval)
	if a == nil {
		return errNoApproval
	}
	return a.Approve(ctx, planToken, approvalCode)
}
