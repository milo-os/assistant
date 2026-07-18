package config

import (
	"errors"
	"testing"
)

func load(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	return Load(MapGetenv(env))
}

func TestLoad_DevDefaults(t *testing.T) {
	cfg, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*"})
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
	if cfg.Auth.Mode != AuthModeDev {
		t.Errorf("auth mode = %q", cfg.Auth.Mode)
	}
	if cfg.Model.Mode != ModelModeMock {
		t.Errorf("model mode = %q, want mock (no anthropic key)", cfg.Model.Mode)
	}
	if cfg.Model.AnthropicModel != DefaultAnthropicModel {
		t.Errorf("anthropic model = %q", cfg.Model.AnthropicModel)
	}
}

func TestLoad_PublicBaseURLTrimsTrailingSlash(t *testing.T) {
	cfg, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*", "PUBLIC_BASE_URL": "http://svc.test/"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicBaseURL != "http://svc.test" {
		t.Errorf("publicBaseURL = %q", cfg.PublicBaseURL)
	}
}

func TestLoad_ModelModeDefaultsToAnthropicWhenKeyPresent(t *testing.T) {
	cfg, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*", "ANTHROPIC_API_KEY": "sk-x"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Mode != ModelModeAnthropic {
		t.Errorf("model mode = %q, want anthropic", cfg.Model.Mode)
	}
}

func TestLoad_ExplicitModelModeWins(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"AUTH_DEV_TOKENS": "t:s:*", "ANTHROPIC_API_KEY": "sk-x", "MODEL_MODE": "mock",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Mode != ModelModeMock {
		t.Errorf("explicit MODEL_MODE=mock should win, got %q", cfg.Model.Mode)
	}
}

func TestLoad_CapabilityDocsFixtureRename(t *testing.T) {
	cfg, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*", "CAPABILITY_DOCS_FIXTURE": "/tmp/caps.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CapabilityDocsFixture != "/tmp/caps.json" {
		t.Errorf("capability docs fixture = %q", cfg.CapabilityDocsFixture)
	}
}

func TestLoad_CapabilityProviderURL(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"AUTH_DEV_TOKENS":         "t:s:*",
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
	cfg, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*", "PERSONA_PROMPT_FILE": "/config/persona.md"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PersonaPromptFile != "/config/persona.md" {
		t.Errorf("persona prompt file = %q", cfg.PersonaPromptFile)
	}
}

func TestLoad_PersonaPromptFileDefaultsEmpty(t *testing.T) {
	cfg, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PersonaPromptFile != "" {
		t.Errorf("persona prompt file = %q, want empty", cfg.PersonaPromptFile)
	}
}

func TestLoad_CapabilitySourcesMutuallyExclusive(t *testing.T) {
	_, err := load(t, map[string]string{
		"AUTH_DEV_TOKENS":         "t:s:*",
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
			"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "gateway",
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
		"dev mode requires tokens":        {"AUTH_MODE": "dev"},
		"oidc requires issuer + audience": {"AUTH_MODE": "oidc"},
		"anthropic mode requires key":     {"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "anthropic"},
		"gateway mode requires url":       {"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "gateway"},
		"invalid model mode":              {"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "bogus"},
		"invalid port":                    {"AUTH_DEV_TOKENS": "t:s:*", "PORT": "99999"},
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

func TestLoad_RefusesDevTokensOnHTTPSPublicURL(t *testing.T) {
	_, err := load(t, map[string]string{
		"AUTH_MODE": "dev", "AUTH_DEV_TOKENS": "t:s:*",
		"PUBLIC_BASE_URL": "https://patch.example.com",
	})
	if !hasFieldError(t, err, "AUTH_MODE") {
		t.Fatalf("dev tokens + https public url must be refused, got %v", err)
	}
}

func TestLoad_AllowsDevTokensOnHTTPPublicURL(t *testing.T) {
	// The existing dev/e2e posture: plaintext http, non-loopback .test host.
	cfg, err := load(t, map[string]string{
		"AUTH_MODE": "dev", "AUTH_DEV_TOKENS": "t:s:*",
		"PUBLIC_BASE_URL": "http://assistant.test",
	})
	if err != nil {
		t.Fatalf("http dev posture must still boot: %v", err)
	}
	if cfg.PublicBaseURL != "http://assistant.test" {
		t.Errorf("publicBaseURL = %q", cfg.PublicBaseURL)
	}
}

func TestLoad_RefusesDevTokensInOIDCMode(t *testing.T) {
	_, err := load(t, map[string]string{
		"AUTH_MODE": "oidc", "OIDC_ISSUER": "https://issuer", "OIDC_AUDIENCE": "aud",
		"AUTH_DEV_TOKENS": "leftover:s:*",
	})
	if !hasFieldError(t, err, "AUTH_DEV_TOKENS") {
		t.Fatalf("stray AUTH_DEV_TOKENS in oidc mode must be refused, got %v", err)
	}
}

func TestLoad_RefusesPlaintextExternalGateway(t *testing.T) {
	_, err := load(t, map[string]string{
		"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "gateway",
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
			"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "gateway", "GATEWAY_URL": gw,
		})
		if err != nil {
			t.Fatalf("internal gateway %q must boot: %v", gw, err)
		}
		if cfg.Model.GatewayURL != gw {
			t.Errorf("gateway url = %q", cfg.Model.GatewayURL)
		}
	}
}

// ── authorization mode (AUTHZ_MODE) ────────────────────────────

func TestLoad_AuthzModeDefaultsToClaims(t *testing.T) {
	cfg, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.AuthzMode != AuthzModeClaims {
		t.Errorf("authz mode = %q, want claims", cfg.Auth.AuthzMode)
	}
}

func TestLoad_AuthzSARDerivesInClusterEndpoint(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"AUTH_DEV_TOKENS": "t:s:*", "AUTHZ_MODE": "sar",
		"KUBERNETES_SERVICE_HOST": "10.96.0.1", "KUBERNETES_SERVICE_PORT": "443",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.AuthzMode != AuthzModeSAR {
		t.Errorf("authz mode = %q, want sar", cfg.Auth.AuthzMode)
	}
	if cfg.Auth.SARAPIURL != "https://10.96.0.1:443" {
		t.Errorf("SAR api url = %q, want derived https endpoint", cfg.Auth.SARAPIURL)
	}
	if cfg.Auth.SARTokenPath != defaultSARTokenPath {
		t.Errorf("SAR token path = %q", cfg.Auth.SARTokenPath)
	}
}

func TestLoad_AuthzSARRequiresEndpoint(t *testing.T) {
	// sar mode with no explicit endpoint and no in-cluster env must refuse.
	_, err := load(t, map[string]string{"AUTH_DEV_TOKENS": "t:s:*", "AUTHZ_MODE": "sar"})
	if !hasFieldError(t, err, "AUTHZ_SAR_API_URL") {
		t.Fatalf("sar mode without an endpoint must be refused, got %v", err)
	}
}

func TestLoad_AuthzSARExplicitEndpoint(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"AUTH_DEV_TOKENS": "t:s:*", "AUTHZ_MODE": "sar",
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

func TestLoad_OIDCValid(t *testing.T) {
	cfg, err := load(t, map[string]string{
		"AUTH_MODE": "oidc", "OIDC_ISSUER": "https://issuer", "OIDC_AUDIENCE": "aud",
		"OIDC_PROJECTS_CLAIM": "groups",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Mode != AuthModeOIDC || cfg.Auth.OIDCProjectsClaim != "groups" {
		t.Errorf("auth = %+v", cfg.Auth)
	}
}
