// A2A e2e assertion driver (QA-owned) — zero external deps, plain Node >= 22.
//
// Exercises the RUNNING assistant service over HTTP with a dev bearer token
// and proves contract items 2-5 (CONTRACT-ASSISTANT.md "Definition of done /
// QA"). run-e2e.sh boots the service + StreamCo + sink and invokes this.
//
// Design notes:
//  - The JSON-RPC endpoint is DISCOVERED from the agent card's `url` field
//    (A2A v1.0), so this driver does not hard-code the /a2a path.
//  - SSE and A2A event field names are the engineer's implementation choice
//    (contract: "best-effort naming per spec; document deviation in README").
//    Assertions are therefore tolerant: they scan structured fields first and
//    fall back to substring checks, and every raw event is written to
//    out/stream-events.jsonl so mismatches are debuggable, not silent.
//  - Exit 0 iff every REQUIRED check passed. Evidence is written under OUT_DIR.

import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';

// ── Config (env, with contract defaults) ────────────────────────────────────
const ASSISTANT_URL = (process.env.ASSISTANT_URL ?? 'http://127.0.0.1:7820').replace(/\/$/, '');
const SINK_URL = (process.env.SINK_URL ?? 'http://127.0.0.1:7811').replace(/\/$/, '');
const PROJECT = process.env.PROJECT ?? 'demo-project';
const GOOD_TOKEN = process.env.GOOD_TOKEN ?? 'e2e-good-token';
const WRONGPROJ_TOKEN = process.env.WRONGPROJ_TOKEN ?? 'e2e-wrongproject-token';
const OUT_DIR = process.env.OUT_DIR ?? join(process.cwd(), 'out');
// Where run-e2e.sh tee'd StreamCo stdout (MCP call log) for item-5 proof.
const STREAMCO_LOG = process.env.STREAMCO_LOG ?? '';
const STREAM_TIMEOUT_MS = Number(process.env.STREAM_TIMEOUT_MS ?? 30_000);
// The user prompt for item 4/5.
const PROMPT = process.env.PROMPT ?? 'Diagnose pipeline p-1 for StreamCo';
// Canned findings markers the final answer / artifact should surface.
const FINDING_MARKERS = ['CONSUMER_LAG', 'vod-transcode', 'p-1', 'runbooks/lag.md'];

mkdirSync(OUT_DIR, { recursive: true });

// ── Result tracking ─────────────────────────────────────────────────────────
const results = [];
function record(item, name, ok, detail, required = true) {
  results.push({ item, name, ok: !!ok, required, detail: String(detail).slice(0, 400) });
  const tag = ok ? 'PASS' : required ? 'FAIL' : 'WARN';
  const line = `${tag}  [item ${item}] ${name} — ${detail}`;
  (ok ? console.log : console.error)(line);
}
function readText(res) {
  return res.text().catch(() => '');
}

// ── JSON-RPC helper ─────────────────────────────────────────────────────────
let rpcId = 0;
async function rpc(endpoint, method, params, { token, accept } = {}) {
  const headers = { 'content-type': 'application/json' };
  if (accept) headers['accept'] = accept;
  if (token) headers['authorization'] = `Bearer ${token}`;
  const body = JSON.stringify({ jsonrpc: '2.0', id: ++rpcId, method, params });
  return fetch(endpoint, { method: 'POST', headers, body });
}

function userMessage(text, { projectName = PROJECT } = {}) {
  return {
    message: {
      role: 'user',
      kind: 'message',
      messageId: randomUUID(),
      parts: [{ kind: 'text', text }],
      metadata: { projectName },
    },
  };
}

// Recursively harvest every string under a `text`/`content` key (tolerant to
// exact artifact/message nesting).
function harvestText(node, acc = []) {
  if (node == null) return acc;
  if (Array.isArray(node)) {
    for (const el of node) harvestText(el, acc);
    return acc;
  }
  if (typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) {
      if ((k === 'text' || k === 'content') && typeof v === 'string') acc.push(v);
      else harvestText(v, acc);
    }
  }
  return acc;
}
function harvestByKey(node, key, acc = new Set()) {
  if (node == null) return acc;
  if (Array.isArray(node)) {
    for (const el of node) harvestByKey(el, key, acc);
    return acc;
  }
  if (typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) {
      if (k === key && (typeof v === 'string' || typeof v === 'number')) acc.add(String(v));
      else harvestByKey(v, key, acc);
    }
  }
  return acc;
}

