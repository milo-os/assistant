package auth

import (
	"context"
	"fmt"
	"slices"
)

// ClaimsAuthorizer is the v0 [Authorizer]: it decides from the project grants
// the [Principal] already carries (dev-token list or OIDC claim). This is the
// local-slice stand-in for production authorization (a
// [SubjectAccessReviewAuthorizer]). Both implement [Authorizer] with identical
// 401/403 semantics, so swapping them is a wiring change with no call-site churn.
type ClaimsAuthorizer struct{}

// AuthorizeProject allows when the principal grants all projects or explicitly
// grants projectName; otherwise it returns a 403 [*Error].
func (ClaimsAuthorizer) AuthorizeProject(_ context.Context, principal Principal, projectName string) error {
	if principal.GrantAll || slices.Contains(principal.Projects, projectName) {
		return nil
	}
	return Unauthorized(fmt.Sprintf("Token does not grant access to project %q", projectName))
}

// SubjectAccessReviewAuthorizer is the production authorization seam — a NAMED
// STUB, not wired in v0. In production, caller authorization moves here: instead
// of trusting the grants a credential carries, it issues a SubjectAccessReview
// against the Milo control plane (resolved by the platform's OpenFGA-backed
// webhook, with the assistant IAM role materialized by catalog IAM fan-out).
// It implements [Authorizer] with identical 401/403 semantics so it slots into
// the factory with no change to the A2A layer or any call site.
//
// TODO(agent-framework): implement the SAR call —
//
//	POST {APIURL}/apis/authorization.k8s.io/v1/subjectaccessreviews
//	{ spec: { user: <principal.Subject>, resourceAttributes: {
//	    group: "assistant.miloapis.com", resource: "conversations",
//	    verb: "create", namespace: projectName } } }
//
// using an agent-identity token obtained via RFC 8693 token exchange; allow when
// status.allowed is true, else return a 403 [*Error].
type SubjectAccessReviewAuthorizer struct {
	// APIURL is the Milo control-plane API base URL the SAR is posted to.
	APIURL string
	// AgentToken resolves the agent-identity bearer token for the SAR call
	// (RFC 8693 token exchange in production). Injected so it stays testable.
	AgentToken func(ctx context.Context, principal Principal) (string, error)
}

// AuthorizeProject is unimplemented in v0. It throws (rather than allowing or
// denying silently) so an accidental production wiring fails loud, not open.
func (SubjectAccessReviewAuthorizer) AuthorizeProject(_ context.Context, _ Principal, _ string) error {
	return Unauthorized(
		"SubjectAccessReviewAuthorizer is not implemented yet — use ClaimsAuthorizer for the local slice (see auth/authorizers.go TODO)")
}
