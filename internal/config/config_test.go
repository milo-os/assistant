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
		"dev mode requires tokens":         {"AUTH_MODE": "dev"},
		"oidc requires issuer + audience":  {"AUTH_MODE": "oidc"},
		"anthropic mode requires key":      {"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "anthropic"},
		"gateway mode requires url":        {"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "gateway"},
		"invalid model mode":               {"AUTH_DEV_TOKENS": "t:s:*", "MODEL_MODE": "bogus"},
		"invalid port":                     {"AUTH_DEV_TOKENS": "t:s:*", "PORT": "99999"},
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
