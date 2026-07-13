// StreamCo demo server self-test (contract item 3 evidence).
//
// Proves, against a real HTTP listener on 127.0.0.1:7810:
//   1. MCP round-trip: initialize (via client connect) -> tools/list ->
//      tools/call for all three canned tools, including an unknown-id error.
//   2. Knowledge routes: /llms-full.txt, /runbooks/lag.md and the STRETCH
//      /.well-known/agent-card.json serve the expected content.
//
// If nothing is listening it spawns `node src/server.ts` itself and tears it
// down at the end, so `node src/selftest.ts` is a self-contained check.
// Exit code 0 = all checks passed.

import { spawn, type ChildProcess } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';

import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js';

const HOST = process.env.STREAMCO_HOST ?? '127.0.0.1';
const PORT = Number(process.env.STREAMCO_PORT ?? 7810);
const BASE = `http://${HOST}:${PORT}`;

let failures = 0;

function check(name: string, ok: boolean, detail: string): void {
  if (ok) {
    console.log(`PASS  ${name} — ${detail}`);
  } else {
    failures += 1;
    console.error(`FAIL  ${name} — ${detail}`);
  }
}

function firstText(result: unknown): string {
  const content = (result as { content?: Array<{ type: string; text?: string }> }).content ?? [];
  const text = content.find((c) => c.type === 'text')?.text;
  if (text === undefined) throw new Error(`tool result has no text content: ${JSON.stringify(result)}`);
  return text;
}

async function healthy(): Promise<boolean> {
  try {
    const res = await fetch(`${BASE}/healthz`, { signal: AbortSignal.timeout(500) });
    return res.ok;
  } catch {
    return false;
  }
}

async function main(): Promise<void> {
  let child: ChildProcess | undefined;

  if (await healthy()) {
    console.log(`[selftest] reusing already-running server at ${BASE}`);
  } else {
    const serverPath = join(dirname(fileURLToPath(import.meta.url)), 'server.ts');
    console.log(`[selftest] starting server: node ${serverPath}`);
    child = spawn(process.execPath, [serverPath], {
      env: { ...process.env, STREAMCO_HOST: HOST, STREAMCO_PORT: String(PORT) },
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    const deadline = Date.now() + 10_000;
    while (!(await healthy())) {
      if (Date.now() > deadline) throw new Error('server did not become healthy within 10s');
      if (child.exitCode !== null) throw new Error(`server exited early with code ${child.exitCode}`);
      await sleep(200);
    }
  }

  try {
    // --- MCP round-trip -------------------------------------------------
    const client = new Client({ name: 'streamco-selftest', version: '0.1.0' });
    const transport = new StreamableHTTPClientTransport(new URL(`${BASE}/mcp`));
    await client.connect(transport); // performs MCP initialize
    const serverVersion = client.getServerVersion();
    check(
      'mcp.initialize',
      serverVersion?.name === 'streamco-mcp',
      `serverInfo=${JSON.stringify(serverVersion)}`,
    );

    const tools = await client.listTools();
    const names = tools.tools.map((t) => t.name).sort();
    check(
      'mcp.tools/list',
      JSON.stringify(names) ===
        JSON.stringify(['pipeline_diagnose', 'streams_delete', 'streams_get', 'streams_list']),
      `tools=${names.join(', ')}`,
    );

    const list = JSON.parse(firstText(await client.callTool({ name: 'streams_list', arguments: {} })));
    check(
      'mcp.tools/call streams_list',
      Array.isArray(list) && list.length === 3 && list.some((s: { id: string }) => s.id === 's-2'),
      `${list.length} streams: ${list.map((s: { id: string }) => s.id).join(', ')}`,
    );

    const s2 = JSON.parse(firstText(await client.callTool({ name: 'streams_get', arguments: { id: 's-2' } })));
    check(
      'mcp.tools/call streams_get(s-2)',
      s2.name === 'playback-telemetry' && s2.status === 'degraded' && s2.lagSeconds === 847,
      `name=${s2.name} status=${s2.status} lagSeconds=${s2.lagSeconds}`,
    );

    const diag = JSON.parse(
      firstText(await client.callTool({ name: 'pipeline_diagnose', arguments: { id: 'p-1' } })),
    );
    check(
      'mcp.tools/call pipeline_diagnose(p-1)',
      diag.findings?.length === 3 &&
        diag.findings[0]?.code === 'CONSUMER_LAG' &&
        typeof diag.recommendation === 'string' &&
        diag.recommendation.includes('/runbooks/lag.md'),
      `findings=[${diag.findings?.map((f: { code: string }) => f.code).join(', ')}] recommendation="${String(diag.recommendation).slice(0, 60)}..."`,
    );

    const unknown = await client.callTool({ name: 'pipeline_diagnose', arguments: { id: 'p-404' } });
    check(
      'mcp.tools/call pipeline_diagnose(unknown) -> isError',
      (unknown as { isError?: boolean }).isError === true,
      firstText(unknown).slice(0, 80),
    );

    // streams_delete exists DIRECTLY (control for the gateway allow-list proof,
    // where the MCPRoute excludes it). Demo no-op.
    const del = JSON.parse(
      firstText(await client.callTool({ name: 'streams_delete', arguments: { id: 's-1' } })),
    );
    check(
      'mcp.tools/call streams_delete(s-1) (excluded from gateway allow-list)',
      del.id === 's-1' && del.deleted === false,
      `id=${del.id} deleted=${del.deleted}`,
    );

    await client.close();

    // --- Knowledge routes ------------------------------------------------
    const llms = await fetch(`${BASE}/llms-full.txt`);
    const llmsBody = await llms.text();
    check(
      'http GET /llms-full.txt',
      llms.ok &&
        (llms.headers.get('content-type') ?? '').startsWith('text/plain') &&
        llmsBody.includes('streaming.streamco.example') &&
        llmsBody.includes('pipeline_diagnose'),
      `status=${llms.status} bytes=${llmsBody.length}`,
    );

    const runbook = await fetch(`${BASE}/runbooks/lag.md`);
    const runbookBody = await runbook.text();
    check(
      'http GET /runbooks/lag.md',
      runbook.ok &&
        (runbook.headers.get('content-type') ?? '').startsWith('text/markdown') &&
        runbookBody.includes('CONSUMER_LAG'),
      `status=${runbook.status} bytes=${runbookBody.length}`,
    );

    const card = await fetch(`${BASE}/.well-known/agent-card.json`);
    const cardBody: { name?: string; skills?: unknown[] } = await card.json();
    check(
      'http GET /.well-known/agent-card.json (stretch)',
      card.ok && cardBody.name === 'StreamCo Streaming Agent' && Array.isArray(cardBody.skills) && cardBody.skills.length >= 1,
      `status=${card.status} name="${cardBody.name}" skills=${cardBody.skills?.length}`,
    );

    const missing = await fetch(`${BASE}/nope`);
    check('http GET unknown route -> 404', missing.status === 404, `status=${missing.status}`);
  } finally {
    if (child) {
      child.kill('SIGTERM');
      await sleep(300);
      if (child.exitCode === null) child.kill('SIGKILL');
    }
  }

  console.log(failures === 0 ? '\nSELFTEST PASS (streamco demo server)' : `\nSELFTEST FAIL: ${failures} check(s) failed`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err: unknown) => {
  console.error('[selftest] fatal:', err);
  process.exit(1);
});
