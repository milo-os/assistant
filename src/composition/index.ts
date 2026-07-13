// src/composition/index.ts
//
// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/index.ts
// -----------------------------------------------------------------------
//
// Entitlement-driven capability composition for Patch. See README.md
// in the service root for the programmatic entry point and env wiring.
export { composeCapabilities } from './compose';
export type { ComposeOptions, ComposedCapabilities } from './compose';
export { FixtureAgentBindingSource } from './fixture-source';
export type { FixtureAgentBindingSourceOptions } from './fixture-source';
export { ControlPlaneAgentBindingSource } from './control-plane-source';
export type { ControlPlaneAgentBindingSourceOptions } from './control-plane-source';
export {
  buildKnowledgeAddendum,
  DEFAULT_KNOWLEDGE_MAX_BYTES_PER_SOURCE,
  DEFAULT_KNOWLEDGE_MAX_SOURCES_PER_SERVICE,
  DEFAULT_KNOWLEDGE_TIMEOUT_MS,
  TRUNCATION_MARKER,
} from './knowledge';
export type { KnowledgeOptions } from './knowledge';
export {
  connectMcpTools,
  defaultMcpClientFactory,
  namespaceToolName,
  DEFAULT_MCP_CONNECT_TIMEOUT_MS,
  TOOL_NAMESPACE_SEPARATOR,
} from './mcp-tools';
export type {
  ConnectedMcpTools,
  ConnectMcpToolsOptions,
  McpClientFactory,
  McpClientLike,
  ProviderToolInvocation,
} from './mcp-tools';
export {
  agentBindingSchema,
  agentBindingSpecSchema,
  agentKnowledgeSchema,
  agentToolsSchema,
  agentAuthoritySchema,
  mcpServerSchema,
  noopLogger,
} from './types';
export type {
  AgentBinding,
  AgentBindingSpec,
  AgentCapabilitySource,
  AgentAuthority,
  AgentKnowledge,
  AgentTools,
  CompositionLogger,
  GVKRef,
  KnowledgeConcept,
  KnowledgeSource,
  McpServer,
  McpToolSelector,
} from './types';