// ── SSE reader: POST message/stream, yield parsed JSON-RPC event objects ─────
async function readStream(endpoint, params, { token }) {
  const res = await fetch(endpoint, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      accept: 'text/event-stream',
      authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ jsonrpc: '2.0', id: ++rpcId, method: 'message/stream', params }),
  });
  const events = [];
  const meta = { status: res.status, contentType: res.headers.get('content-type') ?? '' };
  if (!res.ok || !res.body) return { meta, events, rawStatus: res.status, errorBody: await readText(res) };

  const decoder = new TextDecoder();
  let buf = '';
  const deadline = Date.now() + STREAM_TIMEOUT_MS;
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), STREAM_TIMEOUT_MS);
  try {
    for await (const chunk of res.body) {
      buf += decoder.decode(chunk, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) !== -1) {
        const rawEvent = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        const dataLines = rawEvent
          .split('\n')
          .filter((l) => l.startsWith('data:'))
          .map((l) => l.slice(5).trim());
        if (dataLines.length === 0) continue;
        const payload = dataLines.join('\n');
        try {
          events.push(JSON.parse(payload));
        } catch {
          events.push({ _unparsed: payload });
        }
      }
      // Stop once we observe a terminal marker to avoid hanging on a
      // server that keeps the connection open. A2A v1.0 (a2a-go v2): there is
      // NO `final` flag — the stream closes on a terminal TASK_STATE_* enum.
      const rawSoFar = JSON.stringify(events);
      if (/"state"\s*:\s*"TASK_STATE_(COMPLETED|CANCELED|FAILED|REJECTED)"/.test(rawSoFar)) {
        break;
      }
      if (Date.now() > deadline) break;
    }
  } catch (err) {
    meta.readError = err instanceof Error ? err.message : String(err);
  } finally {
    clearTimeout(timer);
  }
  return { meta, events };
}

