package capability

import (
	"log/slog"
	"net/url"
)

// Caller-identity propagation to provider MCP servers.
//
// Some provider tools read the CUSTOMER's own resources (the compute plugin's
// MCP server is the first: it clears every credential of its own and reads as
// the bearer of the token on the request). Those servers therefore need the
// caller's credential and the project to read in — and nothing else, because a
// project taken from a tool ARGUMENT would be steerable by prompt injection.
//
// Forwarding a raw end-user token is credential spreading, and a capability
// document is provider-controlled data: any document could name any endpoint
// and harvest tokens from every entitled project. So the endpoint's presence
// in a document grants it nothing. Identity travels only to hosts an OPERATOR
// has sanctioned out-of-band (ComposeOptions.IdentityForwardHosts), and the
// empty default forwards to nobody.
//
// Knowledge sources and skill bodies are deliberately excluded: they are
// static provider documents fetched with a plain GET, and the same document
// that names an MCP endpoint names those URLs, so carrying a credential there
// would widen the harvest surface for no capability gained.
const (
	// AuthorizationHeader carries the caller's own bearer token.
	AuthorizationHeader = "Authorization"
	// ProjectHeader names the project the provider must read in. It comes from
	// the authorized request, never from a model-supplied tool argument.
	ProjectHeader = "X-Datum-Project"
)

// CallerIdentity is the calling user's own credential, threaded through
// composition so a sanctioned provider endpoint can act as that user.
//
// String and LogValue redact the token: this value passes through the agent
// layer and into ComposeOptions, and a stray %v or slog attribute must not be
// able to write a live credential to a log.
type CallerIdentity struct {
	// BearerToken is the raw token from the authenticated request. Empty means
	// there is no identity to forward (any non-HTTP entry point).
	BearerToken string
}

func (c CallerIdentity) String() string { return "capability.CallerIdentity{BearerToken:[redacted]}" }

func (c CallerIdentity) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// identityHeaders returns the headers endpoint may receive, or nil when it may
// receive none. Both headers travel together — a provider that cannot read as
// the caller has no use for the project, and withholding the pair keeps the
// contract one decision rather than two.
//
// It fails closed on every doubt: no token, no authorized project, no
// sanctioned-host list, an unparseable endpoint, or a host outside the list.
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
