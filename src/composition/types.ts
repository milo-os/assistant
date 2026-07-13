// src/composition/types.ts
//
// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/types.ts
// Kept byte-compatible so AgentBinding fixtures produced for the portal
// slice parse here unchanged. Field names mirror the CRD (see below).
// -----------------------------------------------------------------------
//
// TypeScript mirror of the services.miloapis.com/v1alpha1 AgentBinding
// CRD (see the AI Agent Framework enhancement / build contract). Field
// names are intentionally IDENTICAL to the CRD spec so a
// `kubectl get agentbindings -o json | jq '.items'` dump round-trips
// through these types without any mapping layer.
//
// The zod schemas double as the fixture-file validators used by
// FixtureAgentBindingSource. They deliberately strip unknown fields
// (zod default) rather than reject them, so newer CRD fields never
// break an older portal.
//
// This module is PURE: no env, no logger, no HTTP. Everything the
// composition path needs from the outside world is injected (see
// compose.ts), which lets the platform-qa harness drive it without a
// live LLM or portal env.
import { z } from 'zod';

// ─────────────────────────────────────────────────────────────
// CRD spec schemas (services.miloapis.com/v1alpha1 AgentBinding)
// ─────────────────────────────────────────────────────────────

/** GVKRef style used across the service catalog: {group, kind}, no version. */
export const gvkRefSchema = z.object({
  group: z.string(),
  kind: z.string(),
});

export const knowledgeSourceSchema = z.object({
  type: z.enum(['LLMDocs', 'Runbook', 'Markdown']),
  title: z.string().optional(),
  url: z.string(),
});

export const knowledgeConceptSchema = z.object({
  gvk: gvkRefSchema,
  summary: z.string(),
});

export const agentKnowledgeSchema = z.object({
  sources: z.array(knowledgeSourceSchema).default([]),
  concepts: z.array(knowledgeConceptSchema).default([]),
});

export const mcpToolSelectorSchema = z.object({
  include: z.array(z.string()).default([]),
});

export const mcpServerSchema = z.object({
  name: z.string().min(1),
  endpoint: z.string(),
  toolSelector: mcpToolSelectorSchema,
  mutating: z.array(z.string()).default([]),
});

export const agentToolsSchema = z.object({
  mcpServers: z.array(mcpServerSchema).default([]),
});

export const agentAuthoritySchema = z.object({
  reads: z.array(z.object({ gvk: gvkRefSchema })).default([]),
  maxTaskDurationSeconds: z.number().int().optional(),
});

export const agentBindingSpecSchema = z.object({
  serviceRef: z.object({ name: z.string() }),
  /** Reverse-DNS provider service name, e.g. `streaming.streamco.example`. */
  serviceName: z.string().min(1),
  serviceAgentRef: z.object({ name: z.string() }),
  configurationVersion: z.string(),
  // knowledge/tools/authority are projected verbatim from the active
  // ServiceAgentConfiguration; each may be absent when the provider
  // published an empty tier.
  knowledge: agentKnowledgeSchema.optional(),
  tools: agentToolsSchema.optional(),
  authority: agentAuthoritySchema.optional(),
});

export const agentBindingConditionSchema = z.object({
  type: z.string(),
  status: z.string(),
  reason: z.string().optional(),
  message: z.string().optional(),
});

export const agentBindingSchema = z.object({
  apiVersion: z.string().optional(),
  kind: z.string().optional(),
  metadata: z
    .object({
      name: z.string().optional(),
      namespace: z.string().optional(),
    })
    .optional(),
  spec: agentBindingSpecSchema,
  status: z
    .object({
      conditions: z.array(agentBindingConditionSchema).optional(),
    })
    .optional(),
});

export type GVKRef = z.infer<typeof gvkRefSchema>;
export type KnowledgeSource = z.infer<typeof knowledgeSourceSchema>;
export type KnowledgeConcept = z.infer<typeof knowledgeConceptSchema>;
export type AgentKnowledge = z.infer<typeof agentKnowledgeSchema>;
export type McpToolSelector = z.infer<typeof mcpToolSelectorSchema>;
export type McpServer = z.infer<typeof mcpServerSchema>;
export type AgentTools = z.infer<typeof agentToolsSchema>;
export type AgentAuthority = z.infer<typeof agentAuthoritySchema>;
export type AgentBindingSpec = z.infer<typeof agentBindingSpecSchema>;
export type AgentBinding = z.infer<typeof agentBindingSchema>;

// ─────────────────────────────────────────────────────────────
// Capability source seam
// ─────────────────────────────────────────────────────────────

/**
 * Where AgentBindings come from. The local slice uses
 * FixtureAgentBindingSource (JSON file exported from the control
 * plane); production will use the control-plane-backed source.
 */
export interface AgentCapabilitySource {
  getBindings(projectName: string): Promise<AgentBinding[]>;
}

// ─────────────────────────────────────────────────────────────
// Injectable logger
// ─────────────────────────────────────────────────────────────

/**
 * Minimal structural subset of `@/modules/logger`'s Logger. Declared
 * here (instead of importing the logger module) so the composition
 * module never drags in the env-validated logger config — a hard
 * requirement for running this module from a standalone harness.
 */
export interface CompositionLogger {
  info(message: string, data?: Record<string, unknown>): void;
  warn(message: string, data?: Record<string, unknown>): void;
}

export const noopLogger: CompositionLogger = {
  info: () => {},
  warn: () => {},
};
