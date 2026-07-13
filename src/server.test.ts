// src/server.test.ts
//
// End-to-end HTTP integration test driven through the real Hono app
// (app.request). Wires the whole graph — dev auth, fixture bindings, a
// REAL in-process MCP server (Streamable HTTP), the scripted mock model,
// and an in-process usage sink — and asserts the contract's Definition
// of Done items (b) full message/send with tool result reaching the
// final text + usage events, and (d) the message/stream SSE path, plus
// the agent card and auth (401/403/200) surfaces.
import { buildApp } from './server';
import { loadConfig } from './config';
import { silentLogger } from './logger';
import type { CloudEvent } from './usage';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createServer, type Server } from 'node:http';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Hono } from 'hono';

// ── In-process StreamCo-like MCP server + knowledge doc ───────

interface JsonRpcRequest {
  jsonrpc: '2.0';
  id?: number | string;
  method: string;
  params?: Record<string, unknown>;
}

const MCP_TOOL_CALLS: Array<{ name: string; args: unknown }> = [];

function handleMcp(message: JsonRpcRequest): { status: number; body?: unknown } {
  switch (message.method) {
    case 'initialize':
      return {
        status: 200,
        body: {
          jsonrpc: '2.0',
          id: message.id,
          result: {
            protocolVersion: (message.params as { protocolVersion?: string })?.protocolVersion,
            capabilities: { tools: {} },
            serverInfo: { name: 'fake-streamco', version: '1.0.0' },
          },
        },
      };
    case 'notifications/initialized':
      return { status: 200 };
    case 'tools/list':
      return {
        status: 200,
        body: {
          jsonrpc: '2.0',
          id: message.id,
          result: {
            tools: [
              {
                name: 'streams_list',
                description: 'List streams',
                inputSchema: { type: 'object', properties: {} },
              },
              {
                name: 'pipeline_diagnose',
                description: 'Diagnose a pipeline',
                inputSchema: {
                  type: 'object',
                  properties: { id: { type: 'string' } },
                  required: ['id'],
                },
              },
            ],
          },
        },
      };
    case 'tools/call': {
      const params = message.params as { name: string; arguments?: unknown };
      MCP_TOOL_CALLS.push({ name: params.name, args: params.arguments });
      return {
        status: 200,
        body: {
          jsonrpc: '2.0',
          id: message.id,
          result: {
            content: [
              {
                type: 'text',
                text: JSON.stringify({
                  id: (params.arguments as { id?: string })?.id,
                  findings: ['CONSUMER_LAG'],
                }),
              },
            ],
          },
        },
      };
    }
    default:
      return { status: 200, body: { jsonrpc: '2.0', id: message.id, result: {} } };
  }
}

// ── Usage sink ────────────────────────────────────────────────

const SINK_EVENTS: CloudEvent[] = [];

// ── Shared fixtures ───────────────────────────────────────────

let providerServer: Server;
let sinkServer: Server;
let providerPort: number;
let sinkPort: number;
let app: Hono;
let tmpDir: string;

const GOOD_TOKEN = 'good';
const WRONG_TOKEN = 'wrong';
const PROJECT = 'demo-project';

