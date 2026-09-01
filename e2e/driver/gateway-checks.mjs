// Gateway e2e assertion driver (QA-owned) — task #15 / CONTRACT-GATEWAY.md.
// Zero external deps beyond @modelcontextprotocol/sdk (for MCP tools/list).
// Runs against the LIVE Envoy AI Gateway env (infra-engineer's e2e/gateway/).
//
// Modes (run by run-e2e.sh's `gateway` leg):
//   chat          proof 2 — `patch chat` (gateway mode, --json) streams the
//                 StreamCo findings, exit 0; captures the contextId for proof 3.
//   allowlist     proof 5 — through the gateway MCPRoute, tools/list = the 3
//                 gateway-namespaced tools (streamco-backend__*), NO
//                 streamco-backend__streams_delete, and calling it is rejected;
//                 direct-to-StreamCo shows all 4 RAW tools (the control).
//   tokens        proof 3 — gateway access-log token counts (summed over the
//                 conversation's llm lines) EQUAL the sink's self-reported
//                 totals, and each line carries x_datum_* attribution.
//   credisolation proof 4 — a direct call to the stub WITHOUT the gateway-
//                 injected key → 401.
import { spawn } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

// The MCP SDK is imported LAZILY (only the `allowlist` mode needs it, and only
// that mode is run with NODE_PATH → e2e/streamco/node_modules). Importing it at
// module top-level would make chat/tokens/credisolation fail to load without
// NODE_PATH. Resolved once, on first use.
let _mcp;
async function mcpSdk() {
  if (!_mcp) {
    const [{ Client }, { StreamableHTTPClientTransport }] = await Promise.all([
      import('@modelcontextprotocol/sdk/client/index.js'),
      import('@modelcontextprotocol/sdk/client/streamableHttp.js'),
    ]);
    _mcp = { Client, StreamableHTTPClientTransport };
  }
  return _mcp;
}

// ── Config (env; run-e2e.sh supplies infra-engineer's real values) ──────────
const OUT_DIR = process.env.OUT_DIR ?? join(process.cwd(), 'out');
const SINK_URL = (process.env.SINK_URL ?? 'http://127.0.0.1:7811').replace(/\/$/, '');
const PROJECT = process.env.PROJECT ?? 'demo-project';
const PROMPT = process.env.PROMPT ?? 'Diagnose pipeline p-1 for StreamCo';
const FINDING_MARKERS = ['CONSUMER_LAG', 'vod-transcode', 'p-1'];
const CTX_FILE = join(OUT_DIR, 'gateway-contextid.txt');
// CLI (gateway mode; PATCH_URL/PATCH_TOKEN in env)
const PATCH_CMD = process.env.PATCH_CMD ?? 'bun run cli/main.ts';
const PATCH_URL = (process.env.PATCH_URL ?? 'http://127.0.0.1:7820').replace(/\/$/, '');
const PATCH_TOKEN = process.env.PATCH_TOKEN ?? 'e2e-token';
// Proof 5 — MCP endpoints
const GATEWAY_MCP_URL = process.env.GATEWAY_MCP_URL ?? 'http://localhost:1975/mcp';
const STREAMCO_DIRECT_URL = process.env.STREAMCO_DIRECT_URL ?? 'http://127.0.0.1:7810/mcp';
const GATEWAY_INCLUDED_TOOLS = (process.env.GATEWAY_INCLUDED_TOOLS ??
  'streamco-backend__pipeline_diagnose,streamco-backend__streams_get,streamco-backend__streams_list')
  .split(',').map((s) => s.trim()).sort();
const GATEWAY_EXCLUDED_TOOL = process.env.GATEWAY_EXCLUDED_TOOL ?? 'streamco-backend__streams_delete';
const DIRECT_EXCLUDED_TOOL = process.env.DIRECT_EXCLUDED_TOOL ?? 'streams_delete';
// Proof 3 — access log
const ACCESS_LOG_FILE = process.env.ACCESS_LOG_FILE ?? '';
let EXPECT_CONTEXT_ID = process.env.EXPECT_CONTEXT_ID ?? '';
// Proof 4 — stub direct
const STUB_DIRECT_URL = process.env.STUB_DIRECT_URL ?? '';
const CLI_TIMEOUT_MS = Number(process.env.CLI_TIMEOUT_MS ?? 45_000);

mkdirSync(OUT_DIR, { recursive: true });

