// cli/integration.test.ts
//
// Drives the `patch` CLI's main() against an in-process service instance
// (buildApp + mock model + a REAL StreamCo MCP server + fixture bindings)
// via an injected fetch. Proves the CLI end to end: the diagnose chat
// streams the tool findings to stdout with exit 0, `card` works, and auth
// failures exit non-zero with a clear message.
import { A2AClient } from '../src/a2a/client';
import { buildApp } from '../src/server';
import { loadConfig } from '../src/config';
import { silentLogger } from '../src/logger';
import type { Io } from './render';
import { main } from './main';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createServer, type Server } from 'node:http';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Hono } from 'hono';

const MCP_TOOL_CALLS: Array<{ name: string; args: unknown }> = [];

function handleMcp(m: { id?: unknown; method: string; params?: { arguments?: { id?: string } } }) {
  switch (m.method) {
    case 'initialize':
      return { jsonrpc: '2.0', id: m.id, result: { protocolVersion: '2025-06-18', capabilities: { tools: {} }, serverInfo: { name: 'streamco', version: '1' } } };
    case 'notifications/initialized':
      return undefined;
    case 'tools/list':
      return {
        jsonrpc: '2.0',
        id: m.id,
        result: {
          tools: [
            { name: 'streams_list', description: 'List', inputSchema: { type: 'object', properties: {} } },
            { name: 'pipeline_diagnose', description: 'Diagnose', inputSchema: { type: 'object', properties: { id: { type: 'string' } }, required: ['id'] } },
          ],
        },
      };
    case 'tools/call':
      MCP_TOOL_CALLS.push({ name: 'pipeline_diagnose', args: m.params?.arguments });
      return { jsonrpc: '2.0', id: m.id, result: { content: [{ type: 'text', text: JSON.stringify({ id: m.params?.arguments?.id, findings: ['CONSUMER_LAG'] }) }] } };
    default:
      return { jsonrpc: '2.0', id: m.id, result: {} };
  }
}

let mcp: Server;
let app: Hono;
let tmpDir: string;
let fetchImpl: typeof fetch;

const ENV = { PATCH_URL: 'http://patch.test', PATCH_TOKEN: 'good' };

function captureIo(): { io: Io; out: () => string; err: () => string } {
  const o: string[] = [];
  const e: string[] = [];
  return { io: { out: (t) => o.push(t), err: (t) => e.push(t) }, out: () => o.join(''), err: () => e.join('') };
}

beforeAll(async () => {
  mcp = createServer((req, res) => {
    if (req.url !== '/mcp' || req.method !== 'POST') { res.writeHead(404).end(); return; }
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const body = handleMcp(JSON.parse(Buffer.concat(chunks).toString()));
      if (body === undefined) res.writeHead(200).end();
      else res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(body));
    });
  });
  await new Promise<void>((r) => mcp.listen(0, '127.0.0.1', r));
  const port = (mcp.address() as { port: number }).port;

  tmpDir = mkdtempSync(join(tmpdir(), 'cli-itest-'));
  const fixturePath = join(tmpDir, 'bindings.json');
  writeFileSync(
    fixturePath,
    JSON.stringify([
      {
        spec: {
          serviceRef: { name: 'streamco' },
          serviceName: 'streaming.streamco.example',
          serviceAgentRef: { name: 'a' },
          configurationVersion: 'v1',
          tools: { mcpServers: [{ name: 'streamco', endpoint: `http://127.0.0.1:${port}/mcp`, toolSelector: { include: ['streams_list', 'pipeline_diagnose'] } }] },
        },
      },
    ])
  );

  const config = loadConfig({
    AUTH_MODE: 'dev',
    AUTH_DEV_TOKENS: 'good:alice:demo-project;wrong:bob:other-project',
    MODEL_MODE: 'mock',
    AGENT_BINDINGS_FIXTURE: fixturePath,
    PUBLIC_BASE_URL: 'http://patch.test',
  });
  app = buildApp(config, silentLogger).app;
  fetchImpl = ((input: string | URL | Request, init?: RequestInit) =>
    app.fetch(new Request(input as string, init))) as typeof fetch;
});

afterAll(async () => {
  await new Promise<void>((r) => mcp.close(() => r()));
  rmSync(tmpDir, { recursive: true, force: true });
});

describe('patch CLI (in-process service)', () => {
  it('chat: streams the tool findings to stdout and exits 0', async () => {
    MCP_TOOL_CALLS.length = 0;
    const { io, out, err } = captureIo();
    const code = await main(
      ['chat', 'Diagnose pipeline p-1 for StreamCo', '--project', 'demo-project'],
      ENV,
      io,
      { fetchImpl }
    );
    expect(code).toBe(0);
    expect(out()).toContain('CONSUMER_LAG');
    expect(err()).toContain('completed');
    // The tool actually ran over MCP.
    expect(MCP_TOOL_CALLS).toEqual([{ name: 'pipeline_diagnose', args: { id: 'p-1' } }]);
  });

  it('card: prints the agent card', async () => {
    const { io, out } = captureIo();
    const code = await main(['card'], ENV, io, { fetchImpl });
    expect(code).toBe(0);
    expect(out()).toContain('Patch');
    expect(out()).toContain('http://patch.test/a2a');
  });

  it('chat: no token → exit 1 with an unauthorized message', async () => {
    const { io, err } = captureIo();
    const code = await main(
      ['chat', 'Diagnose pipeline p-1 for StreamCo', '--project', 'demo-project'],
      { PATCH_URL: ENV.PATCH_URL },
      io,
      { fetchImpl }
    );
    expect(code).toBe(1);
    expect(err().toLowerCase()).toContain('unauthorized');
  });

  it('chat: wrong-project token → exit 1 with a forbidden message', async () => {
    const { io, err } = captureIo();
    const code = await main(
      ['chat', 'Diagnose pipeline p-1 for StreamCo', '--project', 'demo-project'],
      { PATCH_URL: ENV.PATCH_URL, PATCH_TOKEN: 'wrong' },
      io,
      { fetchImpl }
    );
    expect(code).toBe(1);
    expect(err().toLowerCase()).toContain('forbidden');
  });

  it('task get: fetches a completed task; cancel on it exits non-zero', async () => {
    const client = new A2AClient({ baseUrl: ENV.PATCH_URL, token: 'good', fetchImpl });
    const task = await client.messageSend(
      client.buildMessageParams('Diagnose pipeline p-1 for StreamCo', 'demo-project')
    );
    expect(task.status.state).toBe('completed');

    const getIo = captureIo();
    const getCode = await main(['task', 'get', task.id], ENV, getIo.io, { fetchImpl });
    expect(getCode).toBe(0);
    expect(getIo.out()).toContain(task.id);
    expect(getIo.out()).toContain('completed');

    const cancelIo = captureIo();
    const cancelCode = await main(['task', 'cancel', task.id], ENV, cancelIo.io, { fetchImpl });
    expect(cancelCode).toBe(1);
    expect(cancelIo.err()).toContain('cannot be canceled');
  });

  it('missing PATCH_URL → exit 2', async () => {
    const { io, err } = captureIo();
    const code = await main(['card'], {}, io, { fetchImpl });
    expect(code).toBe(2);
    expect(err()).toContain('PATCH_URL');
  });
});
