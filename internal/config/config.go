// Package config is the single place that reads the environment. Everything
// downstream takes the parsed [Config] by injection, so no other package
// touches os.Getenv — that keeps the agent loop, auth, and usage emitter
// harness-drivable.
//
// It is a direct port of the TypeScript service's src/config.ts. The env var
// names are identical to the TS service with one deliberate exception noted in
// [Load]: AGENT_BINDINGS_FIXTURE is renamed CAPABILITY_DOCS_FIXTURE to reflect
// the capability-contract inversion (the assistant now owns its capability
// document schema).
package config

import (
	"fmt"
	"strconv"
	"strings"
)

// AuthMode selects the request authenticator.
type AuthMode string

const (
	// AuthModeDev uses static bearer tokens from AUTH_DEV_TOKENS.
	AuthModeDev AuthMode = "dev"
	// AuthModeOIDC verifies bearer JWTs against an OIDC issuer's JWKS.
	AuthModeOIDC AuthMode = "oidc"
)

// ModelMode selects the model backend.
type ModelMode string

const (
	// ModelModeAnthropic talks to the real Anthropic API (needs ANTHROPIC_API_KEY).
	ModelModeAnthropic ModelMode = "anthropic"
	// ModelModeMock uses the in-process scripted model (no credentials).
	ModelModeMock ModelMode = "mock"
	// ModelModeGateway talks to an Envoy AI Gateway (OpenAI-compatible; the
	// gateway injects the upstream credential, so the service holds none).
	ModelModeGateway ModelMode = "gateway"
)

// Defaults mirrored from the TS service.
const (
	DefaultPort          = 7820
	DefaultAnthropicModel = "claude-sonnet-4-6"
	DefaultGatewayModel  = "patch-stub-v1"
	defaultHost          = "0.0.0.0"
	defaultProjectsClaim = "projects"
)

// AuthConfig holds the authentication and authorization settings.
type AuthConfig struct {
	Mode AuthMode
	// DevTokens is the raw AUTH_DEV_TOKENS string, parsed by the dev authenticator.
	DevTokens string
	OIDCIssuer   string
	OIDCAudience string
	// OIDCProjectsClaim is the JWT claim carrying the granted project names.
	OIDCProjectsClaim string
}

// ModelConfig holds the model-backend settings.
type ModelConfig struct {
	Mode            ModelMode
	AnthropicAPIKey string
	AnthropicModel  string
	// GatewayURL is the Envoy AI Gateway base URL (OpenAI-compatible endpoint).
	// Distinct from [UsageConfig.GatewayURL] (the metering collector).
	GatewayURL string
	// GatewayModel is the model name the gateway routes upstream.
	GatewayModel string
	// GatewayCACert is an optional CA PEM path for a self-signed gateway TLS cert.
	GatewayCACert string
	// GatewayTLSInsecure skips gateway TLS verification (local convenience only).
	GatewayTLSInsecure bool
}

// UsageConfig holds the usage-metering collector settings.
type UsageConfig struct {
	// GatewayURL is the collector base URL (USAGE_GATEWAY_URL). Unset ⇒ emit is a no-op.
	GatewayURL string
	// GatewayAPIKey is an optional collector api-key (USAGE_GATEWAY_API_KEY).
	GatewayAPIKey string
}

// Config is the fully-parsed service configuration.
type Config struct {
	Port int
	Host string
	// PublicBaseURL is used for the agent-card url and the CloudEvents source.
	PublicBaseURL string
	LogLevel      string

	Auth AuthConfig

	// CapabilityDocsFixture is the path to the capability-documents fixture
	// (env CAPABILITY_DOCS_FIXTURE). Empty ⇒ no provider capabilities composed.
	CapabilityDocsFixture string

	Model ModelConfig
	Usage UsageConfig
}

// FieldError describes a single invalid configuration field.
type FieldError struct {
	Field   string
	Message string
}

// Error aggregates one or more [FieldError]s from [Load].
type Error struct {
	Errors []FieldError
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("Invalid configuration:")
	for _, fe := range e.Errors {
		fmt.Fprintf(&b, "\n  - %s: %s", fe.Field, fe.Message)
	}
	return b.String()
}

