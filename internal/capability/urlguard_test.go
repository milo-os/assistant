package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// staticResolver answers from a fixed host->IPs map so the SSRF guard can be
// exercised without real DNS.
func staticResolver(m map[string][]string) ipResolver {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		ipStrs, ok := m[host]
		if !ok {
			return nil, fmt.Errorf("no such host %q", host)
		}
		addrs := make([]netip.Addr, 0, len(ipStrs))
		for _, s := range ipStrs {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		return addrs, nil
	}
}

func TestIPGuard_BlocksNonRoutableAllowsPublic(t *testing.T) {
	blocked := map[string]string{
		"imds (cloud metadata)": "169.254.169.254",
		"private 10/8":          "10.0.0.5",
		"private 192.168/16":    "192.168.1.1",
		"loopback":              "127.0.0.1",
		"unspecified":           "0.0.0.0",
		"ula fd00::/8":          "fd00::1",
		"link-local v6":         "fe80::1",
	}
	for name, ip := range blocked {
		g := newIPGuard(false, staticResolver(map[string][]string{"h": {ip}}))
		if err := g.checkURL(context.Background(), "http://h/x"); err == nil {
			t.Errorf("%s (%s) should be blocked", name, ip)
		}
	}

	gPub := newIPGuard(false, staticResolver(map[string][]string{"h": {"93.184.216.34"}}))
	if err := gPub.checkURL(context.Background(), "http://h/x"); err != nil {
		t.Errorf("public host should pass: %v", err)
	}
	// Non-http(s) schemes are refused regardless of host.
	if err := gPub.checkURL(context.Background(), "file:///etc/passwd"); err == nil {
		t.Error("file:// scheme should be refused")
	}
	// Fail-closed: a mixed public+private answer (DNS rebinding shape) is refused.
	gMix := newIPGuard(false, staticResolver(map[string][]string{"h": {"93.184.216.34", "169.254.169.254"}}))
	if err := gMix.checkURL(context.Background(), "http://h/x"); err == nil {
		t.Error("mixed public+private (rebinding) answer should be refused")
	}
	// allowPrivate is the dev escape hatch: it permits loopback.
	gDev := newIPGuard(true, staticResolver(map[string][]string{"h": {"127.0.0.1"}}))
	if err := gDev.checkURL(context.Background(), "http://h/x"); err != nil {
		t.Errorf("allowPrivate should permit loopback: %v", err)
	}
	// But allowPrivate must NOT re-expose cloud metadata / link-local — the
	// highest-value SSRF target stays blocked in every mode.
	gDevMeta := newIPGuard(true, staticResolver(map[string][]string{"h": {"169.254.169.254"}}))
	if err := gDevMeta.checkURL(context.Background(), "http://h/x"); err == nil {
		t.Error("allowPrivate must still block link-local/IMDS (169.254.169.254)")
	}
	// A private RFC1918 in-cluster address (the real gateway/provider posture)
	// is permitted under allowPrivate.
	gDevPriv := newIPGuard(true, staticResolver(map[string][]string{"h": {"10.96.0.10"}}))
	if err := gDevPriv.checkURL(context.Background(), "http://h/x"); err != nil {
		t.Errorf("allowPrivate should permit an in-cluster private address: %v", err)
	}
}

// A knowledge source pointed at an internal/metadata address is refused by the
// guarded client at dial time: the body never reaches the prompt, while the
// service header and concepts survive (degrade, not fail).
func TestComposeKnowledge_RefusesPrivateSource(t *testing.T) {
	var buf bytes.Buffer
	doc := streamcoDoc(func(d *CapabilityDocument) {
		d.Spec.Tools = &Tools{}
		d.Spec.Knowledge.Sources = []KnowledgeSource{{Type: KnowledgeLLMDocs, Title: "Creds", URL: "http://metadata.internal/creds"}}
	})
	composed, _ := Compose(context.Background(), []CapabilityDocument{doc}, ComposeOptions{
		resolver: staticResolver(map[string][]string{"metadata.internal": {"169.254.169.254"}}),
		Logger:   testLogger(&buf),
	})
	defer composed.Close()

	a := composed.SystemPromptAddendum
	if !strings.Contains(a, streamcoHeader) || !strings.Contains(a, "A live media stream") {
		t.Fatalf("header + concepts must survive an SSRF-refused source:\n%s", a)
	}
	if strings.Contains(a, "### Creds") {
		t.Fatalf("SSRF-refused source body must not appear:\n%s", a)
	}
	if !strings.Contains(buf.String(), "knowledge.fetch_failed") {
		t.Fatalf("expected a fetch_failed warning; logs:\n%s", buf.String())
	}
}

// load_skill against an internal/metadata source is refused and surfaces only
// the user-safe "temporarily unavailable" error.
func TestComposeSkill_RefusesPrivateSource(t *testing.T) {
	composed, _ := Compose(context.Background(), []CapabilityDocument{
		skillDoc("s.example", "s", "leaky", "reads internal metadata", "http://metadata.internal/creds"),
	}, ComposeOptions{
		resolver: staticResolver(map[string][]string{"metadata.internal": {"169.254.169.254"}}),
		Logger:   slog.New(slog.DiscardHandler),
	})
	defer composed.Close()

	_, err := composed.Tools[LoadSkillToolName].Execute(context.Background(), json.RawMessage(`{"skill":"s__leaky"}`))
	if err == nil {
		t.Fatal("SSRF-refused skill source should error")
	}
	if !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatalf("error should be user-safe, got: %v", err)
	}
}

// The dev escape hatch: AllowPrivateNetworks permits a loopback skill source, so
// local overlays keep working.
func TestComposeSkill_AllowPrivateNetworksPermitsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("the procedure body"))
	}))
	defer srv.Close()

	composed, _ := Compose(context.Background(), []CapabilityDocument{
		skillDoc("s.example", "s", "ok", "a skill", srv.URL+"/skill.md"),
	}, ComposeOptions{AllowPrivateNetworks: true})
	defer composed.Close()

	out, err := composed.Tools[LoadSkillToolName].Execute(context.Background(), json.RawMessage(`{"skill":"s__ok"}`))
	if err != nil {
		t.Fatalf("dev path (AllowPrivateNetworks) should permit loopback: %v", err)
	}
	if !strings.Contains(out, "the procedure body") {
		t.Fatalf("skill body missing on dev path: %q", out)
	}
}

// The MCP connect path checks the resolved endpoint host before the inner
// connector touches the network.
func TestGuardedConnector_RefusesPrivateEndpointBeforeConnecting(t *testing.T) {
	called := false
	inner := func(context.Context, string) (mcpSession, error) { called = true; return newFakeSession(), nil }

	guard := newIPGuard(false, staticResolver(map[string][]string{"internal-mcp": {"10.1.2.3"}}))
	if _, err := guardedConnector(inner, guard)(context.Background(), "http://internal-mcp/mcp"); err == nil {
		t.Fatal("private MCP endpoint should be refused")
	}
	if called {
		t.Fatal("inner connector must not be reached for a blocked endpoint")
	}

	guardPub := newIPGuard(false, staticResolver(map[string][]string{"public-mcp": {"93.184.216.34"}}))
	if _, err := guardedConnector(inner, guardPub)(context.Background(), "http://public-mcp/mcp"); err != nil {
		t.Fatalf("public MCP endpoint should connect: %v", err)
	}
	if !called {
		t.Fatal("inner connector should be reached for an allowed endpoint")
	}
}
