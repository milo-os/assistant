package config

import (
	"errors"
	"testing"
)

// load supplies the control-plane endpoint every deployment needs, so
// individual tests only set what they are actually about.
func load(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	full := map[string]string{
		"AUTHN_TOKENREVIEW_API_URL": "https://control-plane.test",
		"AUTHZ_SAR_API_URL":         "https://control-plane.test",
	}
	for k, v := range env {
		full[k] = v
	}
	return Load(MapGetenv(full))
}

// loadRaw loads exactly the given env, for the tests that assert what the
// loader demands when nothing is supplied.
func loadRaw(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	return Load(MapGetenv(env))
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("port = %d, want %d", cfg.Port, DefaultPort)
	}
	if cfg.Host != "0.0.0.0" {
		t.Errorf("host = %q", cfg.Host)
	}
	if cfg.PublicBaseURL != "http://localhost:7820" {
		t.Errorf("publicBaseURL = %q", cfg.PublicBaseURL)
	}
	if cfg.Model.Mode != ModelModeMock {
		t.Errorf("model mode = %q, want mock (no anthropic key)", cfg.Model.Mode)
	}
	if cfg.Model.AnthropicModel != DefaultAnthropicModel {
		t.Errorf("anthropic model = %q", cfg.Model.AnthropicModel)
	}
}

func TestLoad_PublicBaseURLTrimsTrailingSlash(t *testing.T) {
	cfg, err := load(t, map[string]string{"PUBLIC_BASE_URL": "http://svc.test/"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicBaseURL != "http://svc.test" {
		t.Errorf("publicBaseURL = %q", cfg.PublicBaseURL)
	}
}

func TestLoad_ModelModeDefaultsToAnthropicWhenKeyPresent(t *testing.T) {
	cfg, err := load(t, map[string]string{"ANTHROPIC_API_KEY": "sk-x"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Mode != ModelModeAnthropic {
		t.Errorf("model mode = %q, want anthropic", cfg.Model.Mode)
	}
}

func TestLoad_ExplicitModelModeWins(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"ANTHROPIC_API_KEY": "sk-x", "MODEL_MODE": "mock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Mode != ModelModeMock {
		t.Errorf("explicit MODEL_MODE=mock should win, got %q", cfg.Model.Mode)
	}
}

func TestLoad_CapabilityDocsFixtureRename(t *testing.T) {
	cfg, err := load(t, map[string]string{"CAPABILITY_DOCS_FIXTURE": "/tmp/caps.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CapabilityDocsFixture != "/tmp/caps.json" {
		t.Errorf("capability docs fixture = %q", cfg.CapabilityDocsFixture)
	}
}

func TestLoad_CapabilityProviderURL(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"CAPABILITY_PROVIDER_URL": "http://capability-adapter/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CapabilityProviderURL != "http://capability-adapter" {
		t.Errorf("capability provider url = %q (trailing slash not trimmed?)", cfg.CapabilityProviderURL)
	}
	if cfg.CapabilityDocsFixture != "" {
		t.Errorf("fixture should be empty, got %q", cfg.CapabilityDocsFixture)
	}
}

func TestLoad_PersonaPromptFile(t *testing.T) {
	cfg, err := load(t, map[string]string{"PERSONA_PROMPT_FILE": "/config/persona.md"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PersonaPromptFile != "/config/persona.md" {
		t.Errorf("persona prompt file = %q", cfg.PersonaPromptFile)
	}
}

func TestLoad_PersonaPromptFileDefaultsEmpty(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PersonaPromptFile != "" {
		t.Errorf("persona prompt file = %q, want empty", cfg.PersonaPromptFile)
	}
}

func TestLoad_CapabilitySourcesMutuallyExclusive(t *testing.T) {
	_, err := load(t, map[string]string{
		"CAPABILITY_DOCS_FIXTURE": "/tmp/caps.json",
		"CAPABILITY_PROVIDER_URL": "http://capability-adapter",
	})
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("want *config.Error, got %v", err)
	}
	found := false
	for _, fe := range cfgErr.Errors {
		if fe.Field == "CAPABILITY_PROVIDER_URL" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a CAPABILITY_PROVIDER_URL mutual-exclusion error, got %+v", cfgErr.Errors)
	}
}

func TestLoad_GatewayTLSInsecureTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes"} {
		cfg, err := load(t, map[string]string{
			"MODEL_MODE":  "gateway",
			"GATEWAY_URL": "https://gw", "GATEWAY_TLS_INSECURE": v,
		})
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if !cfg.Model.GatewayTLSInsecure {
			t.Errorf("GATEWAY_TLS_INSECURE=%q should be truthy", v)
		}
	}
}

func TestLoad_Errors(t *testing.T) {
	cases := map[string]map[string]string{
		"anthropic mode requires key": {"MODEL_MODE": "anthropic"},
		"gateway mode requires url":   {"MODEL_MODE": "gateway"},
		"invalid model mode":          {"MODEL_MODE": "bogus"},
		"invalid port":                {"PORT": "99999"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, env)
			var cfgErr *Error
			if !errors.As(err, &cfgErr) {
				t.Fatalf("want *config.Error, got %v", err)
			}
			if len(cfgErr.Errors) == 0 {
				t.Error("expected at least one field error")
			}
		})
	}
}

// ── production-posture invariants (G) ──────────────────────────

// hasFieldError reports whether err is a *config.Error carrying a FieldError for
// field.
func hasFieldError(t *testing.T, err error, field string) bool {
	t.Helper()
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("want *config.Error, got %v", err)
	}
	for _, fe := range cfgErr.Errors {
		if fe.Field == field {
			return true
		}
	}
	return false
}

