// Package auth splits request handling into two deliberately separate seams so
// production authorization can slot in without touching token verification:
//
//   - [Authenticator] — "who are you": a bearer token → a [Principal] (identity
//     plus the raw project grants the credential carries). A bad token yields
//     an [*Error] with status 401.
//   - [Authorizer] — "may you act on this project": a decision over a Principal
//     and a project name, yielding an [*Error] with status 403 on deny. It is
//     context-aware so a control-plane call (a SubjectAccessReview against Milo,
//     resolved by the platform's OpenFGA-backed webhook) can slot in behind the
//     same interface.
//
// The Principal holds grants as DATA only — it does not make the decision. That
// is the Authorizer's job. This is a direct port of the TS service's src/auth.
package auth

import (
	"context"
	"regexp"
	"strings"
)

// Principal is the authenticated identity plus the project grants its
// credential carries. The grants are consumed by the v0 [ClaimsAuthorizer] and
// are opaque to a SAR-based authorizer — they are NOT the authorization decision.
type Principal struct {
	// Subject is a stable identifier (dev: the configured subject; oidc: `sub`).
	Subject string
	// GrantedProjects lists the project grants carried by the credential.
	GrantAll bool
	// Projects is the explicit grant list (ignored when GrantAll is true).
	Projects []string
}

// PrincipalFromProjects builds a data-only Principal from a fixed grant list;
// a grant of "*" means all projects.
func PrincipalFromProjects(subject string, projects []string) Principal {
	for _, p := range projects {
		if p == "*" {
			return Principal{Subject: subject, GrantAll: true}
		}
	}
	return Principal{Subject: subject, Projects: projects}
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
