// src/agent/model.ts
//
// Resolve MODEL_MODE into a concrete AI SDK language model.
//   - mock:      the scripted MockLanguageModelV3 (no secrets, no network)
//   - anthropic: @ai-sdk/anthropic over ANTHROPIC_API_KEY
//   - gateway:   @ai-sdk/openai-compatible pointed at the Envoy AI Gateway.
//                The service holds NO upstream model credential — the
//                gateway injects it (BackendSecurityPolicy). Per-request
//                consumer attribution travels via x-datum-* headers set by
//                the agent loop, NOT via an API key.
import { createAnthropic } from '@ai-sdk/anthropic';
import { createOpenAICompatible } from '@ai-sdk/openai-compatible';
import type { LanguageModel } from 'ai';
import { readFileSync } from 'node:fs';
import type { Config } from '../config';
import type { Logger } from '../logger';
import { createMockLanguageModel, MOCK_MODEL_ID } from './mock-model';

export interface ResolvedModel {
  model: LanguageModel;
  /** Model id recorded on usage events (dimensions.model). */
  modelId: string;
  mode: Config['model']['mode'];
}

export function resolveModel(config: Config, logger?: Logger): ResolvedModel {
  if (config.model.mode === 'mock') {
    logger?.info('model.resolved', { mode: 'mock', modelId: MOCK_MODEL_ID });
    return { model: createMockLanguageModel(), modelId: MOCK_MODEL_ID, mode: 'mock' };
  }

  if (config.model.mode === 'gateway') {
    return resolveGatewayModel(config, logger);
  }

  const anthropic = createAnthropic({ apiKey: config.model.anthropicApiKey });
  const modelId = config.model.anthropicModel;
  logger?.info('model.resolved', { mode: 'anthropic', modelId });
  return { model: anthropic(modelId), modelId, mode: 'anthropic' };
}

function resolveGatewayModel(config: Config, logger?: Logger): ResolvedModel {
  const baseURL = config.model.gatewayUrl!; // validated in loadConfig
  const modelId = config.model.gatewayModel;
  const customFetch = gatewayFetch(config, logger);

  const provider = createOpenAICompatible({
    name: 'datum-ai-gateway',
    baseURL,
    // Intentionally NO apiKey: in gateway mode the service sends no
    // upstream credential (the gateway's BackendSecurityPolicy injects
    // it). Omitting apiKey means no `Authorization` header is sent.
    // Consumer identity/attribution is carried by the x-datum-* headers
    // the agent loop attaches per request.
    ...(customFetch ? { fetch: customFetch } : {}),
  });

  logger?.info('model.resolved', {
    mode: 'gateway',
    modelId,
    baseURL,
    tls: config.model.gatewayCaCert
      ? 'custom-ca'
      : config.model.gatewayTlsInsecure
        ? 'insecure'
        : 'default',
  });

  // Cast rationale: @ai-sdk/openai-compatible@2.x pins
  // @ai-sdk/provider-utils@4.0.38 while ai@6.0.x pins 4.0.30, so bun nests
  // a second provider-utils copy. Both are LanguageModelV3 (provider v3);
  // the runtime shapes are identical — this is the same nominal-vs-
  // structural bridge used in composition/mcp-tools.ts.
  return {
    model: provider.chatModel(modelId) as unknown as LanguageModel,
    modelId,
    mode: 'gateway',
  };
}

/**
 * Build a custom fetch for the gateway ONLY when a self-signed CA or
 * insecure-TLS is configured (local dev). The TLS options are honored by
 * Bun's fetch; over plain http (the common local posture) no custom fetch
 * is needed and this returns undefined.
 */
function gatewayFetch(config: Config, logger?: Logger): typeof fetch | undefined {
  const { gatewayCaCert, gatewayTlsInsecure } = config.model;
  if (!gatewayCaCert && !gatewayTlsInsecure) return undefined;

  const ca = gatewayCaCert ? readFileSync(gatewayCaCert, 'utf8') : undefined;
  if (gatewayTlsInsecure) {
    logger?.warn('model.gateway.tls_insecure', {
      reason: 'GATEWAY_TLS_INSECURE set — gateway TLS verification disabled (local only)',
    });
  }

  return ((input: string | URL | Request, init?: RequestInit) => {
    const tls: Record<string, unknown> = {};
    if (ca) tls.ca = ca;
    if (gatewayTlsInsecure) tls.rejectUnauthorized = false;
    // `tls` is a Bun fetch extension; typed via cast. Node ignores it (use
    // NODE_EXTRA_CA_CERTS or plain http there — documented in the README).
    return fetch(input, { ...init, tls } as RequestInit);
  }) as typeof fetch;
}
