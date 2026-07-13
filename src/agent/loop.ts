// src/agent/loop.ts
//
// The agent loop: given a user message and the task's project, compose
// the project's provider capabilities (knowledge + MCP tools), run
// streamText, and surface the run as a sequence of high-level events
// plus a final result. message/send drains it to completion; message/
// stream translates each event into an A2A SSE frame.
//
// This is the standalone analogue of cloud-portal's
// app/server/routes/assistant.ts streamText wiring — same composition
// call, same per-request MCP lifecycle (close at terminal state), same
// usage-metering shape — minus the portal's session/env coupling.
import { stepCountIs, streamText } from 'ai';
import { composeCapabilities } from '../composition';
import type {
  AgentBinding,
  AgentCapabilitySource,
  ComposeOptions,
  ComposedCapabilities,
  ProviderToolInvocation,
} from '../composition';
import type { Logger } from '../logger';
import {
  buildAssistantToolInvocationEvent,
  buildAssistantUsageEvents,
  type UsageEmitter,
  type UsageEvent,
} from '../usage';
import { buildSystemPrompt } from './prompt';
import type { ResolvedModel } from './model';

export const DEFAULT_STEP_LIMIT = 8;
export const DEFAULT_MAX_OUTPUT_TOKENS = 4096;

/** Fixed agent identity sent in the x-datum-agent attribution header. */
export const AGENT_ATTRIBUTION_NAME = 'patch';

/**
 * Attribution headers attached to every model call in gateway mode so the
 * gateway can meter and attribute usage per consumer. Names are fixed by
 * the gateway contract. Empty for non-gateway modes (we never leak the
 * project/conversation ids to the real Anthropic API).
 */
function attributionHeaders(
  mode: ResolvedModel['mode'],
  projectName: string,
  contextId: string
): Record<string, string> | undefined {
  if (mode !== 'gateway') return undefined;
  return {
    'x-datum-project': projectName,
    'x-datum-conversation': contextId,
    'x-datum-agent': AGENT_ATTRIBUTION_NAME,
  };
}

export interface AgentDeps {
  model: ResolvedModel;
  usageEmitter: UsageEmitter;
  logger: Logger;
  /** Binding source (fixture in the local slice); undefined ⇒ no provider caps. */
  bindingsSource?: AgentCapabilitySource;
  /** Max agent steps (tool-call rounds). */
  stepLimit?: number;
  /**
   * Overrides forwarded to composeCapabilities — the seam tests use to
   * inject a fake fetch or MCP client factory. Never set in production.
   */
  composeOverrides?: Partial<ComposeOptions>;
}

export interface RunAgentParams {
  userText: string;
  projectName: string;
  /** A2A contextId == conversation id == metering resource name. */
  contextId: string;
  taskId: string;
  /** Polled between stream steps; returning true aborts the run. */
  isCanceled?: () => boolean;
}

export type AgentStreamEvent =
  | { type: 'text-delta'; text: string }
  | { type: 'tool-call'; toolName: string };

export interface AgentUsageSummary {
  inputTokens?: number;
  outputTokens?: number;
  tokenEventCount: number;
  toolInvocationEventCount: number;
  /** True when USAGE_GATEWAY_URL was set and the batch POST returned 2xx. */
  emitted: boolean;
}

export interface AgentResult {
  state: 'completed' | 'failed' | 'canceled';
  text: string;
  error?: string;
  usage: AgentUsageSummary;
  /** The exact usage events that were built (for tests/observability). */
  usageEvents: UsageEvent[];
}

/**
 * Run the agent for one task. Yields text/tool events as they arrive and
 * returns the terminal result. Always composes capabilities, always
 * closes the MCP clients (terminal or error), always attempts usage
 * emission (a no-op when the gateway is unconfigured; never throws).
 */
