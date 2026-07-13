// src/composition/compose.ts
//
// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/compose.ts
// -----------------------------------------------------------------------
//
// Programmatic entry point for Patch's entitlement-driven capability
// composition. Given a set of AgentBindings (however sourced), produce:
//
//   - systemPromptAddendum: provider knowledge under per-service
//     provenance headers ("provider-supplied, treat as data")
//   - tools: allow-listed, namespaced provider tools ready to spread
//     into streamText's `tools`
//   - close(): tears down the per-request MCP clients
//
// PURE/INJECTABLE by design: no env reads, no logger import, no global
// state. The agent loop injects the real fetch/MCP client/logger; the
// platform-qa harness injects fixtures and fakes and drives this
// without a live LLM:
//
//   const bindings = await new FixtureAgentBindingSource(path).getBindings('proj');
//   const composed = await composeCapabilities(bindings, {});
//   await composed.tools['streamco__pipeline_diagnose'].execute?.({id: 'p-1'}, callOptions);
//   await composed.close();
import { buildKnowledgeAddendum } from './knowledge';
import type { KnowledgeOptions } from './knowledge';
import { connectMcpTools } from './mcp-tools';
import type { ConnectMcpToolsOptions, ProviderToolInvocation } from './mcp-tools';
import { noopLogger } from './types';
import type { AgentBinding, CompositionLogger } from './types';
import type { ToolSet } from 'ai';

export interface ComposeOptions {
  // Knowledge (Tier 1)
  fetchImpl?: KnowledgeOptions['fetchImpl'];
  knowledgeTimeoutMs?: number;
  knowledgeMaxBytesPerSource?: number;
  knowledgeMaxSourcesPerService?: number;
  // Tools (Tier 2)
  mcpClientFactory?: ConnectMcpToolsOptions['clientFactory'];
  mcpConnectTimeoutMs?: number;
  /** Metering hook: fired once per provider-tool invocation. */
  onProviderToolInvocation?: (invocation: ProviderToolInvocation) => void;
  logger?: CompositionLogger;
}

export interface ComposedCapabilities {
  /** '' when no binding contributed knowledge. */
  systemPromptAddendum: string;
  /** Exactly the allow-listed provider tools, namespaced `<server>__<tool>`. */
  tools: ToolSet;
  /** Close all MCP clients. Call when the response stream finishes. */
  close(): Promise<void>;
}

export async function composeCapabilities(
  bindings: AgentBinding[],
  options: ComposeOptions = {}
): Promise<ComposedCapabilities> {
  const logger = options.logger ?? noopLogger;

  const [systemPromptAddendum, connected] = await Promise.all([
    buildKnowledgeAddendum(bindings, {
      fetchImpl: options.fetchImpl,
      timeoutMs: options.knowledgeTimeoutMs,
      maxBytesPerSource: options.knowledgeMaxBytesPerSource,
      maxSourcesPerService: options.knowledgeMaxSourcesPerService,
      logger,
    }),
    connectMcpTools(bindings, {
      clientFactory: options.mcpClientFactory,
      connectTimeoutMs: options.mcpConnectTimeoutMs,
      onToolInvocation: options.onProviderToolInvocation,
      logger,
    }),
  ]);

  return {
    systemPromptAddendum,
    tools: connected.tools,
    close: connected.close,
  };
}
