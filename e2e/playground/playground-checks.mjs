// Playground e2e assertion driver (QA-owned) — slice 6 / CONTRACT-REAL-ENV.md.
// Mirrors e2e/driver/gateway-checks.mjs conventions (record/finish PASS-FAIL,
// findKey, CLI spawn, access-log JSON parse). Runs against the LIVE persistent
// playground on the shared test-infra kind cluster.
//
// Modes:
//   chat        P2 — host `patch chat` (gateway mode) streams StreamCo findings
//               through the gateway; exit 0; capture the contextId for P4.
//   reconfig    P3 (OVERLAY) — capability documents from the provider adapter
//               reflect a live ServiceAgentConfiguration change: v2's narrower
//               toolSelector shows fewer tools than v1, the next chat turn's
//               tool set matches, and after unpublish the capabilities are gone.
//   tokens      P4 — gateway access log has llm records for the chat's
//               contextId, each carrying x_datum_* attribution, tokens > 0.
//   entitlement P5 (OVERLAY) — an UNENTITLED project gets zero capability
//               documents from the provider (and no agent capabilities in chat).
//   sink        P6 — the usage sink shows SERVICE-emitted usage events
//               (input/output tokens + tool-invocations) for a playground chat.
//
// Every mode writes out/playground-<mode>-summary.json and exits 0 only when all
// REQUIRED checks pass. Honesty over green: a missing interface fails LOUDLY
// with the reason, it does not silently pass.
import { spawn } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

// ── Config (env; run-proofs.sh supplies live values) ────────────────────────
const OUT_DIR = process.env.OUT_DIR ?? join(process.cwd(), 'out');
const SINK_URL = (process.env.SINK_URL ?? 'http://127.0.0.1:7811').replace(/\/$/, '');
const PROJECT = process.env.PROJECT ?? 'demo-project';
const UNENTITLED_PROJECT = process.env.UNENTITLED_PROJECT ?? 'unentitled-project';
const PROMPT = process.env.PROMPT ?? 'Diagnose pipeline p-1 for StreamCo';
const FINDING_MARKERS = (process.env.FINDING_MARKERS ?? 'CONSUMER_LAG,vod-transcode,p-1')
  .split(',').map((s) => s.trim()).filter(Boolean);
const CTX_FILE = join(OUT_DIR, 'playground-contextid.txt');

// CLI (gateway mode; PATCH_URL/PATCH_TOKEN in env)
const PATCH_CMD = process.env.PATCH_CMD ?? 'patch';
const PATCH_URL = (process.env.PATCH_URL ?? 'http://127.0.0.1:7820').replace(/\/$/, '');
const PATCH_TOKEN = process.env.PATCH_TOKEN ?? 'e2e-token';
const CLI_TIMEOUT_MS = Number(process.env.CLI_TIMEOUT_MS ?? 45_000);

// Capability-provider adapter (component 5): the published capability-document
// contract, GET {CAPABILITY_PROVIDER_URL}/projects/{name}/capability-documents.
const CAPABILITY_PROVIDER_URL = (process.env.CAPABILITY_PROVIDER_URL ?? 'http://127.0.0.1:8085').replace(/\/$/, '');

// P3 reconfig expectations: after applying the v2 (narrower) config, the tool
// set should SHRINK relative to v1. The raw capability doc carries BARE tool
// names (toolSelector.include[]); exact names come from pg-catalog's sample CRs
// and are confirmed live (these exact-match checks are informational — the
// required proof is the structural narrowing, not the literal names).
// pg-catalog (corrected): gateway-federated tool names carry the streamco-backend__
// prefix. v2 narrows to the single prefixed streams_list. These exact-name checks
// are informational; the required proof is the structural narrowing.
const V1_EXPECTED_TOOLS = (process.env.V1_EXPECTED_TOOLS ??
  'streamco-backend__streams_list,streamco-backend__streams_get,streamco-backend__pipeline_diagnose')
  .split(',').map((s) => s.trim()).filter(Boolean).sort();