// ── Main ────────────────────────────────────────────────────────────────────
async function main() {
  // Fresh sink slate so item-5 assertions only see this run's events.
  await fetch(`${SINK_URL}/events`, { method: 'DELETE' }).catch(() => {});

  // ---- Item 2: agent card ---------------------------------------------------
  let a2aEndpoint = `${ASSISTANT_URL}/a2a`;
  const cardRes = await fetch(`${ASSISTANT_URL}/.well-known/agent-card.json`);
  const cardBody = await readText(cardRes);
  let card = {};
  try {
    card = JSON.parse(cardBody);
  } catch {
    /* leave {} */
  }
  writeFileSync(join(OUT_DIR, 'agent-card.json'), cardBody);
  record('2', 'GET agent card returns 200 JSON', cardRes.ok && typeof card === 'object', `status=${cardRes.status}`);
  record('2', 'card.name = "Patch"', card.name === 'Patch', `name=${card.name}`);
  record('2', 'card.protocolVersion = "1.0"', String(card.protocolVersion) === '1.0', `protocolVersion=${card.protocolVersion}`);
  record('2', 'card.capabilities.streaming = true', card.capabilities?.streaming === true, `streaming=${card.capabilities?.streaming}`);
  const provName = card.provider?.organization ?? card.provider?.name ?? card.provider;
  record('2', 'card.provider is Datum', String(provName).toLowerCase().includes('datum'), `provider=${JSON.stringify(card.provider)}`, false);
  // securitySchemes: A2A v1.0 uses an object map of {type:"http",scheme:"bearer"}.
  const schemes = card.securitySchemes ?? card.security ?? {};
  const schemeVals = Array.isArray(schemes) ? schemes : Object.values(schemes ?? {});
  const hasBearer = schemeVals.some(
    (s) => (s?.type === 'http' && String(s?.scheme).toLowerCase() === 'bearer') || String(s?.scheme).toLowerCase() === 'bearer',
  );
  record('2', 'card advertises HTTP bearer security scheme', hasBearer, JSON.stringify(schemes).slice(0, 200));
  const skills = Array.isArray(card.skills) ? card.skills : [];
  const hasProjectSkill = skills.some((s) => /project-assistant/i.test(`${s?.id} ${s?.name}`));
  record('2', 'card.skills includes project-assistant', hasProjectSkill, `skills=${skills.map((s) => s?.id).join(', ')}`);
  if (typeof card.url === 'string' && /^https?:\/\//.test(card.url)) {
    a2aEndpoint = card.url;
    record('2', 'card.url points to JSON-RPC endpoint (used for a2a calls)', true, card.url, false);
  } else {
    record('2', 'card.url present (falling back to /a2a)', false, `url=${card.url}`, false);
  }

  // ---- Item 3: auth matrix --------------------------------------------------
  const noAuth = await rpc(a2aEndpoint, 'message/send', userMessage(PROMPT), {});
  record('3', 'no token -> 401', noAuth.status === 401, `status=${noAuth.status}`);
  await readText(noAuth);

  const wrongProj = await rpc(a2aEndpoint, 'message/send', userMessage(PROMPT, { projectName: PROJECT }), {
    token: WRONGPROJ_TOKEN,
  });
  record('3', 'valid token, unauthorized project -> 403', wrongProj.status === 403, `status=${wrongProj.status}`);
  await readText(wrongProj);

  const goodAuth = await rpc(a2aEndpoint, 'message/send', userMessage(PROMPT), { token: GOOD_TOKEN });
  const goodAuthBody = await readText(goodAuth);
  record('3', 'good token, granted project -> 200', goodAuth.status === 200, `status=${goodAuth.status}`);
  writeFileSync(join(OUT_DIR, 'message-send.json'), goodAuthBody);

  // ---- Item 4: message/stream lifecycle -------------------------------------
  const { meta, events } = await readStream(a2aEndpoint, userMessage(PROMPT), { token: GOOD_TOKEN });
  writeFileSync(join(OUT_DIR, 'stream-meta.json'), JSON.stringify(meta, null, 2));
  const jsonl = events.map((e) => JSON.stringify(e)).join('\n');
  writeFileSync(join(OUT_DIR, 'stream-events.jsonl'), jsonl + (jsonl ? '\n' : ''));
  record('4', 'message/stream responds text/event-stream', /event-stream/.test(meta.contentType), `contentType=${meta.contentType} status=${meta.status}`);
  record('4', 'stream produced >=1 event', events.length > 0, `events=${events.length}`);
  const rawAll = JSON.stringify(events);
  // A2A v1.0 wire (a2a-go v2): task lifecycle states are TASK_STATE_* enums,
  // stream events are a StreamResponse oneOf envelope (task / statusUpdate /
  // artifactUpdate / message), and the v0.3-shaped `kind` discriminator and
  // `final` flag are GONE (deliberate breaking change vs the TS wire).
  record('4', 'stream shows a working (non-terminal) state (TASK_STATE_WORKING)', /"state"\s*:\s*"TASK_STATE_WORKING"/.test(rawAll), 'looked for state=TASK_STATE_WORKING');
  record('4', 'stream reaches terminal state (TASK_STATE_COMPLETED)', /"state"\s*:\s*"TASK_STATE_COMPLETED"/.test(rawAll), 'looked for state=TASK_STATE_COMPLETED');
  // The v1.0 break, asserted positively: the oneOf envelope key is present and
  // the removed v0.3 fields are absent — the server closes on terminal state.
  const hasOneOf = /"(statusUpdate|artifactUpdate|task|message)"\s*:/.test(rawAll);
  const noKind = !/"kind"\s*:/.test(rawAll);
  const noFinal = !/"final"\s*:/.test(rawAll);
  record('4', 'stream is A2A v1.0-shaped (oneOf StreamResponse, no kind/final)', hasOneOf && noKind && noFinal, `oneOf=${hasOneOf} noKind=${noKind} noFinal=${noFinal}`);
  const streamText = harvestText(events).join('\n');
  const markerHits = FINDING_MARKERS.filter((m) => streamText.includes(m));
  record('4', 'artifact/message text surfaces canned findings', markerHits.length > 0, `matched markers: [${markerHits.join(', ')}]`);

  // Extract a taskId + contextId for tasks/get + tasks/cancel.
  const taskIds = [...harvestByKey(events, 'taskId'), ...harvestByKey(events, 'id')];
  const contextIds = [...harvestByKey(events, 'contextId')];
  const taskId = [...harvestByKey(events, 'taskId')][0] ?? taskIds[0];
  record('4', 'a taskId is observable in the stream', !!taskId, `taskId=${taskId} contextId=${contextIds[0]}`);

  if (taskId) {
    const getRes = await rpc(a2aEndpoint, 'tasks/get', { id: taskId }, { token: GOOD_TOKEN });
    const getBody = await readText(getRes);
    writeFileSync(join(OUT_DIR, 'tasks-get.json'), getBody);
    let getJson = {};
    try {
      getJson = JSON.parse(getBody);
    } catch {
      /* */
    }
    const gotSameTask = JSON.stringify(getJson).includes(taskId);
    const completedState = /"state"\s*:\s*"TASK_STATE_COMPLETED"/.test(getBody);
    record('4', 'tasks/get retrieves the task record', getRes.status === 200 && gotSameTask, `status=${getRes.status} sameId=${gotSameTask} completed=${completedState}`);
  } else {
    record('4', 'tasks/get retrieves the task record', false, 'no taskId observed to query');
  }

  // tasks/cancel on a FRESH task. Start a new stream but cancel as soon as we
  // learn its taskId (best effort — either canceled or a sane not-cancelable
  // JSON-RPC error is acceptable; a 5xx/crash is not).
  {
    const fresh = await readStream(a2aEndpoint, userMessage(PROMPT), { token: GOOD_TOKEN });
    const freshTaskId = [...harvestByKey(fresh.events, 'taskId')][0] ?? [...harvestByKey(fresh.events, 'id')][0];
    if (freshTaskId) {
      const cancelRes = await rpc(a2aEndpoint, 'tasks/cancel', { id: freshTaskId }, { token: GOOD_TOKEN });
      const cancelBody = await readText(cancelRes);
      writeFileSync(join(OUT_DIR, 'tasks-cancel.json'), cancelBody);
      let cancelJson = {};
      try {
        cancelJson = JSON.parse(cancelBody);
      } catch {
        /* */
      }
      const sane =
        cancelRes.status < 500 &&
        (/"state"\s*:\s*"TASK_STATE_CANCELED"/.test(cancelBody) || cancelJson.error != null || cancelRes.status === 200);
      record('4', 'tasks/cancel behaves sanely (canceled or documented error, no 5xx)', sane, `status=${cancelRes.status} body=${cancelBody.slice(0, 160)}`);
    } else {
      record('4', 'tasks/cancel behaves sanely', false, 'could not obtain a fresh taskId to cancel');
    }
  }

  // ---- Item 5: prove the chat path (MCP + usage) ----------------------------
  // MCP proof from StreamCo's own request log (real Streamable-HTTP round-trip).
  if (STREAMCO_LOG) {
    let logText = '';
    try {
      logText = await import('node:fs').then((fs) => fs.readFileSync(STREAMCO_LOG, 'utf8'));
    } catch {
      /* */
    }
    const mcpCalled = /tools\/call pipeline_diagnose id=p-1/.test(logText);
    record('5', 'provider tool call went over real MCP (StreamCo log shows pipeline_diagnose id=p-1)', mcpCalled, mcpCalled ? 'streamco.log matched' : 'no matching StreamCo log line');
  } else {
    record('5', 'provider tool call went over real MCP', false, 'STREAMCO_LOG not provided to driver', false);
  }

  // Usage proof from the sink.
  const sinkEvents = await fetch(`${SINK_URL}/events`).then((r) => r.json()).catch(() => []);
  writeFileSync(join(OUT_DIR, 'sink-events.json'), JSON.stringify(sinkEvents, null, 2));
  const toolInv = sinkEvents.filter(
    (e) => e?.type === 'assistant.miloapis.com/conversation/tool-invocations',
  );
  const toolInvGood = toolInv.filter(
    (e) => e?.data?.dimensions?.service === 'streaming.streamco.example' && e?.subject === `projects/${PROJECT}`,
  );
  record(
    '5',
    'sink captured tool-invocations meter (service=streaming.streamco.example, subject=projects/' + PROJECT + ')',
    toolInvGood.length >= 1,
    `toolInvocation events total=${toolInv.length} matching=${toolInvGood.length}`,
  );
  const tokenMeters = sinkEvents.filter((e) =>
    /assistant\.miloapis\.com\/conversation\/(input-tokens|output-tokens)/.test(e?.type ?? ''),
  );
  const tokenMetersGood = tokenMeters.filter(
    (e) => e?.subject === `projects/${PROJECT}` && e?.data?.resource?.kind === 'Conversation',
  );
  record(
    '5',
    'sink captured token meters (input/output-tokens, subject=projects/' + PROJECT + ', resource.kind=Conversation)',
    tokenMetersGood.length >= 1,
    `token meter events total=${tokenMeters.length} matching=${tokenMetersGood.length} types=[${[...new Set(tokenMeters.map((e) => e.type))].join(', ')}]`,
  );

  // ---- Summary --------------------------------------------------------------
  const requiredFails = results.filter((r) => r.required && !r.ok);
  const summary = {
    generatedAt: new Date().toISOString(),
    assistantUrl: ASSISTANT_URL,
    a2aEndpoint,
    project: PROJECT,
    totals: {
      checks: results.length,
      passed: results.filter((r) => r.ok).length,
      failedRequired: requiredFails.length,
      failedOptional: results.filter((r) => !r.required && !r.ok).length,
    },
    results,
  };
  writeFileSync(join(OUT_DIR, 'driver-summary.json'), JSON.stringify(summary, null, 2));
  appendFileSync(join(OUT_DIR, 'driver.log'), JSON.stringify(summary) + '\n');

  console.log(
    `\n${requiredFails.length === 0 ? 'DRIVER PASS' : `DRIVER FAIL (${requiredFails.length} required check(s) failed)`}` +
      ` — ${summary.totals.passed}/${summary.totals.checks} checks passed`,
  );
  process.exit(requiredFails.length === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('[a2a-checks] fatal:', err);
  process.exit(2);
});
