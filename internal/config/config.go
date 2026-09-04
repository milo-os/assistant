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
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Standard in-cluster service-account mount paths: the default token/CA source
// for the assistant's own identity on the control-plane SubjectAccessReview and
// TokenReview calls, when no explicit path is set.
const (
	defaultSARTokenPath  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultSARCACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
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
	DefaultPort           = 7820
	DefaultAnthropicModel = "claude-sonnet-4-6"
	DefaultGatewayModel   = "patch-stub-v1"
	defaultHost           = "0.0.0.0"
)

// AuthConfig holds the authentication and authorization settings.
type AuthConfig struct {
	// SARAPIURL is the control-plane API base URL the SubjectAccessReview is
	// POSTed to (env AUTHZ_SAR_API_URL). When unset it is derived from the
	// in-cluster KUBERNETES_SERVICE_HOST/PORT.
	SARAPIURL string
	// SARGroup/Resource/Verb override the resourceAttributes triple the SAR asks
	// about (envs AUTHZ_SAR_GROUP/RESOURCE/VERB). Empty ⇒ the auth package
	// defaults (assistant.miloapis.com / conversations / create).
	SARGroup    string
	SARResource string
	SARVerb     string
	// SARTokenPath/SARCACertPath point at the assistant's own service-account
	// token and CA bundle for the SAR call (envs AUTHZ_SAR_TOKEN_PATH /
	// AUTHZ_SAR_CA_CERT_PATH). Default to the standard in-cluster mount paths.
	SARTokenPath  string
	SARCACertPath string

	// TokenReviewAPIURL is the control-plane API base URL the TokenReview is
	// POSTed to (env AUTHN_TOKENREVIEW_API_URL). When unset it is derived from
	// the in-cluster KUBERNETES_SERVICE_HOST/PORT.
	TokenReviewAPIURL string
	// TokenReviewTokenPath/TokenReviewCACertPath point at the assistant's own
	// service-account token and CA bundle for the TokenReview call (envs
	// AUTHN_TOKENREVIEW_TOKEN_PATH / AUTHN_TOKENREVIEW_CA_CERT_PATH). Default to
	// the standard in-cluster mount paths.
	TokenReviewTokenPath  string
	TokenReviewCACertPath string
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
	// (env CAPABILITY_DOCS_FIXTURE). Empty ⇒ no fixture source.
	CapabilityDocsFixture string

	// CapabilityProviderURL is the base URL of the capability-provider HTTP API
	// (env CAPABILITY_PROVIDER_URL). Empty ⇒ no HTTP source. Mutually exclusive
	// with CapabilityDocsFixture. With both unset, no provider capabilities are
	// composed.
	CapabilityProviderURL string

	// PersonaPromptFile is the path to a file containing the persona section
	// of the system prompt (env PERSONA_PROMPT_FILE), read once at startup.
	// Empty ⇒ agent.DefaultPersona. A platform provider sets this to mount
	// their own identity/voice text via a ConfigMap without a rebuild; the
	// tool-use and provenance rules stay fixed regardless (see
	// internal/agent.BuildSystemPrompt).
	PersonaPromptFile string

	// ConversationStoreURL is the PostgreSQL URL for durable conversation
	// history (env CONVERSATION_STORE_URL). Empty ⇒ in-memory history
	// (process lifetime). When set, an unreachable database fails boot —
	// a service configured for durable history must not silently forget.
	ConversationStoreURL string

	// AllowPrivateCapabilityNetworks relaxes the capability SSRF guard's
	// loopback/RFC1918 block (env CAPABILITY_ALLOW_PRIVATE_NETWORKS). The
	// platform's real capability endpoints — the in-cluster AI gateway, provider
	// pods — resolve to private ClusterIPs, so every real deployment sets this
	// true; local dev/e2e reach services over loopback and need it too. Even
	// when true, link-local/cloud-metadata addresses stay blocked. Set false
	// only in a posture where all capability endpoints are public AND providers
	// are untrusted (then prefer a host allow-list). Default false = safe.
	AllowPrivateCapabilityNetworks bool

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
	// Identity and project access are both answered by the control plane — a
	// TokenReview for who you are, a SubjectAccessReview for what you may
	// reach. There is no local mode to fall back to, so the endpoint is
	// required; in-cluster it is derived from the injected service env, and a
	// deployment off-cluster must name it explicitly.
	controlPlaneURL := func(explicit, field string) string {
		if u := strings.TrimRight(explicit, "/"); u != "" {
			return u
		}
		if host := env("KUBERNETES_SERVICE_HOST"); host != "" {
			port := env("KUBERNETES_SERVICE_PORT")
			if port == "" {
				port = "443"
			}
			return "https://" + net.JoinHostPort(host, port)
		}
		errs = append(errs, FieldError{field,
			field + " is required (no in-cluster KUBERNETES_SERVICE_HOST to derive the control-plane endpoint from)"})
		return ""
	}

	tokenReviewAPIURL := controlPlaneURL(env("AUTHN_TOKENREVIEW_API_URL"), "AUTHN_TOKENREVIEW_API_URL")
	tokenReviewTokenPath := env("AUTHN_TOKENREVIEW_TOKEN_PATH")
	if tokenReviewTokenPath == "" {
		tokenReviewTokenPath = defaultSARTokenPath
	}
	tokenReviewCACertPath := env("AUTHN_TOKENREVIEW_CA_CERT_PATH")
	if tokenReviewCACertPath == "" {
		tokenReviewCACertPath = defaultSARCACertPath
	}

	sarAPIURL := controlPlaneURL(env("AUTHZ_SAR_API_URL"), "AUTHZ_SAR_API_URL")
	sarTokenPath := env("AUTHZ_SAR_TOKEN_PATH")
	if sarTokenPath == "" {
		sarTokenPath = defaultSARTokenPath
	}
	sarCACertPath := env("AUTHZ_SAR_CA_CERT_PATH")
	if sarCACertPath == "" {
		sarCACertPath = defaultSARCACertPath
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

	// ── Capability source ─────────────────────────────────────
	// The fixture (local file) and HTTP (provider API) sources are mutually
	// exclusive: they answer the same seam, so configuring both is ambiguous.
	capabilityDocsFixture := env("CAPABILITY_DOCS_FIXTURE")
	capabilityProviderURL := strings.TrimRight(env("CAPABILITY_PROVIDER_URL"), "/")
	if capabilityDocsFixture != "" && capabilityProviderURL != "" {
		errs = append(errs, FieldError{"CAPABILITY_PROVIDER_URL",
			"CAPABILITY_PROVIDER_URL and CAPABILITY_DOCS_FIXTURE are mutually exclusive — set at most one capability source"})
	}

	conversationStoreURL := env("CONVERSATION_STORE_URL")
	if conversationStoreURL != "" &&
		!strings.HasPrefix(conversationStoreURL, "postgres://") &&
		!strings.HasPrefix(conversationStoreURL, "postgresql://") {
		errs = append(errs, FieldError{"CONVERSATION_STORE_URL",
			"must be a postgres:// or postgresql:// URL (or empty for in-memory history)"})
	}

	// ── Production-posture invariants ─────────────────────────
	// Refuse to boot on a configuration that silently runs dev-grade security on
	// an internet-facing deployment. These are narrow enough that every existing
	// dev/e2e config (loopback / plaintext-http / .test hosts) still boots; they
	// only fire on the specific unsafe combinations below.
	errs = append(errs, productionInvariants(publicBaseURL, gatewayURL, modelMode)...)

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
			SARAPIURL:     sarAPIURL,
			SARGroup:      env("AUTHZ_SAR_GROUP"),
			SARResource:   env("AUTHZ_SAR_RESOURCE"),
			SARVerb:       env("AUTHZ_SAR_VERB"),
			SARTokenPath:  sarTokenPath,
			SARCACertPath: sarCACertPath,

			TokenReviewAPIURL:     tokenReviewAPIURL,
			TokenReviewTokenPath:  tokenReviewTokenPath,
			TokenReviewCACertPath: tokenReviewCACertPath,
		},
		CapabilityDocsFixture:          capabilityDocsFixture,
		CapabilityProviderURL:          capabilityProviderURL,
		PersonaPromptFile:              env("PERSONA_PROMPT_FILE"),
		ConversationStoreURL:           conversationStoreURL,
		AllowPrivateCapabilityNetworks: isTruthy(env("CAPABILITY_ALLOW_PRIVATE_NETWORKS")),
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

// productionInvariants enforces the "refuse to boot on an unsafe production
// posture" rules. It no longer needs to police credential handling: identity
// and access are decided by the control plane in every deployment, so there is
// no dev-grade auth setting left to leak into production. What remains targets
// transport exposure, and none of it fires for a loopback/internal-host
// deployment, so the dev and e2e configs still boot.
func productionInvariants(publicBaseURL, gatewayURL string, modelMode ModelMode) []FieldError {
	var errs []FieldError

	// Plaintext model gateway to an external host. Gateway mode carries the
	//    prompt (and the gateway injects the upstream key), so a plaintext http
	//    hop to a host outside the cluster/loopback exposes it in transit.
	//    In-cluster (ClusterIP / *.svc.cluster.local) and loopback gateways over
	//    http are the intended dev/deployed posture and stay allowed.
	if modelMode == ModelModeGateway && schemeOf(gatewayURL) == "http" && !isInternalHost(hostOf(gatewayURL)) {
		errs = append(errs, FieldError{"GATEWAY_URL",
			"MODEL_MODE=gateway over plaintext http:// to an external host exposes prompts in transit; use https or an in-cluster/loopback endpoint"})
	}

	return errs
}

// schemeOf returns the lowercased URL scheme, or "" when unparseable.
func schemeOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme)
}

// hostOf returns the hostname (no port) of rawURL, or "".
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// isInternalHost reports whether host is loopback, a private/link-local IP, or a
// cluster-internal DNS name (a single label, or an internal suffix like
// .svc.cluster.local / .internal / .local). Public IP literals and registered
// public domains are NOT internal. An empty host is treated as internal (there
// is nothing external to protect).
func isInternalHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
	}
	// A bare single-label name (e.g. a Kubernetes Service name) is in-cluster.
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".svc.cluster.local", ".svc", ".cluster.local", ".internal", ".local"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
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