export async function* runAgent(
  params: RunAgentParams,
  deps: AgentDeps
): AsyncGenerator<AgentStreamEvent, AgentResult> {
  const { userText, projectName, contextId, taskId, isCanceled } = params;
  const { model, usageEmitter, logger } = deps;

  const bindings = await loadBindings(deps, projectName, logger);

  const toolInvocations: ProviderToolInvocation[] = [];
  const composed: ComposedCapabilities = await composeCapabilities(bindings, {
    logger,
    onProviderToolInvocation: (invocation) => {
      toolInvocations.push(invocation);
      logger.info('agent.tool.invoked', {
        taskId,
        projectName,
        service: invocation.serviceName,
        tool: invocation.namespacedToolName,
      });
    },
    ...deps.composeOverrides,
  });

  let text = '';
  let state: AgentResult['state'] = 'completed';
  let error: string | undefined;
  let usageInputTokens: number | undefined;
  let usageOutputTokens: number | undefined;
  let cachedInputTokens: number | undefined;
  let cacheCreationInputTokens: number | undefined;

  try {
    const result = streamText({
      model: model.model,
      system: buildSystemPrompt(composed.systemPromptAddendum),
      messages: [{ role: 'user', content: userText }],
      tools: composed.tools,
      stopWhen: stepCountIs(deps.stepLimit ?? DEFAULT_STEP_LIMIT),
      maxOutputTokens: DEFAULT_MAX_OUTPUT_TOKENS,
      // Gateway mode only: consumer attribution for gateway metering.
      // streamText forwards these to every model call across tool steps.
      headers: attributionHeaders(model.mode, projectName, contextId),
    });

    for await (const part of result.fullStream) {
      if (isCanceled?.()) {
        state = 'canceled';
        logger.info('agent.canceled', { taskId, projectName });
        break;
      }
      switch (part.type) {
        case 'text-delta':
          text += part.text;
          yield { type: 'text-delta', text: part.text };
          break;
        case 'tool-call':
          yield { type: 'tool-call', toolName: part.toolName };
          break;
        case 'error':
          throw part.error instanceof Error ? part.error : new Error(String(part.error));
        default:
          break;
      }
    }

    if (state !== 'canceled') {
      // Bill the SUM across all steps, not just the final step. On a
      // tool-using turn streamText runs multiple model calls (tool-call
      // step + answer step); `result.usage` is only the LAST step's usage,
      // so reading it under-counts (drops the tool-call step's tokens).
      // `result.totalUsage` is the aggregate the gateway's llmRequestCosts
      // also counts — keep them equal.
      const usage = await result.totalUsage;
      usageInputTokens = usage.inputTokens;
      usageOutputTokens = usage.outputTokens;
      cachedInputTokens = usage.inputTokenDetails?.cacheReadTokens;
      cacheCreationInputTokens = usage.inputTokenDetails?.cacheWriteTokens;
    }
  } catch (err) {
    state = 'failed';
    error = err instanceof Error ? err.message : String(err);
    logger.error('agent.run.failed', { taskId, projectName, error });
  } finally {
    // Provider MCP clients are per-task; tear them down unconditionally.
    await composed.close().catch((closeErr: unknown) => {
      logger.warn('agent.compose.close_failed', {
        taskId,
        error: closeErr instanceof Error ? closeErr.message : String(closeErr),
      });
    });
  }

  // ── Usage metering (terminal) ───────────────────────────────
  // Token meters from the model's usage + one tool-invocations event per
  // provider tool call. Never bill without a project (the gateway
  // attributes via projectRef). Emission is best-effort and never throws.
  const usageEvents: UsageEvent[] = [];
  if (projectName) {
    usageEvents.push(
      ...buildAssistantUsageEvents({
        projectName,
        conversationId: contextId,
        model: model.modelId,
        tokens: {
          inputTokens: usageInputTokens,
          outputTokens: usageOutputTokens,
          cachedInputTokens,
          cacheCreationInputTokens,
        },
      })
    );
    for (const invocation of toolInvocations) {
      usageEvents.push(
        buildAssistantToolInvocationEvent({
          projectName,
          conversationId: contextId,
          serviceName: invocation.serviceName,
        })
      );
    }
  }

  const emitResult = await usageEmitter.emit(usageEvents);

  return {
    state,
    text,
    error,
    usage: {
      inputTokens: usageInputTokens,
      outputTokens: usageOutputTokens,
      tokenEventCount: usageEvents.filter((e) => !e.meterName.endsWith('tool-invocations')).length,
      toolInvocationEventCount: toolInvocations.length,
      emitted: emitResult.ok && !emitResult.noop,
    },
    usageEvents,
  };
}

async function loadBindings(
  deps: AgentDeps,
  projectName: string,
  logger: Logger
): Promise<AgentBinding[]> {
  if (!deps.bindingsSource) return [];
  try {
    return await deps.bindingsSource.getBindings(projectName);
  } catch (err) {
    // A binding-source failure degrades to the built-in-only assistant
    // (which, in v0, is no provider tools) rather than failing the chat.
    logger.warn('agent.bindings.load_failed', {
      projectName,
      error: err instanceof Error ? err.message : String(err),
    });
    return [];
  }
}
