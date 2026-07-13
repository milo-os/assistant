// src/composition/mcp-tools.ts
//
// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/mcp-tools.ts
// Only change: the default MCP client's `clientName` is now
// 'datum-assistant-service' (was 'datum-cloud-portal-patch') since this
// runtime is no longer the portal.
// -----------------------------------------------------------------------
//
// Tier 2 (tools) composition: connect one MCP client per binding
// mcpServer, expose ONLY the tools named in `toolSelector.include`, and
// namespace each as `<serverName>__<tool>` so provider tools can never
// collide with Patch's built-in tools (which are all camelCase, no
// double underscore) or with another provider's.
//
// The MCP client is injected via `McpClientFactory` so unit tests and
// the platform-qa harness can compose against a mock. The default
// factory uses the AI SDK MCP client with the Streamable HTTP
// transport. NOTE: in the installed AI SDK v6 (`ai@6.x`) the MCP client
// no longer lives in the `ai` core package — `experimental_createMCPClient`
// moved to `@ai-sdk/mcp` (v1.x is the ai-v6-compatible line) where it is
// exported as `createMCPClient`, and the Streamable HTTP transport is
// selected with `transport: {type: 'http'}`.
//
// Lifecycle: `connectMcpTools` returns a `close()` that the agent loop
// calls once the response stream finishes (success or error). Clients
// are per-request; there is no pooling in the local slice.
//
// Failure policy: a server that cannot be reached contributes no tools
// and is logged — it never fails the chat request or other bindings.
import { noopLogger } from './types';
import type { AgentBinding, CompositionLogger, McpServer } from './types';
import { createMCPClient } from '@ai-sdk/mcp';
import type { Tool, ToolSet } from 'ai';

export const DEFAULT_MCP_CONNECT_TIMEOUT_MS = 5_000;

/** Separator between the MCP server shortname and the provider tool name. */
export const TOOL_NAMESPACE_SEPARATOR = '__';

/**
 * Structural subset of `@ai-sdk/mcp`'s MCPClient — just what the
 * composition path needs, so mocks stay trivial.
 */
export interface McpClientLike {
  tools(): Promise<Record<string, Tool>>;
  close(): Promise<void>;
}

export type McpClientFactory = (server: McpServer, binding: AgentBinding) => Promise<McpClientLike>;

/** Default factory: AI SDK MCP client over Streamable HTTP. */
export const defaultMcpClientFactory: McpClientFactory = (server) =>
  // Cast rationale: @ai-sdk/mcp@1.x pins @ai-sdk/provider-utils@4.0.38
  // while ai@6.0.x pins 4.0.30, so bun nests a second provider-utils
  // copy under @ai-sdk/mcp. Its `Schema` type is branded with a unique
  // [schemaSymbol], making the MCPClient's Tool record nominally (not
  // structurally) incompatible with `ai`'s Tool. The runtime shapes are
  // identical; this seam is the single place we bridge the two.
  createMCPClient({
    transport: { type: 'http', url: server.endpoint },
    clientName: 'datum-assistant-service',
  }) as unknown as Promise<McpClientLike>;

/** Emitted once per provider-tool invocation (metering hook). */
export interface ProviderToolInvocation {
  /** Reverse-DNS service name from the binding, e.g. `streaming.streamco.example`. */
  serviceName: string;
  /** mcpServers[].name shortname the tool is namespaced under. */
  serverName: string;
  /** Original (un-namespaced) provider tool name. */
  toolName: string;
  /** Name the model sees, `<serverName>__<tool>`. */
  namespacedToolName: string;
}

export interface ConnectMcpToolsOptions {
  clientFactory?: McpClientFactory;
  connectTimeoutMs?: number;
  /**
   * Called synchronously at the start of every provider-tool execution.
   * The agent loop wires this to usage metering; the composition module
   * itself never emits events (keeps it harness-drivable).
   */
  onToolInvocation?: (invocation: ProviderToolInvocation) => void;
  logger?: CompositionLogger;
}

export interface ConnectedMcpTools {
  tools: ToolSet;
  /** Close every connected MCP client. Safe to call more than once. */
  close(): Promise<void>;
}

export async function connectMcpTools(
  bindings: AgentBinding[],
  options: ConnectMcpToolsOptions = {}
): Promise<ConnectedMcpTools> {
  const {
    clientFactory = defaultMcpClientFactory,
    connectTimeoutMs = DEFAULT_MCP_CONNECT_TIMEOUT_MS,
    onToolInvocation,
    logger = noopLogger,
  } = options;

  const tools: ToolSet = {};
  const clients: McpClientLike[] = [];

  await Promise.all(
    bindings.flatMap((binding) =>
      (binding.spec.tools?.mcpServers ?? []).map(async (server) => {
        const serviceName = binding.spec.serviceName;
        try {
          const client = await withTimeout(
            clientFactory(server, binding),
            connectTimeoutMs,
            // A client that resolves after the deadline would otherwise
            // leak its connection — close it as soon as it shows up.
            (lateClient) => void lateClient.close().catch(() => {})
          );
          clients.push(client);

          const serverTools = await client.tools();
          for (const toolName of server.toolSelector.include) {
            const providerTool = serverTools[toolName];
            if (!providerTool) {
              logger.warn('assistant.composition.mcp.tool_missing', {
                service: serviceName,
                server: server.name,
                tool: toolName,
              });
              continue;
            }
            const namespacedToolName = namespaceToolName(server.name, toolName);
            if (tools[namespacedToolName]) {
              logger.warn('assistant.composition.mcp.tool_collision', {
                service: serviceName,
                server: server.name,
                tool: namespacedToolName,
              });
              continue; // first registration wins, deterministically
            }
            tools[namespacedToolName] = wrapProviderTool(providerTool, () =>
              onToolInvocation?.({
                serviceName,
                serverName: server.name,
                toolName,
                namespacedToolName,
              })
            );
          }
        } catch (err) {
          logger.warn('assistant.composition.mcp.connect_failed', {
            service: serviceName,
            server: server.name,
            endpoint: server.endpoint,
            error: err instanceof Error ? err.message : String(err),
          });
        }
      })
    )
  );

  let closed = false;
  const close = async (): Promise<void> => {
    if (closed) return;
    closed = true;
    await Promise.allSettled(clients.map((client) => client.close()));
  };

  return { tools, close };
}

/**
 * Namespace a provider tool as `<serverName>__<tool>`, sanitised to the
 * `[a-zA-Z0-9_-]` character set model providers require for tool names.
 */
export function namespaceToolName(serverName: string, toolName: string): string {
  const sanitize = (value: string): string => value.replace(/[^a-zA-Z0-9_-]/g, '-');
  return `${sanitize(serverName)}${TOOL_NAMESPACE_SEPARATOR}${sanitize(toolName)}`;
}

/**
 * Wrap a provider tool so every execution fires the metering hook
 * before delegating. Description/schema pass through untouched.
 */
function wrapProviderTool(providerTool: Tool, onInvocation: () => void): Tool {
  const execute = providerTool.execute;
  if (!execute) return providerTool;
  return {
    ...providerTool,
    execute: (input, executionOptions) => {
      onInvocation();
      return execute.call(providerTool, input, executionOptions);
    },
  } as Tool;
}

function withTimeout<T>(promise: Promise<T>, ms: number, onLate: (value: T) => void): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`timed out after ${ms}ms`));
      promise.then(onLate).catch(() => {});
    }, ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      }
    );
  });
}