// Recursively find the first value for `key` anywhere in a nested object/array.
// A2A v1.0 (a2a-go) nests contextId inside the StreamResponse oneOf envelope
// and/or a JSON-RPC {result:{...}} wrapper — a top-level lookup no longer works.
function findKey(node, key) {
  if (node == null || typeof node !== 'object') return undefined;
  if (Array.isArray(node)) {
    for (const el of node) { const r = findKey(el, key); if (r !== undefined) return r; }
    return undefined;
  }
  for (const [k, v] of Object.entries(node)) {
    if (k === key && (typeof v === 'string' || typeof v === 'number')) return String(v);
    const r = findKey(v, key); if (r !== undefined) return r;
  }
  return undefined;
}

const results = [];
function record(item, name, ok, detail, required = true) {
  results.push({ item, name, ok: !!ok, required, detail: String(detail).slice(0, 500) });
  (ok ? console.log : console.error)(`${ok ? 'PASS' : required ? 'FAIL' : 'WARN'}  [${item}] ${name} — ${detail}`);
}
function finish(label) {
  const reqFails = results.filter((r) => r.required && !r.ok);
  writeFileSync(join(OUT_DIR, `gateway-${label}-summary.json`), JSON.stringify({ label, results }, null, 2));
  appendFileSync(join(OUT_DIR, 'gateway.log'), JSON.stringify({ label, results }) + '\n');
  console.log(`\n${reqFails.length === 0 ? `GATEWAY ${label.toUpperCase()} PASS` : `GATEWAY ${label.toUpperCase()} FAIL (${reqFails.length} req)`} — ${results.filter((r) => r.ok).length}/${results.length}`);
  process.exit(reqFails.length === 0 ? 0 : 1);
}

