package capability

import (
	"log/slog"
	"net/url"
)

// Caller-identity propagation to provider MCP servers, so one can read the
// customer's own resources as the caller. See
// docs/architecture/identity-and-access.md.
//
// A capability document is provider-controlled data, so naming an endpoint
// grants it nothing: identity travels only to hosts an OPERATOR sanctioned
// (ComposeOptions.IdentityForwardHosts), empty by default. Knowledge sources
// and skill bodies are excluded outright — plain GETs of URLs named by that
// same document, so a credential there would only widen the harvest surface.
const (
	// AuthorizationHeader carries the caller's own bearer token.
	AuthorizationHeader = "Authorization"
	// ProjectHeader names the project the provider must read in. It comes from
	// the authorized request, never from a model-supplied tool argument.
	ProjectHeader = "X-Datum-Project"
)

// CallerIdentity is the calling user's own credential, threaded through
// composition so a sanctioned provider endpoint can act as that user. String
// and LogValue redact the token: a stray %v or slog attribute must never be
// able to write a live credential to a log.
type CallerIdentity struct {
	// BearerToken is the raw token from the authenticated request. Empty means
	// there is no identity to forward (any non-HTTP entry point).
	BearerToken string
}

func (c CallerIdentity) String() string { return "capability.CallerIdentity{BearerToken:[redacted]}" }

func (c CallerIdentity) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// identityHeaders returns the headers endpoint may receive, or nil for none.
// The pair travels together, and it fails closed on every doubt: no token, no
// authorized project, no sanctioned list, an unparseable endpoint, or a host
// outside it.
func identityHeaders(endpoint string, caller CallerIdentity, project string, sanctioned *hostAllowList) map[string]string {
	if caller.BearerToken == "" || project == "" || sanctioned.empty() {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	// Match on the name only, never on resolved addresses: an operator
	// sanctions a NAME, and an attacker who can steer DNS must not be able to
	// talk their way into the list via a shared gateway CIDR.
	if host == "" || !sanctioned.permits(host, nil) {
		return nil
	}
	return map[string]string{
		AuthorizationHeader: "Bearer " + caller.BearerToken,
		ProjectHeader:       project,
	}
}