beforeAll(async () => {
  // Provider server: /mcp (Streamable HTTP MCP) + /llms-full.txt (knowledge).
  providerServer = createServer((req, res) => {
    if (req.url === '/llms-full.txt') {
      res.writeHead(200, { 'content-type': 'text/plain' }).end('StreamCo streams video at the edge.');
      return;
    }
    if (req.url !== '/mcp') {
      res.writeHead(404).end('not found');
      return;
    }
    if (req.method !== 'POST') {
      res.writeHead(405).end('method not allowed');
      return;
    }
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const message = JSON.parse(Buffer.concat(chunks).toString('utf8')) as JsonRpcRequest;
      const { status, body } = handleMcp(message);
      if (body === undefined) res.writeHead(status).end();
      else res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
    });
  });
  await new Promise<void>((r) => providerServer.listen(0, '127.0.0.1', r));
  providerPort = (providerServer.address() as { port: number }).port;

  // Usage sink: records every CloudEvent posted to /cloudevents.
  sinkServer = createServer((req, res) => {
    if (req.method === 'POST' && req.url === '/cloudevents') {
      const chunks: Buffer[] = [];
      req.on('data', (c: Buffer) => chunks.push(c));
      req.on('end', () => {
        try {
          const batch = JSON.parse(Buffer.concat(chunks).toString('utf8')) as CloudEvent[];
          SINK_EVENTS.push(...batch);
        } catch {
          /* ignore */
        }
        res.writeHead(202).end();
      });
      return;
    }
    res.writeHead(404).end();
  });
  await new Promise<void>((r) => sinkServer.listen(0, '127.0.0.1', r));
  sinkPort = (sinkServer.address() as { port: number }).port;

  // Bindings fixture pointing at the in-process provider server.
  tmpDir = mkdtempSync(join(tmpdir(), 'assistant-e2e-'));
  const fixture = [
    {
      apiVersion: 'services.miloapis.com/v1alpha1',
      kind: 'AgentBinding',
      metadata: { name: 'streamco-binding', namespace: PROJECT },
      spec: {
        serviceRef: { name: 'streamco' },
        serviceName: 'streaming.streamco.example',
        serviceAgentRef: { name: 'streamco-agent' },
        configurationVersion: 'v1',
        knowledge: {
          sources: [
            {
              type: 'LLMDocs',
              title: 'StreamCo overview',
              url: `http://127.0.0.1:${providerPort}/llms-full.txt`,
            },
          ],
          concepts: [
            {
              gvk: { group: 'streaming.streamco.example', kind: 'Stream' },
              summary: 'A live media stream',
            },
          ],
        },
        tools: {
          mcpServers: [
            {
              name: 'streamco',
              endpoint: `http://127.0.0.1:${providerPort}/mcp`,
              toolSelector: { include: ['streams_list', 'pipeline_diagnose'] },
              mutating: [],
            },
          ],
        },
      },
    },
  ];
  const fixturePath = join(tmpDir, 'bindings.json');
  writeFileSync(fixturePath, JSON.stringify(fixture));

  const config = loadConfig({
    AUTH_MODE: 'dev',
    AUTH_DEV_TOKENS: `${GOOD_TOKEN}:alice:${PROJECT};${WRONG_TOKEN}:bob:other-project`,
    MODEL_MODE: 'mock',
    AGENT_BINDINGS_FIXTURE: fixturePath,
    USAGE_GATEWAY_URL: `http://127.0.0.1:${sinkPort}`,
    PUBLIC_BASE_URL: 'http://assistant.test',
  });
  app = buildApp(config, silentLogger).app;
});

afterAll(async () => {
  await new Promise<void>((r) => providerServer.close(() => r()));
  await new Promise<void>((r) => sinkServer.close(() => r()));
  rmSync(tmpDir, { recursive: true, force: true });
});

// ── Helpers ───────────────────────────────────────────────────

async function rpc(
  token: string | undefined,
  method: string,
  params: unknown,
  id: number | string = 1
): Promise<Response> {
  const headers: Record<string, string> = { 'content-type': 'application/json' };
  if (token) headers.authorization = `Bearer ${token}`;
  return app.request('/a2a', {
    method: 'POST',
    headers,
    body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
  });
}

function diagnoseMessage(project = PROJECT) {
  return {
    message: {
      kind: 'message',
      role: 'user',
      parts: [{ kind: 'text', text: 'Diagnose pipeline p-1 for StreamCo' }],
      messageId: 'm-1',
      metadata: { projectName: project },
    },
  };
}

function parseSse(text: string): Array<Record<string, unknown>> {
  return text
    .split('\n\n')
    .map((block) => block.trim())
    .filter((block) => block.startsWith('data:'))
    .map((block) => JSON.parse(block.slice('data:'.length).trim()) as Record<string, unknown>);
}

// ── Health + card ─────────────────────────────────────────────

describe('GET /healthz', () => {
  it('returns {status:"ok"}', async () => {
    const res = await app.request('/healthz');
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ status: 'ok' });
  });
});

describe('GET /.well-known/agent-card.json', () => {
  it('serves a valid A2A v1.0 agent card', async () => {
    const res = await app.request('/.well-known/agent-card.json');
    expect(res.status).toBe(200);
    const card = (await res.json()) as Record<string, any>;
    expect(card.protocolVersion).toBe('1.0');
    expect(card.name).toBe('Patch');
    expect(card.provider.organization).toBe('Datum');
    expect(card.url).toBe('http://assistant.test/a2a');
    expect(card.capabilities.streaming).toBe(true);
    expect(card.capabilities.pushNotifications).toBe(false);
    expect(card.securitySchemes.bearer).toMatchObject({ type: 'http', scheme: 'bearer' });
    expect(card.skills.map((s: { id: string }) => s.id)).toContain('project-assistant');
  });
});

