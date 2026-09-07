package a2a

// card.go builds the public A2A v1.0 AgentCard. The package doc lives in
// runner.go.

import (
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/milo-os/assistant/internal/capability"
	"github.com/milo-os/assistant/internal/config"
)

// AgentVersion is the assistant's own version (AgentCard.version), distinct
// from the A2A protocol version. Mirrors the TS AGENT_VERSION.
const AgentVersion = "0.1.0"

// BearerSchemeName is the security-scheme key advertised on the agent card.
const BearerSchemeName = "bearer"

// Capabilities is the single source of truth for what this agent supports. It
// feeds BOTH the published card and a2asrv.WithCapabilityChecks — the handler
// refuses GetExtendedAgentCard when its own copy says false, so the two must
// never drift.
func Capabilities() a2a.AgentCapabilities {
	return a2a.AgentCapabilities{
		Streaming:         true,
		PushNotifications: false,
		ExtendedAgentCard: true,
	}
}

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
		Capabilities:       Capabilities(),
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
					// Deliberately names no provider: this card is public, and
					// which services exist is not public information.
					"Diagnose pipeline p-1 for an entitled service",
					"What can you help me with in this project?",
				},
				InputModes:  []string{"text/plain"},
				OutputModes: []string{"text/plain"},
			},
		},
	}
}

// BuildExtendedAgentCard returns the AUTHENTICATED card for one project: the
// public card plus one skill per entitled provider service. Service names, tool
// names and MCP endpoints are safe here — the caller has been authorized for
// this project — but every value MUST come from that project's capability
// documents.
func BuildExtendedAgentCard(cfg *config.Config, ents []capability.ServiceEntitlement) *a2a.AgentCard {
	return BuildExtendedAgentCardFromSkills(cfg, ServiceSkills(ents))
}

// BuildExtendedAgentCardFromSkills shapes the extended card around skills an
// [SkillAdvertiser] already derived, so the server's card producer never
// re-implements card shaping. The generic project-assistant skill stays first:
// a project entitled to nothing still advertises something.
func BuildExtendedAgentCardFromSkills(cfg *config.Config, skills []a2a.AgentSkill) *a2a.AgentCard {
	card := BuildAgentCard(cfg)
	card.Skills = append(card.Skills, skills...)
	return card
}

// ServiceSkills renders one A2A skill per entitled service. IDs come from
// [capability.ServiceEntitlement.ID] — the sanitized, de-duplicated serviceRef —
// rather than the raw provider-supplied name, so they are collision-free, stable
// across calls, and follow the same convention as the tool and skill names on
// the same card.
func ServiceSkills(ents []capability.ServiceEntitlement) []a2a.AgentSkill {
	skills := make([]a2a.AgentSkill, 0, len(ents))
	for _, ent := range ents {
		skills = append(skills, a2a.AgentSkill{
			ID:          ent.ID,
			Name:        ent.ServiceName,
			Description: serviceSkillDescription(ent),
			Tags:        append([]string{"datum", "provider-service", ent.ID}, ent.ToolNames...),
			// Only real published skill descriptions become examples — an
			// invented example would misrepresent what the service does.
			Examples:    nonEmpty(ent.SkillDescriptions),
			InputModes:  []string{"text/plain"},
			OutputModes: []string{"text/plain"},
		})
	}
	return skills
}

// serviceSkillDescription names the service and everything the project is
// entitled to on it. MCP endpoints belong here rather than in Tags: they are
// addresses, not keywords.
func serviceSkillDescription(ent capability.ServiceEntitlement) string {
	parts := []string{fmt.Sprintf("Provider service %s, entitled to this project.", ent.ServiceName)}
	if len(ent.ToolNames) > 0 {
		parts = append(parts, "Tools: "+strings.Join(ent.ToolNames, ", ")+".")
	}
	if len(ent.SkillNames) > 0 {
		parts = append(parts, "Skills: "+strings.Join(ent.SkillNames, ", ")+".")
	}
	if len(ent.KnowledgeTitles) > 0 {
		parts = append(parts, "Knowledge: "+strings.Join(ent.KnowledgeTitles, ", ")+".")
	}
	if len(ent.MCPEndpoints) > 0 {
		parts = append(parts, "MCP endpoints: "+strings.Join(ent.MCPEndpoints, ", ")+".")
	}
	return strings.Join(parts, " ")
}

// nonEmpty drops blank entries so an undescribed skill does not become an empty
// example string on the card.
func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
