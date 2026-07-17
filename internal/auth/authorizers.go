package auth

import (
	"context"
	"fmt"
	"slices"
)

// ClaimsAuthorizer is the v0 / dev [Authorizer]: it decides from the project
// grants the [Principal] already carries (dev-token list or OIDC claim). This is
// the local-slice stand-in for production authorization (the SAR-based
// authorizer built by [NewSubjectAccessReviewAuthorizer]). Both implement
// [Authorizer] with identical 401/403 semantics, so swapping them is a wiring
// change with no call-site churn.
type ClaimsAuthorizer struct{}

// AuthorizeProject allows when the principal grants all projects or explicitly
// grants projectName; otherwise it returns a 403 [*Error].
func (ClaimsAuthorizer) AuthorizeProject(_ context.Context, principal Principal, projectName string) error {
	if principal.GrantAll || slices.Contains(principal.Projects, projectName) {
		return nil
	}
	return Unauthorized(fmt.Sprintf("Token does not grant access to project %q", projectName))
}