// ── Auth ──────────────────────────────────────────────────────

describe('POST /a2a auth', () => {
  it('401 when no bearer token is present', async () => {
    const res = await rpc(undefined, 'message/send', diagnoseMessage());
    expect(res.status).toBe(401);
    expect(res.headers.get('www-authenticate')).toBe('Bearer');
  });

  it('401 for an unknown token', async () => {
    const res = await rpc('nope', 'message/send', diagnoseMessage());
    expect(res.status).toBe(401);
  });

  it('403 when the token does not grant the requested project', async () => {
    const res = await rpc(WRONG_TOKEN, 'message/send', diagnoseMessage());
    expect(res.status).toBe(403);
  });

  it('200 for a good token on a granted project', async () => {
    const res = await rpc(GOOD_TOKEN, 'message/send', diagnoseMessage());
    expect(res.status).toBe(200);
  });
});

// ── message/send full chat path ───────────────────────────────

describe('message/send (mock model + fixture bindings)', () => {
  it('runs the tool over real MCP, folds the result into the final text, and meters usage', async () => {
    MCP_TOOL_CALLS.length = 0;
    SINK_EVENTS.length = 0;

    const res = await rpc(GOOD_TOKEN, 'message/send', diagnoseMessage(), 'send-1');
    expect(res.status).toBe(200);
    const body = (await res.json()) as { result: any; error?: unknown };
    expect(body.error).toBeUndefined();

    const task = body.result;
    expect(task.kind).toBe('task');
    expect(task.status.state).toBe('completed');

    // The provider tool actually executed over the MCP wire.
    expect(MCP_TOOL_CALLS).toEqual([{ name: 'pipeline_diagnose', args: { id: 'p-1' } }]);

    // The tool findings reached the final text (artifact + status message).
    const artifactText = task.artifacts[0].parts[0].text as string;
    expect(artifactText).toContain('CONSUMER_LAG');
    expect(task.status.message.parts[0].text).toContain('CONSUMER_LAG');

    // Usage events landed at the sink: token meters + one tool-invocation.
    const types = SINK_EVENTS.map((e) => e.type);
    expect(types).toContain('assistant.miloapis.com/conversation/input-tokens');
    expect(types).toContain('assistant.miloapis.com/conversation/output-tokens');
    expect(types).toContain('assistant.miloapis.com/conversation/messages');

    // REGRESSION (billing under-count): a diagnose turn runs TWO model
    // steps — the tool-call step and the answer step — each reporting the
    // mock's 42 in / 23 out. Billing must be the SUM across steps
    // (result.totalUsage), not just the final step (result.usage). If this
    // reads the last step only, these would be 42 / 23 and the tool-call
    // step's tokens would go unbilled.
    const inputEvent = SINK_EVENTS.find(
      (e) => e.type === 'assistant.miloapis.com/conversation/input-tokens'
    );
    const outputEvent = SINK_EVENTS.find(
      (e) => e.type === 'assistant.miloapis.com/conversation/output-tokens'
    );
    expect(inputEvent!.data.value).toBe('84'); // 42 + 42
    expect(outputEvent!.data.value).toBe('46'); // 23 + 23

    const toolEvent = SINK_EVENTS.find(
      (e) => e.type === 'assistant.miloapis.com/conversation/tool-invocations'
    );
    expect(toolEvent).toBeDefined();
    expect(toolEvent!.subject).toBe(`projects/${PROJECT}`);
    expect(toolEvent!.data.dimensions).toEqual({ service: 'streaming.streamco.example' });
    expect(toolEvent!.data.resource).toMatchObject({
      group: 'assistant.miloapis.com',
      kind: 'Conversation',
      name: task.contextId,
    });

    // Every sink event is subject-scoped to the project.
    for (const event of SINK_EVENTS) {
      expect(event.subject).toBe(`projects/${PROJECT}`);
    }
  });

  it('retrieves the finished task via tasks/get', async () => {
    const sent = await rpc(GOOD_TOKEN, 'message/send', diagnoseMessage(), 'send-2');
    const task = ((await sent.json()) as { result: any }).result;

    const got = await rpc(GOOD_TOKEN, 'tasks/get', { id: task.id }, 'get-1');
    const gotBody = (await got.json()) as { result: any };
    expect(gotBody.result.id).toBe(task.id);
    expect(gotBody.result.status.state).toBe('completed');
  });

  it('rejects tasks/get for a task in a project the token does not grant (403)', async () => {
    const sent = await rpc(GOOD_TOKEN, 'message/send', diagnoseMessage(), 'send-3');
    const task = ((await sent.json()) as { result: any }).result;

    const got = await rpc(WRONG_TOKEN, 'tasks/get', { id: task.id }, 'get-2');
    expect(got.status).toBe(403);
  });

  it('returns a generic mock reply when the user does not ask to diagnose', async () => {
    const res = await rpc(
      GOOD_TOKEN,
      'message/send',
      {
        message: {
          kind: 'message',
          role: 'user',
          parts: [{ kind: 'text', text: 'hello there' }],
          messageId: 'm-hi',
          metadata: { projectName: PROJECT },
        },
      },
      'send-hi'
    );
    const task = ((await res.json()) as { result: any }).result;
    expect(task.status.state).toBe('completed');
    expect(task.artifacts[0].parts[0].text.toLowerCase()).toContain('patch');
  });
});

