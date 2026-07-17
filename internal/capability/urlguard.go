package capability

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// maxGuardedRedirects bounds how many redirects a guarded fetch follows. Each
// hop is still re-validated at dial time (see [ipGuard.dialContext]), so a
// redirect toward a blocked address is refused even mid-chain; the cap only
// stops redirect loops.
const maxGuardedRedirects = 5

// ipResolver resolves a host to its IP addresses. It is injectable so tests can
// exercise the guard without real DNS; the default uses net.DefaultResolver.
type ipResolver func(ctx context.Context, host string) ([]netip.Addr, error)

// ipGuard is the SSRF guard shared by the knowledge, skill, and MCP sinks. A
// capability document's URLs are provider-controlled, so a knowledge source or
// skill body pointed at http://169.254.169.254/ (cloud IMDS) or an internal
// service would otherwise be fetched from the assistant's trusted network
// position and surfaced to the model/user. The guard resolves the target and
// refuses non-routable destinations — loopback, RFC1918/ULA private ranges,
// link-local (incl. IMDS), the unspecified address, and multicast — at both
// static-check time and dial time. The dial-time re-check dials the vetted IP
// directly, which defeats DNS rebinding and redirect-to-metadata.
//
// allowPrivate is the dev/cluster escape hatch: local overlays address services
// over loopback and in-cluster (private) IPs, so with it set the IP policy is
// disabled entirely. It defaults to false — the safe default — and the
// integrator opts in from config for dev environments only.
type ipGuard struct {
	allowPrivate bool
	resolve      ipResolver
}

func newIPGuard(allowPrivate bool, resolver ipResolver) *ipGuard {
	if resolver == nil {
		resolver = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	return &ipGuard{allowPrivate: allowPrivate, resolve: resolver}
}

// allowedScheme rejects any URL that is not http(s). Capability sources are web
// documents and MCP endpoints; schemes like file:// or gopher:// are never
// legitimate here and are classic SSRF/exfiltration vectors.
func (g *ipGuard) allowedScheme(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
}

// checkURL statically vets a URL's scheme and resolved destination. It is used
// by the MCP connect path, whose transport the guard does not build, to refuse
// a hostile endpoint before the connector touches the network.
func (g *ipGuard) checkURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("scheme %q not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url %q has no host", rawURL)
	}
	return g.checkHost(ctx, host)
}

// checkHost resolves host and refuses it if any resolved address is blocked.
// This fails closed: a resolver returning even one non-routable address is
// treated as hostile (e.g. a rebinding response mixing a public and a private
// IP). The always-blocked set (link-local/IMDS, unspecified, multicast) applies
// even under allowPrivate; allowPrivate only relaxes loopback/RFC1918.
func (g *ipGuard) checkHost(ctx context.Context, host string) error {
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no addresses for host %q", host)
	}
	for _, ip := range ips {
		if err := g.blocked(ip); err != nil {
			return err
		}
	}
	return nil
}

// dialContext is the guarded dialer installed on the knowledge/skill HTTP
// client. It re-resolves the host at dial time, refuses any non-routable
// address, and dials the vetted IP directly so a concurrent re-resolve cannot
// swap in an unvetted (private) address between the check and the dial.
func (g *ipGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for host %q", host)
	}
	for _, ip := range ips {
		if err := g.blocked(ip); err != nil {
			return nil, err
		}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}

// checkRedirect bounds redirect chains and re-checks the scheme of each hop.
// The resolved-IP re-check happens at dial time via [ipGuard.dialContext], so a
// redirect to a blocked address is refused when the client dials it.
func (g *ipGuard) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxGuardedRedirects {
		return fmt.Errorf("stopped after %d redirects", maxGuardedRedirects)
	}
	switch req.URL.Scheme {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("redirect to disallowed scheme %q", req.URL.Scheme)
	}
}

// wrapClient returns an HTTP client that routes real dials through the guard. A
// nil transport (or the standard *http.Transport) is cloned with the guarded
// DialContext installed, preserving the base client's timeout and transport
// settings. A custom RoundTripper (e.g. an injected test fake) does its own
// transport and cannot host a dial guard, so it is left untouched — scheme and
// redirect checks still apply. wrapClient never returns nil.
func (g *ipGuard) wrapClient(base *http.Client) *http.Client {
	var (
		timeout time.Duration
		rt      http.RoundTripper
	)
	if base != nil {
		timeout = base.Timeout
		rt = base.Transport
	}
	switch t := rt.(type) {
	case nil:
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = g.dialContext
		rt = tr
	case *http.Transport:
		tr := t.Clone()
		tr.DialContext = g.dialContext
		rt = tr
	default:
		rt = t
	}
	return &http.Client{
		Transport:     rt,
		Timeout:       timeout,
		CheckRedirect: g.checkRedirect,
	}
}

// blocked reports why an address is not a permitted destination, or nil if it
// is allowed. The ALWAYS-blocked set — link-local/IMDS, the unspecified
// address, and multicast — is refused in every mode: no legitimate capability
// endpoint is ever link-local, and 169.254.169.254 (cloud metadata) is the
// single highest-value SSRF target, so it stays barred even on the dev path.
// Loopback and RFC1918/ULA private addresses are refused only when allowPrivate
// is false; in this platform the real capability endpoints (the in-cluster AI
// gateway, provider pods) resolve to private ClusterIPs, so deployments opt
// into private addressing while the always-blocked set still holds.
func (g *ipGuard) blocked(ip netip.Addr) error {
	a := ip.Unmap()
	switch {
	case a.IsUnspecified():
		return fmt.Errorf("unspecified address %s is not allowed", a)
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return fmt.Errorf("link-local address %s is not allowed", a)
	case a.IsInterfaceLocalMulticast(), a.IsMulticast():
		return fmt.Errorf("multicast address %s is not allowed", a)
	}
	if g.allowPrivate {
		return nil
	}
	switch {
	case a.IsLoopback():
		return fmt.Errorf("loopback address %s is not allowed", a)
	case a.IsPrivate():
		return fmt.Errorf("private address %s is not allowed", a)
	}
	return nil
}

// blockedIP is the strict-mode (allowPrivate=false) destination check, exposed
// for direct unit testing of the address policy.
func blockedIP(ip netip.Addr) error {
	return (&ipGuard{}).blocked(ip)
}
