// StreamCo demo server — the "provider side" of the AI Agent Framework
// local slice (CONTRACT.md, "Demo provider").
//
// One process serves:
//   POST /mcp                        Streamable HTTP MCP (stateless mode)
//   GET  /llms-full.txt              Tier-1 knowledge: overview + concepts
//   GET  /runbooks/lag.md            Tier-1 knowledge: troubleshooting runbook
//   GET  /.well-known/agent-card.json  STRETCH: static A2A agent card
//   GET  /healthz                    liveness for scripts
//
// Stateless MCP: a fresh McpServer + transport per request
// (sessionIdGenerator: undefined). No session state is needed for canned
// tools, and it keeps the server robust against client crashes. GET/DELETE
// /mcp return 405 per the Streamable HTTP spec for servers that don't offer
// a standalone SSE stream.
//
// Run: node src/server.ts   (Node >= 22.18, native type stripping)

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { z } from 'zod';

import { deleteStream, diagnose, getStream, streamSummaries } from './data.ts';

const HOST = process.env.STREAMCO_HOST ?? '127.0.0.1';
const PORT = Number(process.env.STREAMCO_PORT ?? 7810);

const KNOWLEDGE_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'knowledge');

// Knowledge payloads are small and immutable for the demo; read once.
const KNOWLEDGE_ROUTES: Record<string, { file: string; contentType: string }> = {
  '/llms-full.txt': { file: 'llms-full.txt', contentType: 'text/plain; charset=utf-8' },
  '/runbooks/lag.md': { file: join('runbooks', 'lag.md'), contentType: 'text/markdown; charset=utf-8' },
  '/.well-known/agent-card.json': { file: 'agent-card.json', contentType: 'application/json; charset=utf-8' },
};
const knowledge = new Map<string, { body: string; contentType: string }>(
  Object.entries(KNOWLEDGE_ROUTES).map(([route, { file, contentType }]) => [
    route,
    { body: readFileSync(join(KNOWLEDGE_DIR, file), 'utf8'), contentType },
  ]),
);

function jsonResult(value: unknown) {
  return {
    content: [{ type: 'text' as const, text: JSON.stringify(value, null, 2) }],
  };
}

function errorResult(message: string) {
  return {
    isError: true,
    content: [{ type: 'text' as const, text: message }],
  };
}

function buildMcpServer(): McpServer {
  const server = new McpServer({ name: 'streamco-mcp', version: '0.1.0' });

  server.registerTool(
    'streams_list',
    {
      title: 'List streams',
      description:
        'List all StreamCo streams with id, name, status (healthy|degraded) and current ingest rate (rps).',
      inputSchema: {},
    },
    async () => {
      log('tools/call streams_list');
      return jsonResult(streamSummaries());
    },
  );

  server.registerTool(
    'streams_get',
    {
      title: 'Get stream detail',
      description:
        'Get full detail for one stream by id (e.g. "s-2"): name, status, rps, region, lagSeconds.',
      inputSchema: { id: z.string().describe('Stream id, e.g. "s-1"') },
    },
    async ({ id }) => {
      log(`tools/call streams_get id=${id}`);
      const stream = getStream(id);
      if (!stream) {
        return errorResult(`Unknown stream id ${JSON.stringify(id)}. Known ids: s-1, s-2, s-3.`);
      }
      return jsonResult(stream);
    },
  );

  server.registerTool(
    'pipeline_diagnose',
    {
      title: 'Diagnose pipeline',
      description:
        'Run diagnostics for a StreamCo pipeline by id (e.g. "p-1"). Returns findings (severity, code, message) and a remediation recommendation. Read-only.',
      inputSchema: { id: z.string().describe('Pipeline id, e.g. "p-1"') },
    },
    async ({ id }) => {
      log(`tools/call pipeline_diagnose id=${id}`);
      const result = diagnose(id);
      if (!result) {
        return errorResult(`Unknown pipeline id ${JSON.stringify(id)}. Known ids: p-1, p-2.`);
      }
      return jsonResult(result);
    },
  );

  // Deliberately DESTRUCTIVE-looking 4th tool. StreamCo exposes it, but the
  // gateway MCPRoute toolSelector EXCLUDES it — the allow-list proof (gateway
  // slice) is that it is reachable directly yet absent/blocked through the
  // gateway. Demo no-op (see data.ts deleteStream).
  server.registerTool(
    'streams_delete',
    {
      title: 'Delete stream',
      description:
        'Delete a StreamCo stream by id. DESTRUCTIVE — excluded from the reviewed gateway allow-list. (Demo no-op: does not actually remove anything.)',
      inputSchema: { id: z.string().describe('Stream id to delete, e.g. "s-1"') },
    },
    async ({ id }) => {
      log(`tools/call streams_delete id=${id}`);
      const result = deleteStream(id);
      if (!result) {
        return errorResult(`Unknown stream id ${JSON.stringify(id)}. Known ids: s-1, s-2, s-3.`);
      }
      return jsonResult(result);
    },
  );

  return server;
}

async function handleMcp(req: IncomingMessage, res: ServerResponse): Promise<void> {
  if (req.method !== 'POST') {
    // Stateless server: no standalone SSE stream, no sessions to delete.
    res.writeHead(405, { 'content-type': 'application/json', allow: 'POST' }).end(
      JSON.stringify({
        jsonrpc: '2.0',
        error: { code: -32000, message: 'Method not allowed. This MCP endpoint is stateless; use POST.' },
        id: null,
      }),
    );
    return;
  }

  const server = buildMcpServer();
  const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
  res.on('close', () => {
    void transport.close();
    void server.close();
  });
  await server.connect(transport);
  await transport.handleRequest(req, res);
}

function log(message: string): void {
  console.log(`[streamco] ${new Date().toISOString()} ${message}`);
}

const httpServer = createServer((req, res) => {
  const url = new URL(req.url ?? '/', `http://${req.headers.host ?? `${HOST}:${PORT}`}`);

  if (url.pathname === '/mcp') {
    handleMcp(req, res).catch((err: unknown) => {
      log(`ERROR handling /mcp: ${err instanceof Error ? err.stack ?? err.message : String(err)}`);
      if (!res.headersSent) {
        res.writeHead(500, { 'content-type': 'application/json' }).end(
          JSON.stringify({
            jsonrpc: '2.0',
            error: { code: -32603, message: 'Internal server error' },
            id: null,
          }),
        );
      } else {
        res.end();
      }
    });
    return;
  }

  if (req.method === 'GET' && url.pathname === '/healthz') {
    res.writeHead(200, { 'content-type': 'text/plain' }).end('ok\n');
    return;
  }

  const doc = knowledge.get(url.pathname);
  if (doc) {
    if (req.method !== 'GET' && req.method !== 'HEAD') {
      res.writeHead(405, { allow: 'GET, HEAD' }).end();
      return;
    }
    log(`GET ${url.pathname}`);
    res.writeHead(200, { 'content-type': doc.contentType });
    res.end(req.method === 'HEAD' ? undefined : doc.body);
    return;
  }

  res.writeHead(404, { 'content-type': 'text/plain' }).end('not found\n');
});

httpServer.listen(PORT, HOST, () => {
  log(`listening on http://${HOST}:${PORT} (MCP at /mcp; knowledge at /llms-full.txt, /runbooks/lag.md, /.well-known/agent-card.json)`);
});

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    log(`received ${signal}, shutting down`);
    httpServer.close(() => process.exit(0));
    // Fallback if connections linger.
    setTimeout(() => process.exit(0), 1500).unref();
  });
}
