// LIFTED VERBATIM from cloud-portal
//   branch:  feat/patch-dynamic-composition
//   path:    app/modules/assistant/composition/mcp-integration.test.ts
// This is the contract-required REAL @ai-sdk/mcp Streamable HTTP
// round-trip against an in-process MCP server.
// -----------------------------------------------------------------------
//
// Round-trip integration test for the DEFAULT MCP client factory: a
// real @ai-sdk/mcp client speaking Streamable HTTP to an in-process
// fake MCP server (node:http, ephemeral port). This covers exactly what
// the unit-level mocks cannot: initialize/tools-list/tools-call over
// the wire against a stateless server (POST-only JSON responses, no
// session id, GET rejected with 405) — the same behavior the StreamCo
// demo server exhibits.
import { composeCapabilities } from './compose';
import { agentBindingSchema } from './types';
import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createServer, type Server } from 'node:http';

interface JsonRpcRequest {
  jsonrpc: '2.0';
  id?: number | string;
  method: string;
  params?: Record<string, unknown>;
}

interface FakeHttpResponse {
  status: number;
  body?: unknown;
}

const TOOL_CALLS: Array<{ name: string; args: unknown }> = [];

function rpcResult(id: number | string | undefined, result: unknown): FakeHttpResponse {
  return { status: 200, body: { jsonrpc: '2.0', id, result } };
}

function handleMcpRequest(message: JsonRpcRequest): FakeHttpResponse {
  switch (message.method) {
    case 'initialize':
      return rpcResult(message.id, {
        protocolVersion: (message.params as { protocolVersion?: string })?.protocolVersion,
        capabilities: { tools: {} },
        serverInfo: { name: 'fake-streamco', version: '1.0.0' },
      });
    case 'notifications/initialized':
      // Notification (no id): the transport ignores the body on 200.
      return { status: 200 };
    case 'tools/list':
      return rpcResult(message.id, {
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
          {
            name: 'not_allow_listed_tool',
            description: 'Must never be exposed',
            inputSchema: { type: 'object', properties: {} },
          },
        ],
      });
    case 'tools/call': {
      const params = message.params as { name: string; arguments?: unknown };
      TOOL_CALLS.push({ name: params.name, args: params.arguments });
      return rpcResult(message.id, {
        content: [
          {
            type: 'text',
            text: JSON.stringify({
              id: (params.arguments as { id?: string })?.id,
              findings: ['CONSUMER_LAG'],
            }),
          },
        ],
      });
    }
    default:
      return rpcResult(message.id, {});
  }
}

let server: Server;
let port: number;

beforeAll(async () => {
  server = createServer((req, res) => {
    if (req.url !== '/mcp') {
      res.writeHead(404).end('not found');
      return;
    }
    // Stateless server: no SSE side-channel, like the StreamCo demo.
    if (req.method !== 'POST') {
      res.writeHead(405).end('method not allowed');
      return;
    }
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => {
      const message = JSON.parse(Buffer.concat(chunks).toString('utf8')) as JsonRpcRequest;
      const { status, body } = handleMcpRequest(message);
      if (body === undefined) {
        res.writeHead(status).end();
      } else {
        res.writeHead(status, { 'content-type': 'application/json' }).end(JSON.stringify(body));
      }
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (typeof address === 'object' && address) port = address.port;
});

afterAll(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

describe('defaultMcpClientFactory (real @ai-sdk/mcp over Streamable HTTP)', () => {
  it('initializes, lists, filters, namespaces, and calls tools end to end', async () => {
    const binding = agentBindingSchema.parse({
      spec: {
        serviceRef: { name: 'streamco' },
        serviceName: 'streaming.streamco.example',
        serviceAgentRef: { name: 'streamco-agent' },
        configurationVersion: 'v1',
        tools: {
          mcpServers: [
            {
              name: 'streamco',
              endpoint: `http://127.0.0.1:${port}/mcp`,
              toolSelector: { include: ['streams_list', 'pipeline_diagnose'] },
              mutating: [],
            },
          ],
        },
      },
    });

    const invocations: string[] = [];
    // No mcpClientFactory override: exercises the real default factory.
    const composed = await composeCapabilities([binding], {
      onProviderToolInvocation: (invocation) => invocations.push(invocation.namespacedToolName),
    });

    try {
      expect(Object.keys(composed.tools).sort()).toEqual([
        'streamco__pipeline_diagnose',
        'streamco__streams_list',
      ]);

      const result = (await composed.tools['streamco__pipeline_diagnose']!.execute!(
        { id: 'p-1' },
        { toolCallId: 'itest', messages: [] }
      )) as { content: Array<{ type: string; text: string }> };

      expect(result.content[0]!.type).toBe('text');
      expect(JSON.parse(result.content[0]!.text)).toEqual({
        id: 'p-1',
        findings: ['CONSUMER_LAG'],
      });

      // The wire actually saw the un-namespaced tool name...
      expect(TOOL_CALLS).toEqual([{ name: 'pipeline_diagnose', args: { id: 'p-1' } }]);
      // ...and the metering hook fired once with the namespaced one.
      expect(invocations).toEqual(['streamco__pipeline_diagnose']);
    } finally {
      await composed.close();
    }
  });
});
