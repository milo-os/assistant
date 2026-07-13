import { buildAssistantUsageEvents } from './assistant-events';
import { createUsageEmitter } from './emitter';
import type { UsageEvent } from './types';
import { describe, expect, it } from 'bun:test';

function sampleEvents(): UsageEvent[] {
  return buildAssistantUsageEvents({
    projectName: 'demo-project',
    conversationId: 'conv-1',
    model: 'patch-mock-v0',
    tokens: { inputTokens: 10, outputTokens: 5 },
  });
}

describe('createUsageEmitter', () => {
  it('is a no-op when no gateway URL is configured', async () => {
    const emitter = createUsageEmitter({ source: 'http://svc/a2a' });
    const result = await emitter.emit(sampleEvents());
    expect(result.noop).toBe(true);
    expect(result.ok).toBe(true);
  });

  it('POSTs a JSON array of CloudEvents to <gateway>/cloudevents with the api-key', async () => {
    const captured: { url?: string; headers?: Headers; body?: unknown } = {};
    const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
      captured.url = String(input);
      captured.headers = new Headers(init?.headers);
      captured.body = JSON.parse(String(init?.body));
      return new Response('', { status: 202 });
    }) as typeof fetch;

    const emitter = createUsageEmitter({
      gatewayUrl: 'http://collector:8080',
      apiKey: 'secret-key',
      source: 'http://svc/a2a',
      fetchImpl,
    });
    const result = await emitter.emit(sampleEvents());

    expect(result.ok).toBe(true);
    expect(result.noop).toBe(false);
    expect(result.status).toBe(202);
    expect(captured.url).toBe('http://collector:8080/cloudevents');
    expect(captured.headers?.get('x-api-key')).toBe('secret-key');
    expect(captured.headers?.get('content-type')).toBe('application/json');
    expect(Array.isArray(captured.body)).toBe(true);
    const body = captured.body as Array<Record<string, unknown>>;
    expect(body[0]).toMatchObject({
      specversion: '1.0',
      subject: 'projects/demo-project',
      datacontenttype: 'application/json',
    });
  });

  it('never throws and reports ok:false when the gateway errors', async () => {
    const fetchImpl = (async () => new Response('boom', { status: 500 })) as unknown as typeof fetch;
    const emitter = createUsageEmitter({
      gatewayUrl: 'http://collector:8080',
      source: 'http://svc/a2a',
      fetchImpl,
    });
    const result = await emitter.emit(sampleEvents());
    expect(result.ok).toBe(false);
    expect(result.status).toBe(500);
  });

  it('never throws when fetch itself rejects', async () => {
    const fetchImpl = (async () => {
      throw new Error('network down');
    }) as unknown as typeof fetch;
    const emitter = createUsageEmitter({
      gatewayUrl: 'http://collector:8080',
      source: 'http://svc/a2a',
      fetchImpl,
    });
    const result = await emitter.emit(sampleEvents());
    expect(result.ok).toBe(false);
    expect(result.error).toContain('network down');
  });

  it('returns ok with count 0 for an empty event list', async () => {
    const emitter = createUsageEmitter({ gatewayUrl: 'http://c', source: 's' });
    const result = await emitter.emit([]);
    expect(result).toEqual({ ok: true, noop: false, count: 0 });
  });
});