func TestLoad_RefusesPlaintextExternalGateway(t *testing.T) {
	_, err := load(t, map[string]string{
		"MODEL_MODE":  "gateway",
		"GATEWAY_URL": "http://gateway.public.example.com/v1",
	})
	if !hasFieldError(t, err, "GATEWAY_URL") {
		t.Fatalf("plaintext external gateway must be refused, got %v", err)
	}
}

func TestLoad_AllowsPlaintextInternalGateway(t *testing.T) {
	// Both the deployed (ClusterIP DNS) and e2e (loopback) gateway postures.
	for _, gw := range []string{
		"http://patch-ai-gateway.envoy-gateway-system.svc.cluster.local:80/v1",
		"http://localhost:1975/v1",
		"http://10.0.0.5:80/v1",
	} {
		cfg, err := load(t, map[string]string{
			"MODEL_MODE": "gateway", "GATEWAY_URL": gw,
		})
		if err != nil {
			t.Fatalf("internal gateway %q must boot: %v", gw, err)
		}
		if cfg.Model.GatewayURL != gw {
			t.Errorf("gateway url = %q", cfg.Model.GatewayURL)
		}
	}
}

// ── control-plane endpoints ────────────────────────────────────

// In-cluster, a pod needs to configure nothing: both endpoints derive from the
// service env the kubelet injects.
func TestLoad_DerivesInClusterEndpoints(t *testing.T) {
	cfg, err := loadRaw(t, map[string]string{
		"KUBERNETES_SERVICE_HOST": "10.96.0.1", "KUBERNETES_SERVICE_PORT": "443",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.SARAPIURL != "https://10.96.0.1:443" {
		t.Errorf("SAR api url = %q, want derived https endpoint", cfg.Auth.SARAPIURL)
	}
	if cfg.Auth.TokenReviewAPIURL != "https://10.96.0.1:443" {
		t.Errorf("TokenReview api url = %q, want derived https endpoint", cfg.Auth.TokenReviewAPIURL)
	}
	if cfg.Auth.SARTokenPath != defaultSARTokenPath {
		t.Errorf("SAR token path = %q", cfg.Auth.SARTokenPath)
	}
}

// With no explicit endpoint and no in-cluster env there is nothing to fall back
// to — the service cannot decide identity or access, so it must refuse to boot
// rather than start in a state where every request is undecidable.
func TestLoad_RequiresControlPlaneEndpoint(t *testing.T) {
	_, err := loadRaw(t, map[string]string{})
	if !hasFieldError(t, err, "AUTHZ_SAR_API_URL") {
		t.Errorf("missing SAR endpoint must be refused, got %v", err)
	}
	if !hasFieldError(t, err, "AUTHN_TOKENREVIEW_API_URL") {
		t.Errorf("missing TokenReview endpoint must be refused, got %v", err)
	}
}

func TestLoad_ExplicitSAREndpoint(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"AUTHZ_SAR_API_URL": "https://kubernetes.default.svc/",
		"AUTHZ_SAR_VERB":    "get",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.SARAPIURL != "https://kubernetes.default.svc" {
		t.Errorf("SAR api url = %q (trailing slash not trimmed?)", cfg.Auth.SARAPIURL)
	}
	if cfg.Auth.SARVerb != "get" {
		t.Errorf("SAR verb = %q", cfg.Auth.SARVerb)
	}
}

func TestLoad_CapabilityIdentityForwardHosts(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"CAPABILITY_IDENTITY_FORWARD_HOSTS": " gateway.svc.cluster.local , ,mcp.datum.test ",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"gateway.svc.cluster.local", "mcp.datum.test"}
	if len(cfg.CapabilityIdentityForwardHosts) != len(want) {
		t.Fatalf("hosts = %v, want %v", cfg.CapabilityIdentityForwardHosts, want)
	}
	for i, h := range want {
		if cfg.CapabilityIdentityForwardHosts[i] != h {
			t.Errorf("hosts[%d] = %q, want %q", i, cfg.CapabilityIdentityForwardHosts[i], h)
		}
	}
}

// Unset forwards the caller's credential to nobody — the safe default for a
// list that decides who may receive it.
func TestLoad_CapabilityIdentityForwardHostsDefaultEmpty(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.CapabilityIdentityForwardHosts) != 0 {
		t.Fatalf("hosts = %v, want none", cfg.CapabilityIdentityForwardHosts)
	}
}

// ── platform API (the base tools' read/write path) ─────────────

// The platform that decides whether a caller may act on a project is the same
// one that serves that project's resources, so a deployment that named one has
// already named the other and configures nothing extra.
func TestLoad_PlatformAPIDefaultsToTheAuthorizationEndpoint(t *testing.T) {
	cfg, err := load(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlatformAPIURL != cfg.Auth.SARAPIURL {
		t.Errorf("platform api url = %q, want the SAR endpoint %q", cfg.PlatformAPIURL, cfg.Auth.SARAPIURL)
	}
	if cfg.PlatformAPICACertPath != cfg.Auth.SARCACertPath {
		t.Errorf("platform api CA = %q, want the SAR CA %q", cfg.PlatformAPICACertPath, cfg.Auth.SARCACertPath)
	}
}

func TestLoad_PlatformAPICanBeNamedSeparately(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"PLATFORM_API_URL":          "https://api.datum.test/",
		"PLATFORM_API_CA_CERT_PATH": "/etc/platform/ca.crt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlatformAPIURL != "https://api.datum.test" {
		t.Errorf("platform api url = %q, want the trailing slash trimmed", cfg.PlatformAPIURL)
	}
	if cfg.PlatformAPICACertPath != "/etc/platform/ca.crt" {
		t.Errorf("platform api CA = %q", cfg.PlatformAPICACertPath)
	}
}