const V2_EXPECTED_TOOLS = (process.env.V2_EXPECTED_TOOLS ??
  'streamco-backend__streams_list')
  .split(',').map((s) => s.trim()).filter(Boolean).sort();
// The chat-turn "next turn reflects it" sub-check is only meaningful when the
// in-cluster assistant reads the adapter live (not fixture-mode). run-proofs.sh
// sets CHAT_TURN_REQUIRED=0 when the assistant is fixture-mode, downgrading that
// one sub-check to a non-required WARN (the doc-level reconfiguration still proves).
const CHAT_TURN_REQUIRED = (process.env.CHAT_TURN_REQUIRED ?? '1') !== '0';

// P4 access log
const ACCESS_LOG_FILE = process.env.ACCESS_LOG_FILE ?? '';
let EXPECT_CONTEXT_ID = process.env.EXPECT_CONTEXT_ID ?? '';

mkdirSync(OUT_DIR, { recursive: true });

// ── Shared helpers (mirrors gateway-checks.mjs) ─────────────────────────────
function findKey(node, key) {
  if (node == null || typeof node !== 'object') return undefined;
  if (Array.isArray(node)) { for (const el of node) { const r = findKey(el, key); if (r !== undefined) return r; } return undefined; }
  for (const [k, v] of Object.entries(node)) {
    if (k === key && (typeof v === 'string' || typeof v === 'number')) return String(v);
    const r = findKey(v, key); if (r !== undefined) return r;
  }
  return undefined;
}
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

const results = [];
function record(item, name, ok, detail, required = true) {
  results.push({ item, name, ok: !!ok, required, detail: String(detail).slice(0, 500) });
  (ok ? console.log : console.error)(`${ok ? 'PASS' : required ? 'FAIL' : 'WARN'}  [${item}] ${name} — ${detail}`);
}
function finish(label) {
  const reqFails = results.filter((r) => r.required && !r.ok);
  writeFileSync(join(OUT_DIR, `playground-${label}-summary.json`), JSON.stringify({ label, results }, null, 2));
  appendFileSync(join(OUT_DIR, 'playground.log'), JSON.stringify({ label, results }) + '\n');
  console.log(`\n${reqFails.length === 0 ? `PLAYGROUND ${label.toUpperCase()} PASS` : `PLAYGROUND ${label.toUpperCase()} FAIL (${reqFails.length} req)`} — ${results.filter((r) => r.ok).length}/${results.length}`);
  process.exit(reqFails.length === 0 ? 0 : 1);
}

