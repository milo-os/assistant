// Package auth splits request handling into two deliberately separate seams:
//
//   - [Authenticator] — "who are you": a bearer token → a [Principal]. A token
//     the control plane will not vouch for yields an [*Error] with status 401.
//   - [Authorizer] — "may you act on this project": a decision over a Principal
//     and a project name, yielding an [*Error] with status 403 on deny.
//
// Both are answered by the control plane and nothing else. Identity comes from
// a Kubernetes TokenReview; project access from a SubjectAccessReview, resolved
// by the platform's OpenFGA-backed webhook. The service holds no credential
// database of its own and never decides access locally, so there is no path
// where a token's own contents can widen what it may reach.
//
// Both paths fail closed: any error, timeout or undecidable answer is a
// rejection, never a grant.
package auth

import (
	"context"
	"regexp"
	"strings"
)

// Principal is the authenticated identity, as resolved by the control plane.
//
// It carries no grants. What a subject may reach is decided per request by the
// [Authorizer] against the control plane — deliberately not derivable from the
// credential, so a token cannot describe its own authority.
type Principal struct {
	// Subject is the stable identifier the control plane returned for the
	// token (the TokenReview's user.username).
	Subject string
}

// Error is an authentication (401) or authorization (403) failure.
type Error struct {
	// Status is 401 (authentication) or 403 (authorization).
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Unauthenticated returns a 401 [*Error].
func Unauthenticated(message string) *Error { return &Error{Status: 401, Message: message} }

// Unauthorized returns a 403 [*Error].
func Unauthorized(message string) *Error { return &Error{Status: 403, Message: message} }

// Authenticator resolves a bearer token to a [Principal], or returns a 401 [*Error].
type Authenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (Principal, error)
}

// Authorizer decides whether principal may act on projectName. It returns nil
// on allow and a 403 [*Error] on deny. Context-aware so a control-plane
// SubjectAccessReview can slot in behind this interface unchanged.
type Authorizer interface {
	AuthorizeProject(ctx context.Context, principal Principal, projectName string) error
}

var bearerRE = regexp.MustCompile(`(?i)^Bearer\s+(.+)$`)

// ExtractBearerToken pulls the token out of an "Authorization: Bearer <token>"
// header value. It returns "" when absent or malformed.
func ExtractBearerToken(authorization string) string {
	m := bearerRE.FindStringSubmatch(strings.TrimSpace(authorization))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}
