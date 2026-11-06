// Package credentialpolicy is the one rule every path that carries a
// credential must obey: the credential travels only to an https URL, unless an
// operator explicitly turns on the dev-only escape hatch.
//
// The escape hatch exists because local kind playgrounds and e2e run the AI
// gateway on plaintext loopback. It is deliberately named so its danger is
// obvious, defaults off, and is never set by config/base or a production
// overlay.
package credentialpolicy

import (
	"fmt"
	"net/url"
	"strings"
)

// AllowInsecureEnv is the single opt-in that permits credential forwarding over
// plaintext http. It is read only at startup, by [internal/config].
const AllowInsecureEnv = "ALLOW_INSECURE_CREDENTIAL_FORWARDING"

// Policy decides whether a credential may be attached to a request.
type Policy struct {
	// AllowInsecure is the dev-only escape hatch. False means https only.
	AllowInsecure bool
}

// CheckURL reports whether rawURL may receive a credential. An unparseable URL
// or one without a host fails closed. The error names the host and scheme, never
// the token or the full URL (which can carry a credential in its userinfo or
// query).
func (p Policy) CheckURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("refusing to send a credential to an unparseable endpoint")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("refusing to send a credential to an endpoint with no host")
	}
	if strings.EqualFold(u.Scheme, "https") || p.AllowInsecure {
		return nil
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "no scheme"
	}
	return fmt.Errorf("refusing to send a credential to %s over %s: credentials require https (set %s=true only for local dev or e2e)",
		host, scheme, AllowInsecureEnv)
}

// CheckConfigured validates a startup-configured endpoint that will receive a
// credential. It names the environment variable rather than the host, because
// an operator reading a boot failure needs to know which setting to fix.
func (p Policy) CheckConfigured(variable, rawURL string) error {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("%s must be an absolute https:// URL", variable)
	}
	if strings.EqualFold(u.Scheme, "https") || p.AllowInsecure {
		return nil
	}
	return fmt.Errorf("%s must use https:// because it receives a credential (got %s); set %s=true only for local dev or e2e",
		variable, u.Scheme, AllowInsecureEnv)
}