function runCli(args, extraEnv = {}) {
  const words = PATCH_CMD.trim().split(/\s+/);
  return new Promise((resolve) => {
    const child = spawn(words[0], [...words.slice(1), ...args], {
      env: { ...process.env, PATCH_URL, PATCH_TOKEN, ...extraEnv },
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

// Run a chat turn, return {code, events[], contextId, text}. `--json` streams
// one A2A event per line; we parse leniently and flatten for marker matching.
async function chatTurn(project, prompt = PROMPT) {
  const res = await runCli(['chat', prompt, '--project', project, '--json']);
  const events = res.stdout.split('\n').map((l) => l.trim()).filter(Boolean)
    .map((l) => { try { return JSON.parse(l); } catch { return undefined; } }).filter(Boolean);
  const contextId = events.map((e) => findKey(e, 'contextId')).find(Boolean);
  return { code: res.code, events, contextId, text: JSON.stringify(events), raw: res.stdout, stderr: res.stderr };
}

// GET the capability documents the provider adapter serves for a project.
// Returns { ok, status, docs, toolNames[] } — toolNames flattened across docs.
async function fetchCapabilityDocs(project) {
  const url = `${CAPABILITY_PROVIDER_URL}/projects/${encodeURIComponent(project)}/capability-documents`;
  try {
    const res = await fetch(url, { headers: { accept: 'application/json' } });
    const status = res.status;
    let body; try { body = await res.json(); } catch { body = await res.text().catch(() => ''); }
    const toolNames = [...stringsByKeySubstr(body, 'name')].filter((n) => /__|_/.test(n));
    // A capability doc set can be an array, {documents:[...]}, or {capabilities:[...]}.
    const docs = Array.isArray(body) ? body : (body?.documents ?? body?.capabilities ?? (body ? [body] : []));
    return { ok: res.ok, status, docs, body, url };
  } catch (e) { return { ok: false, status: 0, docs: [], body: `threw: ${e.message}`, url }; }
}

// Pull the tool allow-list out of a capability-doc response. Per pg-assistant
// (code-confirmed): the tools live at spec.tools.mcpServers[].toolSelector.
// include[]. mcpServers[].name is the SERVER name, NOT a tool — do not harvest
// it. include[] entries are BARE tool names in the raw doc; the model-visible
// surface namespaces them <server>__<tool>. Best-effort, order-independent: we
// collect every toolSelector.include[] array anywhere in the payload.
function toolsFromDocs(docs) {
  const set = new Set();
  const walk = (n) => {
    if (n == null || typeof n !== 'object') return;
    if (Array.isArray(n)) { n.forEach(walk); return; }
    // toolSelector.include[] is the authoritative tool list.
    if (n.toolSelector && Array.isArray(n.toolSelector.include)) {
      n.toolSelector.include.forEach((t) => typeof t === 'string' && set.add(t));
    }
    for (const v of Object.values(n)) walk(v);
  };
  walk(docs);
  return [...set].sort();
}
// Normalize a tool name to its bare form (strip any <server>__ namespacing), so
// a raw capability-doc name (bare) and a model-visible name (<server>__<tool>)
// compare equal. Used only for the cross-surface next-turn check.
function bare(name) { const i = String(name).lastIndexOf('__'); return i >= 0 ? String(name).slice(i + 2) : String(name); }

// ── P2: chat through the gateway ────────────────────────────────────────────
async function chat() {
  const t = await chatTurn(PROJECT);
  writeFileSync(join(OUT_DIR, 'playground-cli-chat.txt'), t.raw + '\n---STDERR---\n' + t.stderr);
  const hits = FINDING_MARKERS.filter((m) => t.text.includes(m));
  record('pg.chat.exit', 'patch chat (playground gateway mode) exits 0', t.code === 0, `exit=${t.code}`);
  record('pg.chat.findings', 'chat streams StreamCo findings (host→gateway→stub + MCP via gateway)', hits.length > 0, `matched [${hits.join(', ')}]`);
  record('pg.chat.context', 'captured the conversation contextId', !!t.contextId, `contextId=${t.contextId}`);
  if (t.contextId) writeFileSync(CTX_FILE, t.contextId);
  finish('chat');
}

// ── P3: live reconfiguration (OVERLAY) ──────────────────────────────────────
// run-proofs.sh applies v1, calls this with STAGE=v1; applies v2, STAGE=v2;
// unpublishes, STAGE=unpublished. Each stage captures a document set; the final
// stage compares them. We persist per-stage tool sets to out/ across invocations.
async function reconfig() {
  const stage = process.env.STAGE ?? 'compare';
  const stageFile = (s) => join(OUT_DIR, `playground-reconfig-${s}.json`);

  if (stage === 'v1' || stage === 'v2' || stage === 'unpublished') {
    const cap = await fetchCapabilityDocs(PROJECT);
    const tools = toolsFromDocs(cap.docs);
    // Behavioral signal for "next chat turn reflects it": the A2A CLI stream does
    // NOT expose tool names (only the result text), so we detect whether the turn
    // actually RAN pipeline_diagnose by its findings in the answer. The prompt
    // ("Diagnose pipeline p-1") needs pipeline_diagnose; CONSUMER_LAG appears ONLY
    // when that tool ran. So v1 (has pipeline_diagnose) → diagnosed; v2 (streams_list
    // only) → NOT diagnosed. Retry once — a turn fired immediately after a reconcile
    // can transiently miss.
    let turn = await chatTurn(PROJECT);
    let diagnosed = /CONSUMER_LAG/.test(turn.text);
    if (!diagnosed && stage === 'v1') { await new Promise((r) => setTimeout(r, 4000)); turn = await chatTurn(PROJECT); diagnosed = /CONSUMER_LAG/.test(turn.text); }
    writeFileSync(stageFile(stage), JSON.stringify({
      stage, status: cap.status, ok: cap.ok, tools, diagnosed,
      chatExit: turn.code, contextId: turn.contextId, text: turn.text.slice(0, 2000),
    }, null, 2));
    console.log(`stage=${stage} adapter-status=${cap.status} tools=[${tools.join(', ')}] chat-diagnosed(pipeline_diagnose ran)=${diagnosed}`);
    return; // orchestrator applies the next stage, then re-invokes with STAGE=compare
  }

  // compare stage: read the three captures and assert the reconfiguration story.
  const read = (s) => { try { return JSON.parse(readFileSync(stageFile(s), 'utf8')); } catch { return undefined; } };
  const v1 = read('v1'), v2 = read('v2'), un = read('unpublished');
  record('pg.reconfig.captured', 'captured v1, v2, and unpublished capability-document stages', !!v1 && !!v2 && !!un,
    `v1=${!!v1} v2=${!!v2} unpublished=${!!un}`);
  if (!v1 || !v2 || !un) return finish('reconfig');

  // Exact-name expectations are INFORMATIONAL (non-required): the concrete tool
  // names come from pg-catalog's sample CRs and the adapter's namespacing, which
  // I confirm live. The PROOF is the structural narrowing below, which holds
  // regardless of the exact names.
  record('pg.reconfig.v1_tools', `v1 capability docs expose the expected broad set [${V1_EXPECTED_TOOLS.join(', ')}]`,
    JSON.stringify(v1.tools) === JSON.stringify(V1_EXPECTED_TOOLS), `v1 tools=[${v1.tools.join(', ')}]`, false);
  record('pg.reconfig.v2_narrower', 'v2 (narrower toolSelector) exposes STRICTLY FEWER tools than v1, all a subset',
    v1.tools.length > 0 && v2.tools.length < v1.tools.length && v2.tools.every((t) => v1.tools.includes(t)),
    `v1=${v1.tools.length} tools, v2=${v2.tools.length} tools; v1=[${v1.tools.join(', ')}] v2=[${v2.tools.join(', ')}]`);
  record('pg.reconfig.v2_expected', `v2 capability docs match the expected narrowed set [${V2_EXPECTED_TOOLS.join(', ')}]`,
    JSON.stringify(v2.tools) === JSON.stringify(V2_EXPECTED_TOOLS), `v2 tools=[${v2.tools.join(', ')}]`, false);
  // BEHAVIORAL "next chat turn reflects it": the prompt runs pipeline_diagnose,
  // which v1 has and v2 removes. So the v1 turn diagnoses (CONSUMER_LAG present)
  // and the v2 turn does NOT — a positive, tool-behavioral observation, not a
  // vacuous empty-set match. Requires the assistant to be adapter-wired
  // (CHAT_TURN_REQUIRED); otherwise it's informational.
  record('pg.reconfig.next_turn_reflects', 'chat turn reflects the change: v1 turn diagnoses (pipeline_diagnose), v2 turn does NOT',
    v1.diagnosed === true && v2.diagnosed === false,
    `${CHAT_TURN_REQUIRED ? '' : '(assistant fixture-mode — informational) '}v1-diagnosed=${v1.diagnosed} v2-diagnosed=${v2.diagnosed} (v2 removed pipeline_diagnose → cannot diagnose)`,
    CHAT_TURN_REQUIRED);
  // After unpublish, the project's capability documents (and thus agent tools)
  // are gone: empty tool set AND/OR a 404 from the adapter.
  record('pg.reconfig.unpublished_gone', 'after unpublish the project has NO capability documents',
    (un.tools?.length ?? 0) === 0 && (un.turnTools?.length ?? 0) === 0,
    `adapter-status=${un.status} docTools=[${(un.tools ?? []).join(', ')}] turnTools=[${(un.turnTools ?? []).join(', ')}]`);
  finish('reconfig');
}

// ── P4: gateway token attribution ───────────────────────────────────────────
async function tokens() {
  if (!EXPECT_CONTEXT_ID && existsSync(CTX_FILE)) EXPECT_CONTEXT_ID = readFileSync(CTX_FILE, 'utf8').trim();
  if (!ACCESS_LOG_FILE || !existsSync(ACCESS_LOG_FILE)) { record('pg.tokens.log', 'gateway access log present', false, `ACCESS_LOG_FILE=${ACCESS_LOG_FILE}`); return finish('tokens'); }
  const lines = readFileSync(ACCESS_LOG_FILE, 'utf8').split('\n').map((l) => l.trim()).filter(Boolean);
  const recs = lines.map((l) => { const i = l.indexOf('{'); if (i < 0) return undefined; try { return JSON.parse(l.slice(i)); } catch { return undefined; } }).filter(Boolean);
  const convRecs = recs.filter((r) => {
    const convs = [...stringsByKeySubstr(r, 'x_datum_conversation')];
    return EXPECT_CONTEXT_ID ? convs.includes(EXPECT_CONTEXT_ID) : convs.length > 0;
  });
  record('pg.tokens.record', 'gateway access log has llm record(s) for our chat conversation', convRecs.length >= 1,
    `matched=${convRecs.length} contextId=${EXPECT_CONTEXT_ID} (of ${recs.length} records)`);
  if (convRecs.length === 0) return finish('tokens');
  const proj = new Set(), agent = new Set();
  for (const r of convRecs) { for (const v of stringsByKeySubstr(r, 'x_datum_project')) proj.add(v); for (const v of stringsByKeySubstr(r, 'x_datum_agent')) agent.add(v); }
  record('pg.tokens.attribution', 'access log carries x_datum_project + x_datum_agent for the chat', proj.has(PROJECT) && agent.size > 0,
    `project=${[...proj]} agent=${[...agent]}`);
  const gIn = convRecs.reduce((s, r) => s + sumByKeySubstr(r, 'input_tokens').sum, 0);
  const gOut = convRecs.reduce((s, r) => s + sumByKeySubstr(r, 'output_tokens').sum, 0);
  const gTot = convRecs.reduce((s, r) => s + sumByKeySubstr(r, 'total_tokens').sum, 0);
  record('pg.tokens.present', 'gateway counted input/output/total tokens (>0)', gIn > 0 && gOut > 0 && gTot > 0, `input=${gIn} output=${gOut} total=${gTot}`);
  // Bonus cross-check vs sink (non-required — P6 owns the sink assertion).
  const sink = await fetch(`${SINK_URL}/events`).then((r) => r.json()).catch(() => []);
  const forConv = sink.filter((e) => !EXPECT_CONTEXT_ID || e?.data?.resource?.name === EXPECT_CONTEXT_ID);
  const sIn = forConv.filter((e) => /input-tokens/.test(e?.type ?? '')).reduce((s, e) => s + Number(e?.data?.value ?? 0), 0);
  const sOut = forConv.filter((e) => /output-tokens/.test(e?.type ?? '')).reduce((s, e) => s + Number(e?.data?.value ?? 0), 0);
  record('pg.tokens.equal_sink', 'gateway-counted tokens equal the sink self-reported totals', gIn === sIn && gOut === sOut,
    `gateway in/out=${gIn}/${gOut} sink in/out=${sIn}/${sOut} (delta in=${gIn - sIn} out=${gOut - sOut})`, false);
  finish('tokens');
}

// ── P5: entitlement isolation (OVERLAY) ─────────────────────────────────────
async function entitlement() {
  // Control: the entitled project DOES get capability documents (so an empty
  // result for the unentitled project is meaningful, not a dead adapter).
  const entitled = await fetchCapabilityDocs(PROJECT);
  const entitledTools = toolsFromDocs(entitled.docs);
  record('pg.entitlement.control', `entitled project '${PROJECT}' receives capability documents (control)`,
    entitled.ok && entitledTools.length > 0, `status=${entitled.status} tools=[${entitledTools.join(', ')}]`);

  const un = await fetchCapabilityDocs(UNENTITLED_PROJECT);
  const unTools = toolsFromDocs(un.docs);
  // Isolation holds if the adapter returns 404 OR an empty document/tool set —
  // but NOT a transport error (status 0), which would be a false pass.
  const reached = un.status !== 0;
  const empty = (un.status === 404) || (Array.isArray(un.docs) && un.docs.length === 0) || unTools.length === 0;
  record('pg.entitlement.reached', `adapter reachable for the unentitled project query`, reached, `status=${un.status}`);
  record('pg.entitlement.isolated', `UNENTITLED project '${UNENTITLED_PROJECT}' gets NO capability documents`,
    reached && empty, `status=${un.status} tools=[${unTools.join(', ')}]`);

  // A chat turn as the unentitled project must have no agent tool capabilities
  // (either denied, or an answer with zero tool invocations). Non-required: the
  // adapter-level isolation above is the primary proof; the chat depends on a
  // token that scopes to the unentitled project (UNENTITLED_TOKEN).
  const unToken = process.env.UNENTITLED_TOKEN;
  if (unToken) {
    const turn = await chatTurn(UNENTITLED_PROJECT);
    const turnTools = [...stringsByKeySubstr(turn.events, 'name')].filter((n) => /__/.test(n));
    record('pg.entitlement.chat_no_tools', 'unentitled project chat turn invokes no agent tools', turnTools.length === 0,
      `exit=${turn.code} turnTools=[${turnTools.join(', ')}]`, false);
  }
  finish('entitlement');
}

// ── P6: service-emitted usage at the sink ───────────────────────────────────
async function sink() {
  if (!EXPECT_CONTEXT_ID && existsSync(CTX_FILE)) EXPECT_CONTEXT_ID = readFileSync(CTX_FILE, 'utf8').trim();
  const events = await fetch(`${SINK_URL}/events`).then((r) => r.json()).catch((e) => ({ err: e.message }));
  if (!Array.isArray(events)) { record('pg.sink.reachable', 'usage sink /events reachable', false, `resp=${JSON.stringify(events)}`); return finish('sink'); }
  const forConv = events.filter((e) => !EXPECT_CONTEXT_ID || e?.data?.resource?.name === EXPECT_CONTEXT_ID);
  record('pg.sink.events', 'sink captured usage events for the playground conversation', forConv.length > 0,
    `events=${forConv.length} (of ${events.length} total) contextId=${EXPECT_CONTEXT_ID}`);
  if (forConv.length === 0) return finish('sink');
  const hasType = (re) => forConv.some((e) => re.test(e?.type ?? ''));
  record('pg.sink.tokens', 'sink has input-tokens AND output-tokens meters', hasType(/input-tokens/) && hasType(/output-tokens/),
    `types=[${[...new Set(forConv.map((e) => e?.type))].join(', ')}]`);
  record('pg.sink.toolinvocations', 'sink has tool-invocations meter', hasType(/tool-invocations/),
    `present=${hasType(/tool-invocations/)}`);
  // The events are SERVICE-emitted: source ends in /a2a (the assistant service's
  // public base url), proving the service meters (not the CLI/host).
  const sources = [...new Set(forConv.map((e) => e?.source).filter(Boolean))];
  const serviceSourced = sources.length > 0 && sources.every((s) => /\/a2a$/.test(String(s)));
  record('pg.sink.service_sourced', 'usage events are SERVICE-sourced (source ends in /a2a), not host/CLI-emitted', serviceSourced,
    `sources=[${sources.join(', ')}]`);
  // CloudEvent `subject` is TOP-LEVEL (e.subject = "projects/<project>"), not data.subject.
  record('pg.sink.subject', `usage events attributed to subject projects/${PROJECT}`,
    forConv.some((e) => String(e?.subject ?? '').includes(PROJECT)),
    `subjects=[${[...new Set(forConv.map((e) => e?.subject).filter(Boolean))].join(', ')}]`);
  finish('sink');
}

const mode = process.argv[2];
const table = { chat, reconfig, tokens, entitlement, sink };
if (table[mode]) await table[mode]();
else { console.error(`unknown mode '${mode}' (use: ${Object.keys(table).join(' | ')})`); process.exit(2); }
