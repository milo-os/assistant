// Datum Compute MCP provider (prototype).
//
// The provider side of a compute AI plugin: the tools an assistant may call to
// diagnose Workloads, plus the knowledge and skills it reads. Modelled on
// e2e/streamco so it drops into the same playground wiring, but the tool
// surface and the reason catalog are real compute semantics taken from
// datum-cloud/compute api/v1alpha.
//
// One process serves:
//   POST /mcp                          Streamable HTTP MCP (stateless mode)
//   GET  /llms-full.txt                Knowledge: the compute resource model
//   GET  /runbooks/<name>.md           Skills: triage procedures
//   GET  /.well-known/agent-card.json  Static A2A agent card
//   GET  /healthz                      liveness
//
// Stateless MCP: a fresh McpServer + transport per request
// (sessionIdGenerator: undefined), matching the StreamCo provider.
//
// Run: node src/server.ts   (Node >= 22.18, native type stripping)

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { z } from 'zod';

import { allReasons, explainReason } from './catalog.ts';
import { diagnoseWorkload, fleetSummary } from './diagnose.ts';
import {
  deploymentsFor,
  getWorkload,
  instancesFor,
  workloadNames,
  type Instance,
} from './data.ts';

const HOST = process.env.COMPUTE_HOST ?? '127.0.0.1';
const PORT = Number(process.env.COMPUTE_PORT ?? 7830);

const KNOWLEDGE_DIR = join(dirname(fileURLToPath(import.meta.url)), '..', 'knowledge');

const RUNBOOKS = [
  'workload-not-available',
  'quota-triage',
  'instance-not-ready',
  'referenced-data-triage',
  'placement-triage',
] as const;

const KNOWLEDGE_ROUTES: Record<string, { file: string; contentType: string }> = {
  '/llms-full.txt': { file: 'llms-full.txt', contentType: 'text/plain; charset=utf-8' },
  '/.well-known/agent-card.json': {
    file: 'agent-card.json',
    contentType: 'application/json; charset=utf-8',
  },
  ...Object.fromEntries(
    RUNBOOKS.map((name) => [
      `/runbooks/${name}.md`,
      { file: join('runbooks', `${name}.md`), contentType: 'text/markdown; charset=utf-8' },
    ]),
  ),
};

const knowledge = new Map<string, { body: string; contentType: string }>(
  Object.entries(KNOWLEDGE_ROUTES).map(([route, { file, contentType }]) => [
    route,
    { body: readFileSync(join(KNOWLEDGE_DIR, file), 'utf8'), contentType },
  ]),
);

function jsonResult(value: unknown) {
  return { content: [{ type: 'text' as const, text: JSON.stringify(value, null, 2) }] };
}

function errorResult(message: string) {
  return { isError: true, content: [{ type: 'text' as const, text: message }] };
}

const unknownWorkload = (name: string) =>
  errorResult(
    `Unknown workload ${JSON.stringify(name)} in project demo-project. Known workloads: ${workloadNames().join(', ')}.`,
  );

/** Condition view used in tool output — the fields worth spending tokens on. */
function conditionView(i: Instance) {
  return {
    name: i.name,
    deployment: i.deployment,
    location: i.location,
    conditions: i.conditions.map((c) => ({
      type: c.type,
      status: c.status,
      reason: c.reason,
      message: c.message,
    })),
  };
}