// ── message/stream SSE ────────────────────────────────────────

describe('message/stream (SSE)', () => {
  it('streams working → artifact(text) → completed(final) and quotes the findings', async () => {
    MCP_TOOL_CALLS.length = 0;
    const res = await rpc(GOOD_TOKEN, 'message/stream', diagnoseMessage(), 'stream-1');
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toContain('text/event-stream');

    const frames = parseSse(await res.text());
    const events = frames.map((f) => f.result as Record<string, any>);

    // First frame: the initial Task.
    expect(events[0]?.kind).toBe('task');

    const states = events
      .filter((e) => e.kind === 'status-update')
      .map((e) => e.status.state);
    expect(states).toContain('working');
    expect(states).toContain('completed');

    // Terminal status-update is marked final.
    const terminal = events.find((e) => e.kind === 'status-update' && e.status.state === 'completed');
    expect(terminal!.final).toBe(true);

    // The artifact text (concatenated across chunks) mentions the findings.
    const artifactText = events
      .filter((e) => e.kind === 'artifact-update')
      .flatMap((e) => e.artifact.parts as Array<{ text: string }>)
      .map((p) => p.text)
      .join('');
    expect(artifactText).toContain('CONSUMER_LAG');

    // And the tool really ran over MCP for the streamed request too.
    expect(MCP_TOOL_CALLS).toEqual([{ name: 'pipeline_diagnose', args: { id: 'p-1' } }]);
  });
});

// ── tasks/cancel ──────────────────────────────────────────────

describe('tasks/cancel', () => {
  it('reports a terminal task as not cancelable', async () => {
    const sent = await rpc(GOOD_TOKEN, 'message/send', diagnoseMessage(), 'send-4');
    const task = ((await sent.json()) as { result: any }).result;

    const res = await rpc(GOOD_TOKEN, 'tasks/cancel', { id: task.id }, 'cancel-1');
    const body = (await res.json()) as { error?: { code: number } };
    expect(body.error?.code).toBe(-32002); // A2A TaskNotCancelable
  });

  it('returns a task-not-found error for an unknown task id', async () => {
    const res = await rpc(GOOD_TOKEN, 'tasks/cancel', { id: 'does-not-exist' }, 'cancel-2');
    const body = (await res.json()) as { error?: { code: number } };
    expect(body.error?.code).toBe(-32001); // A2A TaskNotFound
  });
});

// ── JSON-RPC framing ──────────────────────────────────────────

describe('JSON-RPC framing', () => {
  it('returns method-not-found for an unknown method', async () => {
    const res = await rpc(GOOD_TOKEN, 'does/notexist', {}, 'x-1');
    const body = (await res.json()) as { error?: { code: number } };
    expect(body.error?.code).toBe(-32601);
  });

  it('returns invalid-params when projectName is missing', async () => {
    const res = await rpc(
      GOOD_TOKEN,
      'message/send',
      {
        message: {
          kind: 'message',
          role: 'user',
          parts: [{ kind: 'text', text: 'hi' }],
          messageId: 'm-x',
        },
      },
      'x-2'
    );
    const body = (await res.json()) as { error?: { code: number } };
    expect(body.error?.code).toBe(-32602);
  });
});
