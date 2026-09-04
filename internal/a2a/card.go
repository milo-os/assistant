package a2a

// card.go builds the public A2A v1.0 AgentCard. The package doc lives in
// runner.go.

import (
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/milo-os/assistant/internal/config"
)

// AgentVersion is the assistant's own version (AgentCard.version), distinct
// from the A2A protocol version. Mirrors the TS AGENT_VERSION.
const AgentVersion = "0.1.0"

// BearerSchemeName is the security-scheme key advertised on the agent card.
const BearerSchemeName = "bearer"

// BuildAgentCard builds the A2A v1.0 AgentCard served at
// /.well-known/agent-card.json. UNSIGNED for v0 (no Signatures) — card signing
// is a documented follow-up. Ported from the TS buildAgentCard, retargeted to
// the a2a-go v1.0 card shape (SupportedInterfaces instead of the single
// url/preferredTransport pair).
func BuildAgentCard(cfg *config.Config) *a2a.AgentCard {
	endpoint := cfg.PublicBaseURL + "/a2a"
	return &a2a.AgentCard{
		Name: "Patch",
		Description: "Patch is the Datum Cloud assistant. It answers questions about a project and its " +
			"resources and can invoke provider service tools that are entitled to the project " +
			"through the Datum agent framework.",
		Version: AgentVersion,
		SupportedInterfaces: []*a2a.AgentInterface{
			{
				URL:             endpoint,
				ProtocolBinding: a2a.TransportProtocolJSONRPC,
				ProtocolVersion: a2a.Version,
			},
		},
		Provider: &a2a.AgentProvider{
			Org: "Datum",
			URL: "https://www.datum.net",
		},
		Capabilities: a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		SecuritySchemes: a2a.NamedSecuritySchemes{
			BearerSchemeName: a2a.HTTPAuthSecurityScheme{
				Scheme: "bearer",
				Description: "Bearer token. Resolved to an identity by the control plane " +
					"(Kubernetes TokenReview); project access is then decided by a " +
					"SubjectAccessReview.",
			},
		},
		SecurityRequirements: a2a.SecurityRequirementsOptions{
			{BearerSchemeName: a2a.SecuritySchemeScopes{}},
		},
		Skills: []a2a.AgentSkill{
			{
				ID:   "project-assistant",
				Name: "Project assistant",
				Description: "General assistance for a Datum Cloud project: answering questions about the " +
					"project and its resources, and running entitled provider service tools (for " +
					"example, diagnosing a provider pipeline).",
				Tags: []string{"datum", "assistant", "project", "agent-framework"},
				Examples: []string{
					"Diagnose pipeline p-1 for StreamCo",
					"What can you help me with in this project?",
				},
				InputModes:  []string{"text/plain"},
				OutputModes: []string{"text/plain"},
			},
		},
	}
}