function buildMcpServer(): McpServer {
  const server = new McpServer({ name: 'datum-compute-mcp', version: '0.1.0' });

  server.registerTool(
    'workloads_list',
    {
      title: 'List workloads',
      description:
        'List every Workload in the project with its availability, ready/desired replicas, and — when it is not fully available — the root-cause reason and whether that cause is user-actionable, a platform fault, or transient. Start here. Read-only.',
      inputSchema: {},
    },
    async () => {
      log('tools/call workloads_list');
      return jsonResult(fleetSummary());
    },
  );

  server.registerTool(
    'workloads_get',
    {
      title: 'Get workload detail',
      description:
        'Get one Workload by name with its full condition tree: workload conditions, its placements, every WorkloadDeployment, and every Instance with their conditions. Use when you need the raw status rather than a diagnosis. Read-only.',
      inputSchema: {
        name: z.string().describe('Workload name, e.g. "api-backend"'),
      },
    },
    async ({ name }) => {
      log(`tools/call workloads_get name=${name}`);
      const w = getWorkload(name);
      if (!w) return unknownWorkload(name);
      return jsonResult({
        workload: {
          name: w.name,
          namespace: w.namespace,
          image: w.image,
          desiredReplicas: w.desiredReplicas,
          replicas: w.replicas,
          readyReplicas: w.readyReplicas,
          creationTimestamp: w.creationTimestamp,
          conditions: w.conditions,
          placements: w.placements,
        },
        deployments: deploymentsFor(name),
        instances: instancesFor(name).map(conditionView),
      });
    },
  );

  server.registerTool(
    'instances_list',
    {
      title: 'List instances',
      description:
        "List Instances, optionally filtered to one workload, with each instance's conditions (Ready, Available, Programmed, QuotaGranted, ReferencedDataReady). Use to see how a failure is distributed across replicas. Read-only.",
      inputSchema: {
        workload: z
          .string()
          .optional()
          .describe('Optional workload name to filter by, e.g. "api-backend"'),
      },
    },
    async ({ workload }) => {
      log(`tools/call instances_list workload=${workload ?? '*'}`);
      if (workload && !getWorkload(workload)) return unknownWorkload(workload);
      const rows = workload
        ? instancesFor(workload)
        : workloadNames().flatMap((w) => instancesFor(w));
      return jsonResult(rows.map(conditionView));
    },
  );

  server.registerTool(
    'workload_diagnose',
    {
      title: 'Diagnose workload',
      description:
        "Diagnose why a Workload is not available. Walks Workload -> WorkloadDeployment -> Instance, follows compute's pointer reasons (QuotaNotGranted, NoAvailablePlacements, ReferencedDataNotReady) down to the condition that names the real cause, and returns that root cause with an explanation, whether it is user-actionable or a platform fault, concrete next steps, and which skill covers the full procedure. This is the tool to reach for when someone asks why a workload is broken. Read-only.",
      inputSchema: {
        name: z.string().describe('Workload name, e.g. "api-backend"'),
      },
    },
    async ({ name }) => {
      log(`tools/call workload_diagnose name=${name}`);
      const d = diagnoseWorkload(name);
      if (!d) return unknownWorkload(name);
      return jsonResult(d);
    },
  );

  server.registerTool(
    'reason_explain',
    {
      title: 'Explain a condition reason',
      description:
        'Explain any compute condition reason (e.g. "QuotaExceeded", "ImageUnavailable", "CityCodeMismatch"): what it means, which condition types carry it, whether it is user-actionable, a platform fault, or transient, and how to remediate it. Call with no argument to list the whole catalog. Use when you encounter a reason on a resource the diagnose tool did not cover. Read-only.',
      inputSchema: {
        reason: z
          .string()
          .optional()
          .describe('Reason string, e.g. "QuotaExceeded". Omit to list every known reason.'),
      },
    },
    async ({ reason }) => {
      log(`tools/call reason_explain reason=${reason ?? '*'}`);
      if (!reason) return jsonResult(allReasons());
      const info = explainReason(reason);
      if (!info) {
        return errorResult(
          `No catalog entry for reason ${JSON.stringify(reason)}. Call reason_explain with no argument to list every known reason.`,
        );
      }
      return jsonResult(info);
    },
  );

  // Deliberately DESTRUCTIVE tool, mirroring StreamCo's streams_delete: the
  // provider exposes it, the gateway MCPRoute toolSelector EXCLUDES it. It must
  // be reachable on a direct connection yet absent through the gateway — that
  // gap is the allow-list enforcement proof. Demo no-op; never mutates.
  server.registerTool(
    'workload_delete',
    {
      title: 'Delete workload',
      description:
        'Delete a Workload by name. DESTRUCTIVE — excluded from the reviewed gateway allow-list. (Demo no-op: does not actually remove anything.)',
      inputSchema: { name: z.string().describe('Workload name to delete') },
    },
    async ({ name }) => {
      log(`tools/call workload_delete name=${name}`);
      if (!getWorkload(name)) return unknownWorkload(name);
      return jsonResult({
        name,
        deleted: false,
        note: 'demo no-op: workload_delete does not remove anything; it exists only to prove gateway MCPRoute allow-list exclusion.',
      });
    },
  );

  return server;
}

async function handleMcp(req: IncomingMessage, res: ServerResponse): Promise<void> {
  if (req.method !== 'POST') {
    res.writeHead(405, { 'content-type': 'application/json', allow: 'POST' }).end(
      JSON.stringify({
        jsonrpc: '2.0',
        error: {
          code: -32000,
          message: 'Method not allowed. This MCP endpoint is stateless; use POST.',
        },
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
  console.log(`[compute] ${new Date().toISOString()} ${message}`);
}

const httpServer = createServer((req, res) => {
  const url = new URL(req.url ?? '/', `http://${req.headers.host ?? `${HOST}:${PORT}`}`);

  if (url.pathname === '/mcp') {
    handleMcp(req, res).catch((err: unknown) => {
      log(`ERROR handling /mcp: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}`);
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
  log(
    `listening on http://${HOST}:${PORT} (MCP at /mcp; knowledge at /llms-full.txt and /runbooks/*.md)`,
  );
});

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    log(`received ${signal}, shutting down`);
    httpServer.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 1500).unref();
  });
}
