package a2a

import "errors"

// Plan is a change the person can approve as a card, as the stream carries
// it. Token goes back unchanged in a [PlanApproval]; the server checks it, so
// a client holding it can approve this plan and nothing else.
type Plan struct {
	ID        string
	Token     string
	ExpiresAt string
	Resources []PlanResource
}

// PlanResource is one row of a plan, in apply order. Action is "create" or
// "update"; Diff is empty for a create.
type PlanResource struct {
	Kind      string
	Name      string
	Namespace string
	Action    string
	Summary   string
	Diff      []string
	Manifest  string
}

// PlanResult is how applying a plan went.
type PlanResult struct {
	PlanID  string
	OK      bool
	Applied []AppliedResource
	Error   string
}

// AppliedResource is one thing a plan created or changed.
type AppliedResource struct {
	Action    string
	Kind      string
	Name      string
	Namespace string
}

// PlanApproval is the person clicking Approve on a plan card: the manifests
// and token the card was drawn from.
type PlanApproval struct {
	Token     string   `json:"token"`
	Manifests []string `json:"manifests"`
}

const (
	// maxApprovalManifests matches what one plan can carry.
	maxApprovalManifests = 20
	// maxApprovalTokenLen is far above a real token; it only stops a request
	// from carrying an arbitrary blob into the run.
	maxApprovalTokenLen = 4096
)

// ValidatePlanApproval checks an approval's shape before it reaches the run.
// Whether it matches a plan is decided later, by resources_apply re-deriving
// the token from the manifests; this only refuses what cannot be a plan at
// all. A nil approval is valid: the message is not an approval.
func ValidatePlanApproval(a *PlanApproval) error {
	if a == nil {
		return nil
	}
	switch {
	case a.Token == "":
		return errors.New("approvePlan.token is required")
	case len(a.Token) > maxApprovalTokenLen:
		return errors.New("approvePlan.token is too long")
	case len(a.Manifests) == 0:
		return errors.New("approvePlan.manifests is required")
	case len(a.Manifests) > maxApprovalManifests:
		return errors.New("approvePlan.manifests has more manifests than a plan can carry")
	}
	return nil
}
