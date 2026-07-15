package auth

import (
	"context"
	"strings"
)

// devGrant is the parsed grant for a single dev token.
type devGrant struct {
	subject  string
	projects []string
}

// ParseDevTokens parses the AUTH_DEV_TOKENS string into a token→grant map.
//
// Format (entries separated by ';', fields by ':'):
//
//	token:subject:projA,projB;token2:subject2:projC;token3:sub3:*
//
// Whitespace around entries/fields is trimmed; a project grant of '*' grants
// every project. Malformed entries (missing token or subject, fewer than three
// fields) are skipped. Ported from the TS parseDevTokens.
func ParseDevTokens(raw string) map[string]devGrant {
	out := map[string]devGrant{}
	for _, entry := range strings.Split(raw, ";") {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		// Split into at most 3 fields; a project list never contains ':'.
		firstColon := strings.IndexByte(trimmed, ':')
		if firstColon == -1 {
			continue
		}
		secondColon := strings.IndexByte(trimmed[firstColon+1:], ':')
		if secondColon == -1 {
			continue
		}
		secondColon += firstColon + 1
		token := strings.TrimSpace(trimmed[:firstColon])
		subject := strings.TrimSpace(trimmed[firstColon+1 : secondColon])
		projectsRaw := strings.TrimSpace(trimmed[secondColon+1:])
		if token == "" || subject == "" {
			continue
		}
		var projects []string
		for _, p := range strings.Split(projectsRaw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				projects = append(projects, p)
			}
		}
		out[token] = devGrant{subject: subject, projects: projects}
	}
	return out
}

// DevAuthenticator implements [Authenticator] over the static AUTH_DEV_TOKENS map.
type DevAuthenticator struct {
	tokens map[string]devGrant
}

// NewDevAuthenticator parses rawTokens and returns a [DevAuthenticator].
func NewDevAuthenticator(rawTokens string) *DevAuthenticator {
	return &DevAuthenticator{tokens: ParseDevTokens(rawTokens)}
}

// Size reports the number of configured tokens (for boot-time logging).
func (d *DevAuthenticator) Size() int { return len(d.tokens) }

// Authenticate resolves a bearer token to a [Principal]. An unknown token
// yields a 401 [*Error].
func (d *DevAuthenticator) Authenticate(_ context.Context, bearerToken string) (Principal, error) {
	grant, ok := d.tokens[bearerToken]
	if !ok {
		return Principal{}, Unauthenticated("Unknown or invalid bearer token")
	}
	return PrincipalFromProjects(grant.subject, grant.projects), nil
}