// Load parses a [Config] from the provided environment lookup. Pass os.Getenv
// (wrapped) in production; tests pass a map-backed lookup. It returns an
// [*Error] aggregating every invalid field.
//
// Env var names are identical to the TS service EXCEPT AGENT_BINDINGS_FIXTURE,
// which is renamed CAPABILITY_DOCS_FIXTURE (capability-contract inversion).
func Load(getenv func(string) string) (*Config, error) {
	env := func(k string) string { return strings.TrimSpace(getenv(k)) }
	var errs []FieldError

	port := DefaultPort
	if raw := env("PORT"); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p <= 0 || p > 65535 {
			errs = append(errs, FieldError{"PORT", fmt.Sprintf("must be a valid TCP port, got %q", getenv("PORT"))})
		} else {
			port = p
		}
	}

	host := env("HOST")
	if host == "" {
		host = defaultHost
	}

	publicBaseURL := env("PUBLIC_BASE_URL")
	if publicBaseURL == "" {
		publicBaseURL = fmt.Sprintf("http://localhost:%d", port)
	}
	publicBaseURL = strings.TrimRight(publicBaseURL, "/")

	logLevel := oneOf(env("LOG_LEVEL"), []string{"debug", "info", "warn", "error"}, "info")

	// ── Auth ──────────────────────────────────────────────────
	authMode := AuthMode(oneOf(env("AUTH_MODE"), []string{"dev", "oidc"}, "dev"))
	if authMode == AuthModeDev && env("AUTH_DEV_TOKENS") == "" {
		errs = append(errs, FieldError{"AUTH_DEV_TOKENS",
			`AUTH_MODE=dev requires at least one token (format "token:subject:projA,projB;...")`})
	}
	if authMode == AuthModeOIDC {
		if env("OIDC_ISSUER") == "" {
			errs = append(errs, FieldError{"OIDC_ISSUER", "AUTH_MODE=oidc requires OIDC_ISSUER"})
		}
		if env("OIDC_AUDIENCE") == "" {
			errs = append(errs, FieldError{"OIDC_AUDIENCE", "AUTH_MODE=oidc requires OIDC_AUDIENCE"})
		}
	}
	projectsClaim := env("OIDC_PROJECTS_CLAIM")
	if projectsClaim == "" {
		projectsClaim = defaultProjectsClaim
	}

	// ── Model ─────────────────────────────────────────────────
	anthropicKey := env("ANTHROPIC_API_KEY")
	gatewayURL := env("GATEWAY_URL")
	// Default: anthropic when a key is present, else mock. gateway is only
	// selected explicitly. An explicit MODEL_MODE always wins.
	var modelMode ModelMode
	switch raw := env("MODEL_MODE"); raw {
	case string(ModelModeAnthropic), string(ModelModeMock), string(ModelModeGateway):
		modelMode = ModelMode(raw)
	case "":
		if anthropicKey != "" {
			modelMode = ModelModeAnthropic
		} else {
			modelMode = ModelModeMock
		}
	default:
		errs = append(errs, FieldError{"MODEL_MODE",
			fmt.Sprintf(`must be "anthropic", "mock", or "gateway", got %q`, raw)})
		modelMode = ModelModeMock
	}
	if modelMode == ModelModeAnthropic && anthropicKey == "" {
		errs = append(errs, FieldError{"ANTHROPIC_API_KEY", "MODEL_MODE=anthropic requires ANTHROPIC_API_KEY"})
	}
	if modelMode == ModelModeGateway && gatewayURL == "" {
		errs = append(errs, FieldError{"GATEWAY_URL", "MODEL_MODE=gateway requires GATEWAY_URL (the Envoy AI Gateway endpoint)"})
	}

	if len(errs) > 0 {
		return nil, &Error{Errors: errs}
	}

	anthropicModel := env("ANTHROPIC_MODEL")
	if anthropicModel == "" {
		anthropicModel = DefaultAnthropicModel
	}
	gatewayModel := env("GATEWAY_MODEL")
	if gatewayModel == "" {
		gatewayModel = DefaultGatewayModel
	}

	return &Config{
		Port:          port,
		Host:          host,
		PublicBaseURL: publicBaseURL,
		LogLevel:      logLevel,
		Auth: AuthConfig{
			Mode:              authMode,
			DevTokens:         env("AUTH_DEV_TOKENS"),
			OIDCIssuer:        env("OIDC_ISSUER"),
			OIDCAudience:      env("OIDC_AUDIENCE"),
			OIDCProjectsClaim: projectsClaim,
		},
		CapabilityDocsFixture: env("CAPABILITY_DOCS_FIXTURE"),
		Model: ModelConfig{
			Mode:               modelMode,
			AnthropicAPIKey:    anthropicKey,
			AnthropicModel:     anthropicModel,
			GatewayURL:         gatewayURL,
			GatewayModel:       gatewayModel,
			GatewayCACert:      env("GATEWAY_CA_CERT"),
			GatewayTLSInsecure: isTruthy(env("GATEWAY_TLS_INSECURE")),
		},
		Usage: UsageConfig{
			GatewayURL:    env("USAGE_GATEWAY_URL"),
			GatewayAPIKey: env("USAGE_GATEWAY_API_KEY"),
		},
	}, nil
}

// MapGetenv adapts a map to the getenv function [Load] expects (test helper).
func MapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func oneOf(value string, allowed []string, fallback string) string {
	for _, a := range allowed {
		if value == a {
			return value
		}
	}
	return fallback
}

func isTruthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
