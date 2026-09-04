// Consumer e2e assertion driver (QA-owned) — zero deps, plain Node >= 22.
// Task #12 / CONTRACT-CONSUMERS.md Workstream C.
//
// Two subcommands (run by run-e2e.sh's `consumers` leg against the REAL,
// already-running service + sink):
//
//   node consumers-checks.mjs cli
//     Proves the `patch` CLI consumer: `patch card`, `patch chat ... --project`
//     (streams findings, exit 0, sink shows the turn's tool-invocation + token
//     events under one contextId), and bad-token → non-zero with a clear error.
//
//   node consumers-checks.mjs nodemeter
//     No-double-metering assertion for the PORTAL leg: given the sink state
//     captured right after the portal client-mode conversation, asserts every
//     usage event was emitted by the SERVICE (source contains SERVICE_SOURCE
//     marker) and NONE by the portal (source contains PORTAL_SOURCE_MARKER),
//     with no duplicate (meter,contextId,value) rows.
//
// The CLI is spawned via PATCH_CMD (a shell word list, e.g. "bun run cli/index.ts")
// so this driver never hard-codes the engineer's entrypoint. Exit 0 iff every
// REQUIRED check passed.

import { spawn } from 'node:child_process';
import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

// ── Config (env, contract defaults) ─────────────────────────────────────────
const SINK_URL = (process.env.SINK_URL ?? 'http://127.0.0.1:7811').replace(/\/$/, '');
const PATCH_URL = (process.env.PATCH_URL ?? 'http://127.0.0.1:7820').replace(/\/$/, '');
const PATCH_TOKEN = process.env.PATCH_TOKEN ?? 'e2e-token';
const BAD_TOKEN = process.env.BAD_TOKEN ?? 'not-a-real-token';
const PROJECT = process.env.PROJECT ?? 'demo-project';
const OUT_DIR = process.env.OUT_DIR ?? join(process.cwd(), 'out');
// Shell word list to invoke the CLI (confirmed with assistant-engineer):
// "bun run <repo>/cli/main.ts". Set by run-e2e.sh.
const PATCH_CMD = process.env.PATCH_CMD ?? 'bun run cli/main.ts';
const PROMPT = process.env.PROMPT ?? 'Diagnose pipeline p-1 for StreamCo';
const FINDING_MARKERS = ['CONSUMER_LAG', 'vod-transcode', 'p-1', 'runbooks/lag.md'];
// no-double-metering markers. The SERVICE source is its PUBLIC_BASE_URL + /a2a;
// the PORTAL's embedded-mode source is `${APP_URL}/api/assistant` or the
// `cloud-portal/api/assistant` fallback (per portal-engineer) — either marker
// present on a sink event means the portal double-emitted.
const SERVICE_SOURCE_MARKER = process.env.SERVICE_SOURCE_MARKER ?? ':7820/a2a';
const PORTAL_SOURCE_MARKERS = (process.env.PORTAL_SOURCE_MARKERS ?? '/api/assistant,cloud-portal')
  .split(',').map((s) => s.trim()).filter(Boolean);
// When set (run-e2e.sh pins ASSISTANT_SERVICE_E2E_CONVERSATION), assert every
// portal-conversation sink event carries exactly this contextId.
const EXPECT_CONTEXT_ID = process.env.EXPECT_CONTEXT_ID ?? '';
const CLI_TIMEOUT_MS = Number(process.env.CLI_TIMEOUT_MS ?? 30_000);

mkdirSync(OUT_DIR, { recursive: true });

// Recursively find the first value for `key` anywhere in a nested object/array.
// A2A v1.0 (a2a-go) nests contextId inside the StreamResponse oneOf envelope
// (e.g. {statusUpdate:{contextId}}, {task:{contextId}}) and/or a JSON-RPC
// {result:{...}} wrapper, so a top-level lookup no longer suffices.
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
  results.push({ item, name, ok: !!ok, required, detail: String(detail).slice(0, 400) });
  const tag = ok ? 'PASS' : required ? 'FAIL' : 'WARN';
  (ok ? console.log : console.error)(`${tag}  [${item}] ${name} — ${detail}`);
}

