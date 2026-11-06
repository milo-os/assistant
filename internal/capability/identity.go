package capability

import (
	"log/slog"
	"net/url"

	"github.com/milo-os/assistant/internal/credentialpolicy"
)

// Caller-identity propagation to provider MCP servers, so one can read the
// customer's own resources as the caller. See
// docs/architecture/identity-and-access.md.
//
// A capability document is provider-controlled data, so naming an endpoint
// grants it nothing: identity travels only to hosts an OPERATOR sanctioned
// (ComposeOptions.IdentityForwardHosts), empty by default, AND only over https.
// Knowledge sources and skill bodies are excluded outright — plain GETs of URLs
// named by that same document, so a credential there would only widen the
// harvest surface.
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
//
// A non-nil error means a credential WOULD have been attached but the endpoint
// is not https and the dev escape hatch is off. The caller must skip the request
// entirely rather than send it uncredentialed: the call exists to act as the
// user, and silently dropping the identity would turn a security refusal into a
// misleading provider failure.
func identityHeaders(endpoint string, caller CallerIdentity, project string, sanctioned *hostAllowList, allowInsecure bool) (map[string]string, error) {
	if caller.BearerToken == "" || project == "" || sanctioned.empty() {
		return nil, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil
	}
	host := u.Hostname()
	// Match on the name only, never on resolved addresses: an operator
	// sanctions a NAME, and an attacker who can steer DNS must not be able to
	// talk their way into the list via a shared gateway CIDR.
	if host == "" || !sanctioned.permits(host, nil) {
		return nil, nil
	}
	if err := (credentialpolicy.Policy{AllowInsecure: allowInsecure}).CheckURL(endpoint); err != nil {
		return nil, err
	}
	return map[string]string{
		AuthorizationHeader: "Bearer " + caller.BearerToken,
		ProjectHeader:       project,
	}, nil
}
