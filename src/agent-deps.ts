// src/agent-deps.ts
//
// Wire the agent loop's dependency graph from Config: binding source
// (fixture in the local slice), resolved model, and usage emitter. Kept
// out of server.ts so the wiring is reusable (boot + integration tests)
// without pulling in the HTTP layer.
import { resolveModel, type AgentDeps } from './agent';
import { FixtureAgentBindingSource } from './composition';
import type { AgentCapabilitySource } from './composition';
import type { Config } from './config';
import type { Logger } from './logger';
import { createUsageEmitter } from './usage';

export function runAgentDepsFromConfig(config: Config, logger: Logger): AgentDeps {
  let bindingsSource: AgentCapabilitySource | undefined;
  if (config.agentBindingsFixture) {
    bindingsSource = new FixtureAgentBindingSource(config.agentBindingsFixture, { logger });
    logger.info('agent.bindings.source', { type: 'fixture', path: config.agentBindingsFixture });
  } else {
    logger.warn('agent.bindings.source', {
      type: 'none',
      reason: 'AGENT_BINDINGS_FIXTURE unset — no provider capabilities will be composed',
    });
  }

  const model = resolveModel(config, logger);

  const usageEmitter = createUsageEmitter({
    gatewayUrl: config.usage.gatewayUrl,
    apiKey: config.usage.gatewayApiKey,
    source: `${config.publicBaseUrl}/a2a`,
    logger,
  });

  return { model, usageEmitter, logger, bindingsSource };
}