// Spawn PATCH_CMD + args, with env overrides; capture stdout/stderr/exit code.
function runCli(args, { token = PATCH_TOKEN } = {}) {
  const words = PATCH_CMD.trim().split(/\s+/);
  const cmd = words[0];
  const argv = [...words.slice(1), ...args];
  return new Promise((resolve) => {
    const child = spawn(cmd, argv, {
      env: { ...process.env, PATCH_URL, PATCH_TOKEN: token },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    const timer = setTimeout(() => child.kill('SIGKILL'), CLI_TIMEOUT_MS);
    child.stdout.on('data', (d) => (stdout += d));
    child.stderr.on('data', (d) => (stderr += d));
    child.on('error', (err) => {
      clearTimeout(timer);
      resolve({ code: 127, stdout, stderr: stderr + `\n[spawn error] ${err.message}` });
    });
    child.on('close', (code) => {
      clearTimeout(timer);
      resolve({ code: code ?? 0, stdout, stderr });
    });
  });
}

async function sinkEvents() {
  return fetch(`${SINK_URL}/events`).then((r) => r.json()).catch(() => []);
}
async function wipeSink() {
  await fetch(`${SINK_URL}/events`, { method: 'DELETE' }).catch(() => {});
}

// ── CLI leg ─────────────────────────────────────────────────────────────────
async function cliLeg() {
  // C-card: `patch card`
  const card = await runCli(['card']);
  writeFileSync(join(OUT_DIR, 'cli-card.txt'), card.stdout + '\n---STDERR---\n' + card.stderr);
  record('cli.card', '`patch card` exits 0 and shows the Patch card', card.code === 0 && /Patch/.test(card.stdout), `exit=${card.code} name-present=${/Patch/.test(card.stdout)}`);

  // C-card-json: `patch card --json` is a structurally valid A2A AgentCard.
  const cardJson = await runCli(['card', '--json']);
  writeFileSync(join(OUT_DIR, 'cli-card-json.txt'), cardJson.stdout + '\n---STDERR---\n' + cardJson.stderr);
  let cj = {};
  try { cj = JSON.parse(cardJson.stdout); } catch { /* leave {} */ }
  // A2A v1.0 (a2a-go AgentCard): protocolVersion/url/protocolBinding live in
  // supportedInterfaces[]; bearer scheme is nested under
  // securitySchemes.bearer.httpAuthSecurityScheme.scheme (tolerate the flat
  // v0.3 shape + compare the scheme value case-insensitively).
  const ifaces = Array.isArray(cj.supportedInterfaces) ? cj.supportedInterfaces : [];
  const iface = ifaces.find((i) => String(i?.protocolBinding).toUpperCase() === 'JSONRPC') ?? ifaces[0];
  const bearerScheme = cj.securitySchemes?.bearer?.httpAuthSecurityScheme?.scheme ?? cj.securitySchemes?.bearer?.scheme;
  const cardJsonOk = cardJson.code === 0 && cj.name === 'Patch'
    && String(iface?.protocolVersion) === '1.0'
    && /\/a2a$/.test(iface?.url ?? '')
    && String(iface?.protocolBinding).toUpperCase() === 'JSONRPC'
    && cj.capabilities?.streaming === true
    && String(bearerScheme).toLowerCase() === 'bearer'
    && Array.isArray(cj.skills) && cj.skills.some((s) => s?.id === 'project-assistant');
  record('cli.card.json', '`patch card --json` is a valid A2A v1.0 AgentCard (name/supportedInterfaces[protocolVersion,url,JSONRPC]/streaming/bearer/skill)', cardJsonOk, `name=${cj.name} pv=${iface?.protocolVersion} url=${iface?.url} binding=${iface?.protocolBinding} streaming=${cj.capabilities?.streaming} bearer=${bearerScheme}`);

  // C-chat: wipe sink, then one plain chat turn.
  await wipeSink();
  const chat = await runCli(['chat', PROMPT, '--project', PROJECT]);
  writeFileSync(join(OUT_DIR, 'cli-chat.txt'), chat.stdout + '\n---STDERR---\n' + chat.stderr);
  const markerHits = FINDING_MARKERS.filter((m) => chat.stdout.includes(m));
  record('cli.chat.exit', '`patch chat` exits 0', chat.code === 0, `exit=${chat.code}`);
  record('cli.chat.findings', '`patch chat` streams StreamCo findings to stdout', markerHits.length > 0, `matched: [${markerHits.join(', ')}]`);

  // Sink correlation: all events from this turn share one contextId, incl. a
  // tool-invocation (service dim) + token meters.
  const evs = await sinkEvents();
  writeFileSync(join(OUT_DIR, 'cli-sink-events.json'), JSON.stringify(evs, null, 2));
  const ctxIds = [...new Set(evs.map((e) => e?.data?.resource?.name).filter(Boolean))];
  const toolInv = evs.filter((e) => e?.type === 'assistant.miloapis.com/conversation/tool-invocations' && e?.data?.dimensions?.service === 'streaming.streamco.example' && e?.subject === `projects/${PROJECT}`);
  const tokenMeters = evs.filter((e) => /conversation\/(input|output)-tokens/.test(e?.type ?? '') && e?.subject === `projects/${PROJECT}`);
  record('cli.sink.onecontext', 'sink events for the CLI turn share exactly one contextId', ctxIds.length === 1, `contextIds=${JSON.stringify(ctxIds)}`);
  record('cli.sink.toolinvocation', 'sink has tool-invocation (service=streaming.streamco.example, subject=projects/' + PROJECT + ') for the CLI turn', toolInv.length >= 1, `count=${toolInv.length}`);
  record('cli.sink.tokens', 'sink has token meters for the CLI turn', tokenMeters.length >= 1, `count=${tokenMeters.length} types=[${[...new Set(tokenMeters.map((e) => e.type))].join(', ')}]`);

  // C-json: `--json` yields parseable A2A events incl. a contextId.
  const jsonChat = await runCli(['chat', PROMPT, '--project', PROJECT, '--json']);
  writeFileSync(join(OUT_DIR, 'cli-chat-json.txt'), jsonChat.stdout + '\n---STDERR---\n' + jsonChat.stderr);
  const jsonEvents = jsonChat.stdout.split('\n').map((l) => l.trim()).filter(Boolean).map((l) => { try { return JSON.parse(l); } catch { return undefined; } }).filter(Boolean);
  const jsonCtx = jsonEvents.map((e) => findKey(e, 'contextId')).find(Boolean);
  record('cli.json', '`patch chat --json` prints parseable A2A events with a contextId', jsonChat.code === 0 && jsonEvents.length >= 1 && !!jsonCtx, `exit=${jsonChat.code} events=${jsonEvents.length} contextId=${jsonCtx}`, false);

  // C-badtoken: bad token → non-zero + a clear (non-empty, non-stacktrace) error.
  const bad = await runCli(['chat', PROMPT, '--project', PROJECT], { token: BAD_TOKEN });
  writeFileSync(join(OUT_DIR, 'cli-badtoken.txt'), bad.stdout + '\n---STDERR---\n' + bad.stderr);
  const errText = (bad.stderr + bad.stdout).trim();
  const looksClear = errText.length > 0 && !/^\s*at\s+.*:\d+:\d+/m.test(errText.split('\n')[0] ?? '');
  record('cli.badtoken', 'bad token → non-zero exit with a clear error message', bad.code !== 0 && looksClear, `exit=${bad.code} err="${errText.slice(0, 120).replace(/\n/g, ' ')}"`);
}

// ── No-double-metering assertion (portal leg) ───────────────────────────────
async function noDoubleMetering() {
  const evs = await sinkEvents();
  writeFileSync(join(OUT_DIR, 'portal-sink-events.json'), JSON.stringify(evs, null, 2));
  const usage = evs.filter((e) => /assistant\.miloapis\.com\/conversation\//.test(e?.type ?? ''));
  record('portal.sink.nonempty', 'sink captured usage events during the portal conversation (service emitted)', usage.length >= 1, `usage events=${usage.length}`);

  const bySource = {};
  for (const e of usage) bySource[e.source ?? '<none>'] = (bySource[e.source ?? '<none>'] ?? 0) + 1;
  const fromService = usage.filter((e) => (e.source ?? '').includes(SERVICE_SOURCE_MARKER));
  const fromPortal = usage.filter((e) => PORTAL_SOURCE_MARKERS.some((m) => (e.source ?? '').includes(m)));
  record('portal.meter.serviceonly', 'ALL usage events came from the SERVICE (no portal-sourced events)', usage.length >= 1 && fromService.length === usage.length && fromPortal.length === 0, `sources=${JSON.stringify(bySource)}`);
  record('portal.meter.noportalemit', `portal emitted ZERO usage events (no source contains any of [${PORTAL_SOURCE_MARKERS.join(', ')}])`, fromPortal.length === 0, `portal-sourced=${fromPortal.length}`);

  if (EXPECT_CONTEXT_ID) {
    const ctxNames = [...new Set(usage.map((e) => e?.data?.resource?.name).filter(Boolean))];
    record('portal.meter.contextid', 'all portal-conversation usage events carry the pinned contextId', ctxNames.length === 1 && ctxNames[0] === EXPECT_CONTEXT_ID, `contextIds=${JSON.stringify(ctxNames)} expected=${EXPECT_CONTEXT_ID}`);
  }

  // No duplicate (meter,contextId,value) rows — a double-emit would collide here.
  const seen = new Map();
  let dupes = 0;
  for (const e of usage) {
    const key = `${e.type}|${e?.data?.resource?.name}|${e?.data?.value}|${e?.data?.dimensions?.service ?? e?.data?.dimensions?.model ?? ''}`;
    seen.set(key, (seen.get(key) ?? 0) + 1);
  }
  for (const [, n] of seen) if (n > 1) dupes += 1;
  record('portal.meter.nodupes', 'no duplicate (meter,contextId,value) rows (no double emission)', dupes === 0, `duplicate-key groups=${dupes}`);
}

// ── Main ────────────────────────────────────────────────────────────────────
const mode = process.argv[2] ?? 'cli';
const label = mode === 'nodemeter' ? 'nodemeter' : 'cli';
try {
  if (mode === 'cli') await cliLeg();
  else if (mode === 'nodemeter') await noDoubleMetering();
  else {
    console.error(`unknown mode '${mode}' (use: cli | nodemeter)`);
    process.exit(2);
  }
} catch (err) {
  console.error('[consumers-checks] fatal:', err);
  process.exit(2);
}

const requiredFails = results.filter((r) => r.required && !r.ok);
const summary = { mode: label, generatedAt: new Date().toISOString(), totals: { checks: results.length, passed: results.filter((r) => r.ok).length, failedRequired: requiredFails.length }, results };
writeFileSync(join(OUT_DIR, `consumers-${label}-summary.json`), JSON.stringify(summary, null, 2));
appendFileSync(join(OUT_DIR, 'consumers.log'), JSON.stringify(summary) + '\n');
console.log(`\n${requiredFails.length === 0 ? `CONSUMERS ${label.toUpperCase()} PASS` : `CONSUMERS ${label.toUpperCase()} FAIL (${requiredFails.length} required)`} — ${summary.totals.passed}/${summary.totals.checks} checks passed`);
process.exit(requiredFails.length === 0 ? 0 : 1);
