// src/agent/cache-usage.test.ts
//
// Pinning test: the agent loop must map the provider's cache token counts
// from `usage.inputTokenDetails.{cacheReadTokens,cacheWriteTokens}` (the
// ai@6 shape) into the cache-read / cache-write meters. Reading the older
// flat `usage.cachedInputTokens` fields (which do not exist on ai@6) would
// silently drop the cache meters — this guards against that regression.
import { runAgent, type AgentDeps } from './loop';
import type { ResolvedModel } from './model';
import { silentLogger } from '../logger';
import { ASSISTANT_METERS, type UsageEmitter, type UsageEvent } from '../usage';
import { MockLanguageModelV3 } from 'ai/test';
import type { LanguageModelV3StreamPart } from '@ai-sdk/provider';
import { describe, expect, it } from 'bun:test';

function stream(parts: LanguageModelV3StreamPart[]): ReadableStream<LanguageModelV3StreamPart> {
  return new ReadableStream({
    start(controller) {
      for (const part of parts) controller.enqueue(part);
      controller.close();
    },
  });
}

/** A model that reports cache-read/cache-write tokens in its usage. */
function cacheReportingModel(): ResolvedModel {
  const model = new MockLanguageModelV3({
    modelId: 'cache-pin',
    doStream: async () => ({
      stream: stream([
        { type: 'stream-start', warnings: [] },
        { type: 'text-start', id: 't' },
        { type: 'text-delta', id: 't', delta: 'ok' },
        { type: 'text-end', id: 't' },
        {
          type: 'finish',
          finishReason: { unified: 'stop', raw: undefined },
          usage: {
            inputTokens: { total: 100, noCache: 80, cacheRead: 20, cacheWrite: 15 },
            outputTokens: { total: 30, text: 30, reasoning: 0 },
          },
        },
      ]),
    }),
  });
  return { model, modelId: 'cache-pin', mode: 'mock' };
}

function capturingEmitter(sink: UsageEvent[]): UsageEmitter {
  return {
    emit: async (events) => {
      sink.push(...events);
      return { ok: true, noop: false, count: events.length };
    },
  };
}

async function drain(deps: AgentDeps) {
  const gen = runAgent(
    { userText: 'hello', projectName: 'demo-project', contextId: 'conv-cache', taskId: 'task-1' },
    deps
  );
  for (;;) {
    const next = await gen.next();
    if (next.done) return next.value;
  }
}

describe('agent loop cache-token metering', () => {
  it('emits cache-read + cache-write meters from usage.inputTokenDetails', async () => {
    const captured: UsageEvent[] = [];
    await drain({
      model: cacheReportingModel(),
      usageEmitter: capturingEmitter(captured),
      logger: silentLogger,
    });

    const byMeter = Object.fromEntries(captured.map((e) => [e.meterName, e.value]));
    expect(byMeter[ASSISTANT_METERS.inputTokens]).toBe('100');
    expect(byMeter[ASSISTANT_METERS.outputTokens]).toBe('30');
    expect(byMeter[ASSISTANT_METERS.cacheReadTokens]).toBe('20');
    expect(byMeter[ASSISTANT_METERS.cacheWriteTokens]).toBe('15');
  });
});
