// src/usage/emitter.ts
//
// ADAPTED from cloud-portal main:app/modules/usage/emitter.ts
//   Change: the portal read the gateway URL / api-key / source from its
//   env+logger singletons. This standalone version is a FACTORY —
//   `createUsageEmitter(config)` — so nothing here reads process.env and
//   the emitter is trivially harness-drivable. The WIRE CONTRACT is
//   byte-identical: POST <collector>/cloudevents, JSON array of
//   CloudEvents, 100/batch cap, optional x-api-key, never throws.
// -----------------------------------------------------------------------
//
// Thin HTTP client that POSTs CloudEvent batches to the in-cluster usage
// collector (Vector). This is the blessed ingestion path: producer ->
// Vector -> gateway -> NATS. Vector provides Tier-1 disk durability and
// injects the gateway api-key when forwarding, so producers post
// plaintext with no auth. Calling the ingestion gateway directly is
// explicitly discouraged (see billing/docs/emitting-usage.md) because it
// loses that durability guarantee.
//
// Until the collector is reachable in the deployment env (i.e.
// USAGE_GATEWAY_URL is unset), `emit` is a no-op; this lets the rest of
// the codebase wire emission unconditionally without breaking dev.
//
// On any error we log and resolve — never throw — so usage emission can
// never fail a user's chat request.
import { toCloudEvent } from './to-cloud-event';
import type { CloudEvent, UsageEvent } from './types';
import type { Logger } from '../logger';
import { silentLogger } from '../logger';

const EMIT_TIMEOUT_MS = 5_000;
// Vector's http_server source listens on this path; the downstream
// gateway (which Vector forwards to) enforces the 100-event batch cap.
const BATCH_PATH = '/cloudevents';
const MAX_BATCH_SIZE = 100;

export interface EmitResult {
  /** True when the gateway accepted the batch (2xx) OR was disabled. */
  ok: boolean;
  /** True when no gateway is configured — events were intentionally dropped. */
  noop: boolean;
  /** Number of events submitted (0 when noop or empty). */
  count: number;
  /** HTTP status if a request was made. */
  status?: number;
  /** Error message when ok=false. */
  error?: string;
}

export interface UsageEmitterConfig {
  /** Collector base URL (env USAGE_GATEWAY_URL). Unset ⇒ emit is a no-op. */
  gatewayUrl?: string;
  /** Optional gateway api-key (env USAGE_GATEWAY_API_KEY). */
  apiKey?: string;
  /** CloudEvents `source` URI identifying this producer. */
  source: string;
  logger?: Logger;
  /** Injectable fetch for tests. Defaults to global fetch. */
  fetchImpl?: typeof fetch;
}

export interface UsageEmitter {
  emit(events: UsageEvent[]): Promise<EmitResult>;
}

export function createUsageEmitter(config: UsageEmitterConfig): UsageEmitter {
  const logger = config.logger ?? silentLogger;
  const fetchImpl = config.fetchImpl ?? fetch;

  async function emit(events: UsageEvent[]): Promise<EmitResult> {
    if (events.length === 0) {
      return { ok: true, noop: false, count: 0 };
    }

    if (!config.gatewayUrl) {
      logger.debug('usage.emit.noop', {
        reason: 'USAGE_GATEWAY_URL not configured',
        eventCount: events.length,
        meterNames: [...new Set(events.map((e) => e.meterName))],
      });
      return { ok: true, noop: true, count: events.length };
    }

    const cloudEvents = events.map((e) => toCloudEvent(e, { source: config.source }));
    const batches = chunk(cloudEvents, MAX_BATCH_SIZE);

    let lastStatus: number | undefined;
    for (const batch of batches) {
      const result = await postBatch(config.gatewayUrl, batch, config, fetchImpl, logger);
      lastStatus = result.status;
      if (!result.ok) {
        return {
          ok: false,
          noop: false,
          count: events.length,
          status: result.status,
          error: result.error,
        };
      }
    }

    return { ok: true, noop: false, count: events.length, status: lastStatus };
  }

  return { emit };
}

interface PostBatchResult {
  ok: boolean;
  status?: number;
  error?: string;
}

async function postBatch(
  gateway: string,
  batch: CloudEvent[],
  config: UsageEmitterConfig,
  fetchImpl: typeof fetch,
  logger: Logger
): Promise<PostBatchResult> {
  const url = new URL(BATCH_PATH, gateway).toString();
  const headers: Record<string, string> = { 'content-type': 'application/json' };
  if (config.apiKey) {
    headers['x-api-key'] = config.apiKey;
  }

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), EMIT_TIMEOUT_MS);

  try {
    const res = await fetchImpl(url, {
      method: 'POST',
      headers,
      body: JSON.stringify(batch),
      signal: controller.signal,
    });

    if (!res.ok) {
      const body = await res.text().catch(() => '');
      logger.warn('usage.emit.failed', {
        status: res.status,
        eventCount: batch.length,
        eventIDs: batch.map((e) => e.id),
        meterNames: [...new Set(batch.map((e) => e.type))],
        body: body.slice(0, 512),
      });
      return { ok: false, status: res.status, error: body };
    }

    // 207 Multi-Status: some events were rejected structurally. Log so
    // operators can spot a regression in our builder; the accepted
    // events are durable in NATS and will flow downstream.
    if (res.status === 207) {
      const body = await res.text().catch(() => '');
      logger.warn('usage.emit.partial', {
        status: res.status,
        eventCount: batch.length,
        body: body.slice(0, 512),
      });
    } else {
      logger.info('usage.emit.ok', {
        status: res.status,
        eventCount: batch.length,
        meterNames: [...new Set(batch.map((e) => e.type))],
      });
    }
    return { ok: true, status: res.status };
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    logger.warn('usage.emit.failed', {
      eventCount: batch.length,
      eventIDs: batch.map((e) => e.id),
      meterNames: [...new Set(batch.map((e) => e.type))],
      error: message,
    });
    return { ok: false, error: message };
  } finally {
    clearTimeout(timer);
  }
}

function chunk<T>(items: T[], size: number): T[][] {
  if (items.length <= size) return [items];
  const out: T[][] = [];
  for (let i = 0; i < items.length; i += size) {
    out.push(items.slice(i, i + size));
  }
  return out;
}
