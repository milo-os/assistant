// src/agent/gateway-mode.test.ts
//
// Proves the MODEL_MODE=gateway seam end to end WITHOUT a real gateway: an
// in-process fake OpenAI-compatible chat-completions server (streaming SSE
// + usage) stands in for "gateway → stub LLM", recording the request
// headers. We drive a real message/send through the Hono app and assert:
//   - the x-datum-* attribution headers reached the model call
//   - NO Authorization / credential was sent by the service
//   - the answer streamed back and usage events were built from the stub's
//     usage numbers (the gateway-counted-tokens cross-check, in-process)
// The real gateway/MCPRoute/credential-injection proofs are QA's e2e leg.
import { buildApp } from '../server';
import { loadConfig } from '../config';
import { silentLogger } from '../logger';
import type { CloudEvent } from '../usage';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createServer, type Server } from 'node:http';
import type { Hono } from 'hono';

let capturedHeaders: Record<string, string | string[] | undefined> = {};
let capturedBody: { model?: string; stream?: boolean } = {};
const SINK_EVENTS: CloudEvent[] = [];

function chatCompletionSse(): string {
  const base = { id: 'cmpl-1', object: 'chat.completion.chunk', created: 0, model: 'patch-stub-v1' };
  const frames = [
    { ...base, choices: [{ index: 0, delta: { role: 'assistant', content: '' }, finish_reason: null }] },
    { ...base, choices: [{ index: 0, delta: { content: 'All clear — provider says OK for p-1.' }, finish_reason: null }] },
    { ...base, choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] },
    { ...base, choices: [], usage: { prompt_tokens: 11, completion_tokens: 7, total_tokens: 18 } },
  ];
  return frames.map((f) => `data: ${JSON.stringify(f)}\n\n`).join('') + 'data: [DONE]\n\n';
}

let gateway: Server;
let sink: Server;
let app: Hono;

const TOKEN = 'good';
const PROJECT = 'demo-project';

beforeAll(async () => {
  // Fake "Envoy AI Gateway → stub LLM": OpenAI-compatible chat-completions.
  gateway = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      capturedHeaders = req.headers;
      try {
        capturedBody = JSON.parse(Buffer.concat(chunks).toString('utf8'));
      } catch {
        capturedBody = {};
      }
      res
        .writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' })
        .end(chatCompletionSse());
    });
  });
  await new Promise<void>((r) => gateway.listen(0, '127.0.0.1', r));
  const gatewayPort = (gateway.address() as { port: number }).port;

  // Usage sink.
  sink = createServer((req, res) => {
    if (req.method === 'POST' && req.url === '/cloudevents') {
      const chunks: Buffer[] = [];
      req.on('data', (c: Buffer) => chunks.push(c));
      req.on('end', () => {
        try {
          SINK_EVENTS.push(...(JSON.parse(Buffer.concat(chunks).toString('utf8')) as CloudEvent[]));
        } catch {
          /* ignore */
        }
        res.writeHead(202).end();
      });
      return;
    }
    res.writeHead(404).end();
  });
  await new Promise<void>((r) => sink.listen(0, '127.0.0.1', r));
  const sinkPort = (sink.address() as { port: number }).port;

  const config = loadConfig({
    AUTH_MODE: 'dev',
    AUTH_DEV_TOKENS: `${TOKEN}:alice:${PROJECT}`,
    MODEL_MODE: 'gateway',
    GATEWAY_URL: `http://127.0.0.1:${gatewayPort}`,
    // deliberately NO ANTHROPIC_API_KEY — credential isolation
    USAGE_GATEWAY_URL: `http://127.0.0.1:${sinkPort}`,
    PUBLIC_BASE_URL: 'http://patch.test',
  });
  app = buildApp(config, silentLogger).app;
});

afterAll(async () => {
  await new Promise<void>((r) => gateway.close(() => r()));
  await new Promise<void>((r) => sink.close(() => r()));
});

async function messageSend(text: string, project = PROJECT): Promise<Response> {
  return app.fetch(
    new Request('http://patch.test/a2a', {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: `Bearer ${TOKEN}` },
      body: JSON.stringify({
        jsonrpc: '2.0',
        id: 1,
        method: 'message/send',
        params: {
          message: {
            kind: 'message',
            role: 'user',
            messageId: 'm1',
            parts: [{ kind: 'text', text }],
            metadata: { projectName: project },
          },
        },
      }),
    })
  );
}

describe('MODEL_MODE=gateway seam', () => {
  it('routes the model call through the gateway endpoint with the requested model', async () => {
    SINK_EVENTS.length = 0;
    const res = await messageSend('Diagnose pipeline p-1 for StreamCo');
    expect(res.status).toBe(200);
    const task = ((await res.json()) as { result: { status: { state: string }; artifacts?: Array<{ parts: Array<{ text: string }> }> } }).result;

    expect(task.status.state).toBe('completed');
    expect(capturedBody.model).toBe('patch-stub-v1');
    // The assistant's answer (from the gateway/stub) reached the artifact.
    expect(task.artifacts?.[0]?.parts[0]?.text).toContain('provider says OK');
  });

  it('attaches the x-datum-* attribution headers to the model call', async () => {
    await messageSend('hello there');
    expect(capturedHeaders['x-datum-project']).toBe(PROJECT);
    expect(capturedHeaders['x-datum-agent']).toBe('patch');
    // contextId is a generated uuid; just assert it is present + non-empty.
    expect(typeof capturedHeaders['x-datum-conversation']).toBe('string');
    expect((capturedHeaders['x-datum-conversation'] as string).length).toBeGreaterThan(0);
  });

  it('sends NO upstream credential (no Authorization header) in gateway mode', async () => {
    await messageSend('hello again');
    expect(capturedHeaders['authorization']).toBeUndefined();
  });

  it('builds usage events from the stub usage numbers (gateway-counted cross-check)', async () => {
    SINK_EVENTS.length = 0;
    await messageSend('diagnose p-1');
    const input = SINK_EVENTS.find((e) => e.type === 'assistant.miloapis.com/conversation/input-tokens');
    const output = SINK_EVENTS.find((e) => e.type === 'assistant.miloapis.com/conversation/output-tokens');
    expect(input?.data.value).toBe('11');
    expect(output?.data.value).toBe('7');
    expect(input?.subject).toBe(`projects/${PROJECT}`);
    expect(input?.data.dimensions).toEqual({ model: 'patch-stub-v1' });
  });
});