function runCli(args) {
  const words = PATCH_CMD.trim().split(/\s+/);
  return new Promise((resolve) => {
    const child = spawn(words[0], [...words.slice(1), ...args], {
      env: { ...process.env, PATCH_URL, PATCH_TOKEN },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '', stderr = '';
    const t = setTimeout(() => child.kill('SIGKILL'), CLI_TIMEOUT_MS);
    child.stdout.on('data', (d) => (stdout += d));
    child.stderr.on('data', (d) => (stderr += d));
    child.on('error', (e) => { clearTimeout(t); resolve({ code: 127, stdout, stderr: stderr + e.message }); });
    child.on('close', (code) => { clearTimeout(t); resolve({ code: code ?? 0, stdout, stderr }); });
  });
}

async function mcpTools(url) {
  const { Client, StreamableHTTPClientTransport } = await mcpSdk();
  const client = new Client({ name: 'gateway-qa', version: '0.0.1' });
  const transport = new StreamableHTTPClientTransport(new URL(url));
  await client.connect(transport);
  try { return (await client.listTools()).tools.map((t) => t.name).sort(); }
  finally { await client.close(); }
}

// ── proof 2: CLI chat through the gateway ───────────────────────────────────
async function chat() {
  const res = await runCli(['chat', PROMPT, '--project', PROJECT, '--json']);
  writeFileSync(join(OUT_DIR, 'gateway-cli-chat.txt'), res.stdout + '\n---STDERR---\n' + res.stderr);
  const events = res.stdout.split('\n').map((l) => l.trim()).filter(Boolean).map((l) => { try { return JSON.parse(l); } catch { return undefined; } }).filter(Boolean);
  const contextId = events.map((e) => findKey(e, 'contextId')).find(Boolean);
  const text = JSON.stringify(events);
  const hits = FINDING_MARKERS.filter((m) => text.includes(m));
  record('gw.chat.exit', 'patch chat (gateway mode) exits 0', res.code === 0, `exit=${res.code}`);
  record('gw.chat.findings', 'chat streams StreamCo findings (service→gateway→stub + MCP via gateway)', hits.length > 0, `matched [${hits.join(', ')}]`);
  record('gw.chat.context', 'captured the conversation contextId', !!contextId, `contextId=${contextId}`);
  if (contextId) writeFileSync(CTX_FILE, contextId);
  finish('chat');
}

// ── proof 5: allow-list enforcement ─────────────────────────────────────────
async function allowlist() {
  const direct = await mcpTools(STREAMCO_DIRECT_URL).catch((e) => ({ err: e.message }));
  const directNames = Array.isArray(direct) ? direct : [];
  record('gw.allowlist.control', `direct StreamCo exposes ${DIRECT_EXCLUDED_TOOL} (4 tools) — control`, directNames.includes(DIRECT_EXCLUDED_TOOL), `direct=[${directNames.join(', ')}]${direct.err ? ' err=' + direct.err : ''}`);

  const gw = await mcpTools(GATEWAY_MCP_URL).catch((e) => ({ err: e.message }));
  const gwNames = Array.isArray(gw) ? gw : [];
  // Reachability precondition: an EMPTY list means the gateway MCP was NOT
  // reached — so "excluded"/"blocked" must NOT trivially pass on a dead gateway.
  const gwReached = gwNames.length > 0;
  record('gw.allowlist.included', `gateway tools/list = the allow-list [${GATEWAY_INCLUDED_TOOLS.join(', ')}]`, gwReached && JSON.stringify(gwNames) === JSON.stringify(GATEWAY_INCLUDED_TOOLS), `reached=${gwReached} gateway=[${gwNames.join(', ')}]${gw.err ? ' err=' + gw.err : ''}`);
  record('gw.allowlist.excluded', `gateway reachable AND does NOT expose ${GATEWAY_EXCLUDED_TOOL}`, gwReached && !gwNames.includes(GATEWAY_EXCLUDED_TOOL), `reached=${gwReached} present=${gwNames.includes(GATEWAY_EXCLUDED_TOOL)}`);

  // Call the excluded tool through the gateway — a valid block requires the
  // gateway to be REACHED and to reject it (isError / protocol error), NOT a
  // transport/connect failure (which would be a false "block").
  let blocked = false, detail = '';
  if (!gwReached) {
    detail = 'gateway MCP unreachable — cannot assert a genuine block';
  } else {
    try {
      const { Client, StreamableHTTPClientTransport } = await mcpSdk();
      const client = new Client({ name: 'gateway-qa', version: '0.0.1' });
      const transport = new StreamableHTTPClientTransport(new URL(GATEWAY_MCP_URL));
      await client.connect(transport);
      try {
        const r = await client.callTool({ name: GATEWAY_EXCLUDED_TOOL, arguments: { id: 's-1' } });
        blocked = r?.isError === true; detail = `isError=${r?.isError}`;
      } catch (e) { blocked = true; detail = `rejected by gateway: ${e.message}`; }
      finally { await client.close(); }
    } catch (e) { blocked = false; detail = `connect failed (NOT a valid block): ${e.message}`; }
  }
  record('gw.allowlist.call_blocked', `calling ${GATEWAY_EXCLUDED_TOOL} through the gateway is rejected`, blocked, detail);
  finish('allowlist');
}

// ── proof 3: gateway-counted tokens + attribution ───────────────────────────
function sumByKeySubstr(node, substr, acc = { sum: 0, n: 0 }) {
  if (node == null) return acc;
  if (Array.isArray(node)) { for (const v of node) sumByKeySubstr(v, substr, acc); return acc; }
  if (typeof node === 'object') for (const [k, v] of Object.entries(node)) {
    if (k.includes(substr) && (typeof v === 'number' || (typeof v === 'string' && /^\d+$/.test(v)))) { acc.sum += Number(v); acc.n += 1; }
    else sumByKeySubstr(v, substr, acc);
  }
  return acc;
}
function stringsByKeySubstr(node, substr, acc = new Set()) {
  if (node == null) return acc;
  if (Array.isArray(node)) { for (const v of node) stringsByKeySubstr(v, substr, acc); return acc; }
  if (typeof node === 'object') for (const [k, v] of Object.entries(node)) {
    if (k.includes(substr) && (typeof v === 'string' || typeof v === 'number')) acc.add(String(v));
    else stringsByKeySubstr(v, substr, acc);
  }
  return acc;
}
async function tokens() {
  if (!EXPECT_CONTEXT_ID && existsSync(CTX_FILE)) EXPECT_CONTEXT_ID = readFileSync(CTX_FILE, 'utf8').trim();
  if (!ACCESS_LOG_FILE || !existsSync(ACCESS_LOG_FILE)) { record('gw.tokens.log', 'gateway access log present', false, `ACCESS_LOG_FILE=${ACCESS_LOG_FILE}`); return finish('tokens'); }
  const lines = readFileSync(ACCESS_LOG_FILE, 'utf8').split('\n').map((l) => l.trim()).filter(Boolean);
  // Each JSON access-log line may carry a `[pod/container]` prefix (kubectl
  // --prefix) or other leading text — parse from the first `{`.
  const recs = lines.map((l) => {
    const i = l.indexOf('{');
    if (i < 0) return undefined;
    try { return JSON.parse(l.slice(i)); } catch { return undefined; }
  }).filter(Boolean);
  // llm access-log records for THIS conversation (x_datum_conversation == contextId).
  const convRecs = recs.filter((r) => {
    const convs = [...stringsByKeySubstr(r, 'x_datum_conversation')];
    return EXPECT_CONTEXT_ID ? convs.includes(EXPECT_CONTEXT_ID) : convs.length > 0;
  });
  record('gw.tokens.record', 'gateway access log has llm record(s) for the conversation', convRecs.length >= 1, `matched=${convRecs.length} contextId=${EXPECT_CONTEXT_ID} (of ${recs.length} records)`);
  if (convRecs.length === 0) return finish('tokens');

  // Attribution on every matched line.
  const proj = new Set(), agent = new Set();
  for (const r of convRecs) { for (const v of stringsByKeySubstr(r, 'x_datum_project')) proj.add(v); for (const v of stringsByKeySubstr(r, 'x_datum_agent')) agent.add(v); }
  record('gw.tokens.attribution', 'access log carries x_datum_project/conversation/agent=patch', proj.has(PROJECT) && agent.has('patch'), `project=${[...proj]} agent=${[...agent]}`);

  // Summed gateway token counts across the conversation's lines.
  const gIn = convRecs.reduce((s, r) => s + sumByKeySubstr(r, 'input_tokens').sum, 0);
  const gOut = convRecs.reduce((s, r) => s + sumByKeySubstr(r, 'output_tokens').sum, 0);
  const gTot = convRecs.reduce((s, r) => s + sumByKeySubstr(r, 'total_tokens').sum, 0);
  record('gw.tokens.present', 'gateway counted input/output/total tokens (>0)', gIn > 0 && gOut > 0 && gTot > 0, `input=${gIn} output=${gOut} total=${gTot}`);

  // Cross-check vs the sink's self-reported usage for the same conversation.
  const sink = await fetch(`${SINK_URL}/events`).then((r) => r.json()).catch(() => []);
  const forConv = sink.filter((e) => !EXPECT_CONTEXT_ID || e?.data?.resource?.name === EXPECT_CONTEXT_ID);
  const sIn = forConv.filter((e) => /input-tokens/.test(e?.type ?? '')).reduce((s, e) => s + Number(e?.data?.value ?? 0), 0);
  const sOut = forConv.filter((e) => /output-tokens/.test(e?.type ?? '')).reduce((s, e) => s + Number(e?.data?.value ?? 0), 0);
  record('gw.tokens.equal_sink', 'gateway-counted tokens EQUAL the sink self-reported totals (input & output)', gIn === sIn && gOut === sOut, `gateway in/out=${gIn}/${gOut} sink in/out=${sIn}/${sOut} (delta in=${gIn - sIn} out=${gOut - sOut})`);
  finish('tokens');
}

// ── proof 4: credential isolation ───────────────────────────────────────────
async function credisolation() {
  if (!STUB_DIRECT_URL) { record('gw.cred.direct401', 'direct stub without key → 401', false, 'STUB_DIRECT_URL not set'); return finish('credisolation'); }
  let status = 0, body = '';
  try {
    const res = await fetch(STUB_DIRECT_URL, {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ model: 'patch-stub-v1', messages: [{ role: 'user', content: 'ping' }], stream: false }),
    });
    status = res.status; body = (await res.text()).slice(0, 200);
  } catch (e) { body = `threw: ${e.message}`; }
  record('gw.cred.direct401', 'direct-to-stub WITHOUT the gateway-injected key → 401 (key lives only at the gateway)', status === 401, `status=${status} body=${body}`);
  finish('credisolation');
}

const mode = process.argv[2];
if (mode === 'chat') await chat();
else if (mode === 'allowlist') await allowlist();
else if (mode === 'tokens') await tokens();
else if (mode === 'credisolation') await credisolation();
else { console.error(`unknown mode '${mode}' (use: chat | allowlist | tokens | credisolation)`); process.exit(2); }
