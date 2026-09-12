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

// CapabilitySourceMode selects which [capability.Source] implementation serves
// the assistant's capability configuration.
//
// It is an explicit enum rather than "whichever companion variable happens to
// be set" because there are now three modes and the pairwise
// mutual-exclusion check that served two does not extend: with N modes it is
// N*(N-1)/2 checks and an error message that names two variables out of
// several. An operator should declare the mode and be told precisely which
// companion value that mode needs.
type CapabilitySourceMode string

const (
	// CapabilitySourceNone composes no provider capabilities at all — the
	// built-ins-only assistant. This is what an unconfigured deployment gets,
	// and it is a supported posture (the README's standalone promise), not an
	// error.
	CapabilitySourceNone CapabilitySourceMode = ""
	// CapabilitySourceFixture reads documents from a JSON file
	// (CAPABILITY_DOCS_FIXTURE). The standalone/dev/e2e path.
	CapabilitySourceFixture CapabilitySourceMode = "fixture"
	// CapabilitySourceHTTP pulls documents per turn from the catalog's
	// capability-provider API (CAPABILITY_PROVIDER_URL).
	CapabilitySourceHTTP CapabilitySourceMode = "http"
	// CapabilitySourceCRD LISTs CapabilityBinding objects from each project's
	// own Milo control plane, behind a short-TTL cache. It takes NO companion
	// variable: the control-plane URL, CA bundle, and client certificate are the
	// ones the SubjectAccessReview path is already configured with (AUTHZ_SAR_*).
	// Reusing them is deliberate — two ways to name one control plane is two
	// ways to point half the service at the wrong one.
	CapabilitySourceCRD CapabilitySourceMode = "crd"
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
	// SARClientCertPath/SARClientKeyPath point at a client certificate the
	// assistant presents to identify itself for the SAR call (envs
	// AUTHZ_SAR_CLIENT_CERT_PATH / AUTHZ_SAR_CLIENT_KEY_PATH). Unset on a
	// same-cluster control plane, where the service-account token is enough.
	// Required against Milo, which trusts service-account tokens only from its
	// own issuer. No default: an unset path means "no client certificate".
	SARClientCertPath string
	SARClientKeyPath  string

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
	// TokenReviewClientCertPath/TokenReviewClientKeyPath point at a client
	// certificate the assistant presents to identify itself for the TokenReview
	// call (envs AUTHN_TOKENREVIEW_CLIENT_CERT_PATH /
	// AUTHN_TOKENREVIEW_CLIENT_KEY_PATH). No default; see the SAR pair above.
	TokenReviewClientCertPath string
	TokenReviewClientKeyPath  string
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
	// GatewayTokenFile is an optional path to a bearer token presented to the
	// gateway (GATEWAY_TOKEN_FILE). This authenticates the SERVICE to the
	// gateway; it is not a model credential, which the gateway still injects
	// itself. On the Datum platform it is a projected ServiceAccount token with
	// audience "ai-gateway", which the gateway validates as a JWT.
	GatewayTokenFile string
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

	// CapabilitySource selects the capability source implementation
	// (env CAPABILITY_SOURCE = fixture|http|crd). Unset ⇒ inferred for one
	// release from whichever companion variable is set, then
	// [CapabilitySourceNone]. See [CapabilitySourceMode].
	CapabilitySource CapabilitySourceMode

	// CapabilityDocsFixture is the path to the capability-documents fixture
	// (env CAPABILITY_DOCS_FIXTURE). Required by, and only by,
	// CAPABILITY_SOURCE=fixture.
	CapabilityDocsFixture string

	// CapabilityProviderURL is the base URL of the capability-provider HTTP API
	// (env CAPABILITY_PROVIDER_URL). Required by, and only by,
	// CAPABILITY_SOURCE=http.
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

	// PlatformAPIURL is the platform API the base tools read and write a
	// project's own resources through (env PLATFORM_API_URL). Unset defaults to
	// the SubjectAccessReview endpoint: the platform that decides whether a
	// caller may act on a project is the same one that serves that project's
	// resources, so a deployment that names one has already named the other.
	//
	// The service holds no credential for this endpoint. Every request over it
	// carries the calling user's own bearer token (see internal/projectapi),
	// which is why there is no token path here to go with it.
	PlatformAPIURL string

	// PlatformAPICACertPath verifies the platform API's certificate (env
	// PLATFORM_API_CA_CERT_PATH). Unset defaults to the SubjectAccessReview CA
	// bundle, which is the same server.
	PlatformAPICACertPath string

	// PlanTokenKey binds plans for the base tools' change path (env
	// PLAN_TOKEN_KEY; base64 or a raw string of at least 16 bytes). Setting it
	// lets one process apply another's plan and survives restarts. Unset
	// generates a key per process with a startup warning: the guarantee still
	// holds, but an outstanding plan is lost on restart and refused by a
	// sibling replica. See internal/plantoken.
	PlanTokenKey string

	// CapabilityIdentityForwardHosts are the operator-sanctioned MCP endpoint
	// hosts that may receive the calling user's bearer token and the turn's
	// project (comma-separated env; exact or domain-suffix match). Empty (the
	// default) forwards to nobody — a capability document is provider-controlled
	// data, so naming an endpoint must never by itself send a credential there.
	CapabilityIdentityForwardHosts []string

	// CapabilityMCPEndpointHosts are the operator-sanctioned hosts an MCP
	// endpoint may be DIALED at (comma-separated env
	// CAPABILITY_MCP_ENDPOINT_HOSTS; exact or domain-suffix match, the same
	// shape as CapabilityIdentityForwardHosts). In this platform that list is
	// the AI gateway, because every provider tool call is supposed to traverse
	// it — that is where the reviewed tool allow-list is enforced a second time
	// and where the call is metered.
	//
	// Deliberately a separate knob from CapabilityIdentityForwardHosts even
	// though production will hold the same value in both: that one means "may
	// receive the caller's credential", this one "may be dialed at all", and
	// sharing a list means widening the reachable set silently widens the set
	// that gets handed a bearer token. Empty (the default) disables the check,
	// keeping fixtures, e2e, and dev overlays working.
	CapabilityMCPEndpointHosts []string

	Model ModelConfig
	Usage UsageConfig

	// Warnings are non-fatal configuration notes for the caller to log — a
	// deprecated shape that still works today and will not tomorrow. They are
	// returned rather than logged here because this package deliberately owns
	// no logger (it is the one place that reads the environment, and nothing
	// more).
	Warnings []string
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
	var warnings []string

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
	tokenReviewClientCertPath := env("AUTHN_TOKENREVIEW_CLIENT_CERT_PATH")
	tokenReviewClientKeyPath := env("AUTHN_TOKENREVIEW_CLIENT_KEY_PATH")

	sarAPIURL := controlPlaneURL(env("AUTHZ_SAR_API_URL"), "AUTHZ_SAR_API_URL")
	sarTokenPath := env("AUTHZ_SAR_TOKEN_PATH")
	if sarTokenPath == "" {
		sarTokenPath = defaultSARTokenPath
	}
	sarCACertPath := env("AUTHZ_SAR_CA_CERT_PATH")
	if sarCACertPath == "" {
		sarCACertPath = defaultSARCACertPath
	}
	sarClientCertPath := env("AUTHZ_SAR_CLIENT_CERT_PATH")
	sarClientKeyPath := env("AUTHZ_SAR_CLIENT_KEY_PATH")

	// ── Platform API (the base tools' read/write path) ────────
	platformAPIURL := strings.TrimRight(env("PLATFORM_API_URL"), "/")
	if platformAPIURL == "" {
		platformAPIURL = sarAPIURL
	}
	platformAPICACertPath := env("PLATFORM_API_CA_CERT_PATH")
	if platformAPICACertPath == "" {
		platformAPICACertPath = sarCACertPath
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
	// One explicit mode, then per-mode validation. A companion variable set for
	// a mode that does not use it is an ERROR rather than an ignored leftover:
	// it is almost always a half-finished overlay edit, and the alternative is a
	// deployment that silently reads its capabilities from somewhere other than
	// where the operator believes.
	capabilityDocsFixture := env("CAPABILITY_DOCS_FIXTURE")
	capabilityProviderURL := strings.TrimRight(env("CAPABILITY_PROVIDER_URL"), "/")
	capabilitySource, capWarnings, capErrs := capabilitySourceMode(
		env("CAPABILITY_SOURCE"), capabilityDocsFixture, capabilityProviderURL)
	errs = append(errs, capErrs...)
	warnings = append(warnings, capWarnings...)

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
			SARAPIURL:         sarAPIURL,
			SARGroup:          env("AUTHZ_SAR_GROUP"),
			SARResource:       env("AUTHZ_SAR_RESOURCE"),
			SARVerb:           env("AUTHZ_SAR_VERB"),
			SARTokenPath:      sarTokenPath,
			SARCACertPath:     sarCACertPath,
			SARClientCertPath: sarClientCertPath,
			SARClientKeyPath:  sarClientKeyPath,

			TokenReviewAPIURL:         tokenReviewAPIURL,
			TokenReviewTokenPath:      tokenReviewTokenPath,
			TokenReviewCACertPath:     tokenReviewCACertPath,
			TokenReviewClientCertPath: tokenReviewClientCertPath,
			TokenReviewClientKeyPath:  tokenReviewClientKeyPath,
		},
		PlatformAPIURL:                 platformAPIURL,
		PlanTokenKey:                   env("PLAN_TOKEN_KEY"),
		PlatformAPICACertPath:          platformAPICACertPath,
		CapabilitySource:               capabilitySource,
		CapabilityDocsFixture:          capabilityDocsFixture,
		CapabilityProviderURL:          capabilityProviderURL,
		PersonaPromptFile:              env("PERSONA_PROMPT_FILE"),
		ConversationStoreURL:           conversationStoreURL,
		AllowPrivateCapabilityNetworks: isTruthy(env("CAPABILITY_ALLOW_PRIVATE_NETWORKS")),
		CapabilityIdentityForwardHosts: splitList(env("CAPABILITY_IDENTITY_FORWARD_HOSTS")),
		CapabilityMCPEndpointHosts:     splitList(env("CAPABILITY_MCP_ENDPOINT_HOSTS")),
		Warnings:                       warnings,
		Model: ModelConfig{
			Mode:               modelMode,
			AnthropicAPIKey:    anthropicKey,
			AnthropicModel:     anthropicModel,
			GatewayURL:         gatewayURL,
			GatewayModel:       gatewayModel,
			GatewayTokenFile:   env("GATEWAY_TOKEN_FILE"),
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

// splitList parses a comma-separated env value into trimmed, non-empty
// entries. Unset or all-blank yields nil — the safe default for an allow-list.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isTruthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// capabilitySourceMode resolves CAPABILITY_SOURCE and validates the companion
// variables that mode does (and does not) take.
//
// The inference branch is a ONE-RELEASE backward-compatibility shim. Before
// this enum existed the mode was implied by whichever of the two companion
// variables was set, and every overlay plus e2e/run-e2e.sh still reads that
// way; breaking them all in the same commit that adds a third mode would make
// an additive change look like an outage. It warns rather than passing
// silently, because a shim nobody is told about is a shim nobody removes.
// Delete this branch (and the warning) once the overlays declare the mode.
//
// Note what inference deliberately does NOT do: when both companion variables
// are set it refuses rather than picking one. That was an error before and is a
// worse one now, since the operator has not said which they meant.
func capabilitySourceMode(raw, fixture, providerURL string) (CapabilitySourceMode, []string, []FieldError) {
	var warnings []string
	var errs []FieldError

	mode := CapabilitySourceMode(raw)
	switch mode {
	case CapabilitySourceFixture, CapabilitySourceHTTP, CapabilitySourceCRD:
	case CapabilitySourceNone:
		switch {
		case fixture != "" && providerURL != "":
			return CapabilitySourceNone, nil, []FieldError{{"CAPABILITY_SOURCE",
				"CAPABILITY_DOCS_FIXTURE and CAPABILITY_PROVIDER_URL are both set and CAPABILITY_SOURCE is unset — set CAPABILITY_SOURCE to fixture, http, or crd and keep only that mode's companion variable"}}
		case fixture != "":
			mode = CapabilitySourceFixture
			warnings = append(warnings, "CAPABILITY_SOURCE is unset; inferring fixture from CAPABILITY_DOCS_FIXTURE. Set CAPABILITY_SOURCE=fixture explicitly — this inference is removed in the next release.")
		case providerURL != "":
			mode = CapabilitySourceHTTP
			warnings = append(warnings, "CAPABILITY_SOURCE is unset; inferring http from CAPABILITY_PROVIDER_URL. Set CAPABILITY_SOURCE=http explicitly — this inference is removed in the next release.")
		}
		return mode, warnings, nil
	default:
		return CapabilitySourceNone, nil, []FieldError{{"CAPABILITY_SOURCE",
			fmt.Sprintf(`must be "fixture", "http", or "crd" (or unset for no capability source), got %q`, raw)}}
	}

	// Explicit mode: the companion it needs, and nothing it does not.
	switch mode {
	case CapabilitySourceFixture:
		if fixture == "" {
			errs = append(errs, FieldError{"CAPABILITY_DOCS_FIXTURE", "CAPABILITY_SOURCE=fixture requires CAPABILITY_DOCS_FIXTURE (the documents file path)"})
		}
		if providerURL != "" {
			errs = append(errs, FieldError{"CAPABILITY_PROVIDER_URL", "CAPABILITY_SOURCE=fixture does not use CAPABILITY_PROVIDER_URL — unset it or switch to CAPABILITY_SOURCE=http"})
		}
	case CapabilitySourceHTTP:
		if providerURL == "" {
			errs = append(errs, FieldError{"CAPABILITY_PROVIDER_URL", "CAPABILITY_SOURCE=http requires CAPABILITY_PROVIDER_URL (the capability-provider API base URL)"})
		}
		if fixture != "" {
			errs = append(errs, FieldError{"CAPABILITY_DOCS_FIXTURE", "CAPABILITY_SOURCE=http does not use CAPABILITY_DOCS_FIXTURE — unset it or switch to CAPABILITY_SOURCE=fixture"})
		}
	case CapabilitySourceCRD:
		// No companion of its own: the control plane it reads is AUTHZ_SAR_*.
		if fixture != "" {
			errs = append(errs, FieldError{"CAPABILITY_DOCS_FIXTURE", "CAPABILITY_SOURCE=crd does not use CAPABILITY_DOCS_FIXTURE — unset it or switch to CAPABILITY_SOURCE=fixture"})
		}
		if providerURL != "" {
			errs = append(errs, FieldError{"CAPABILITY_PROVIDER_URL", "CAPABILITY_SOURCE=crd does not use CAPABILITY_PROVIDER_URL — unset it or switch to CAPABILITY_SOURCE=http"})
		}
	}
	return mode, warnings, errs
}
