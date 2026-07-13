// src/composition/fixture-source.ts
//
// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/fixture-source.ts
// -----------------------------------------------------------------------
//
// Local-slice implementation of AgentCapabilitySource: reads a JSON
// file of AgentBinding objects, i.e. the output of
//
//   kubectl get agentbindings -o json | jq '.items' > bindings.json
//
// The path comes from env AGENT_BINDINGS_FIXTURE, but this class takes
// it as a constructor arg so it stays env-free and harness-drivable —
// the route (here: the agent loop wiring) is the only place that reads
// the env var.
//
// Both the bare-array form (`.items`) and the full List object
// (`{items: [...]}`) are accepted, since exporters produce either.
// Entries that fail schema validation are skipped with a warning
// rather than failing the whole file — one malformed binding must not
// take down every provider's capabilities.
import { agentBindingSchema, noopLogger } from './types';
import type { AgentBinding, AgentCapabilitySource, CompositionLogger } from './types';
import { readFile } from 'node:fs/promises';

export interface FixtureAgentBindingSourceOptions {
  logger?: CompositionLogger;
}

export class FixtureAgentBindingSource implements AgentCapabilitySource {
  private readonly fixturePath: string;
  private readonly logger: CompositionLogger;

  constructor(fixturePath: string, options: FixtureAgentBindingSourceOptions = {}) {
    this.fixturePath = fixturePath;
    this.logger = options.logger ?? noopLogger;
  }

  /**
   * The fixture file IS the project's binding set (the export was
   * project-scoped), so `projectName` is not used for filtering here.
   * The control-plane source is the one that queries by project.
   */
  async getBindings(_projectName: string): Promise<AgentBinding[]> {
    const raw = await readFile(this.fixturePath, 'utf8');

    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch (err) {
      throw new Error(
        `AGENT_BINDINGS_FIXTURE at ${this.fixturePath} is not valid JSON: ${
          err instanceof Error ? err.message : String(err)
        }`
      );
    }

    const items = Array.isArray(parsed)
      ? parsed
      : isObjectWithItems(parsed)
        ? parsed.items
        : undefined;

    if (!items) {
      throw new Error(
        `AGENT_BINDINGS_FIXTURE at ${this.fixturePath} must be a JSON array of AgentBinding objects (or a List object with an "items" array)`
      );
    }

    const bindings: AgentBinding[] = [];
    for (const [index, item] of items.entries()) {
      const result = agentBindingSchema.safeParse(item);
      if (result.success) {
        bindings.push(result.data);
      } else {
        this.logger.warn('agent-bindings fixture entry skipped: schema validation failed', {
          fixturePath: this.fixturePath,
          index,
          issues: result.error.issues.map((i) => `${i.path.join('.')}: ${i.message}`),
        });
      }
    }
    return bindings;
  }
}

function isObjectWithItems(value: unknown): value is { items: unknown[] } {
  return (
    typeof value === 'object' &&
    value !== null &&
    Array.isArray((value as { items?: unknown }).items)
  );
}
