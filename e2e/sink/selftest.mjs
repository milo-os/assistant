// Sink self-test: spawns sink.mjs against a temp capture file, POSTs a
// portal-shaped CloudEvents batch, and asserts capture + read-back +
// rejection behavior. Exit 0 = pass.

import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { setTimeout as sleep } from 'node:timers/promises';

const HOST = '127.0.0.1';
const PORT = Number(process.env.SINK_PORT ?? 7811);
const BASE = `http://${HOST}:${PORT}`;

let failures = 0;
function check(name, ok, detail) {
  if (ok) console.log(`PASS  ${name} — ${detail}`);
  else {
    failures += 1;
    console.error(`FAIL  ${name} — ${detail}`);
  }
}

// Shaped exactly like cloud-portal's toCloudEvent output (usage/types.ts).
function sampleEvent(id, tool) {
  return {
    id,
    specversion: '1.0',
    type: 'assistant.miloapis.com/conversation/tool-invocations',
    source: 'http://localhost:3000/api/assistant',
    subject: 'projects/demo-project',
    datacontenttype: 'application/json',
    time: new Date().toISOString(),
    data: {
      value: '1',
      dimensions: { service: 'streaming.streamco.example', tool },
      resource: {
        group: 'assistant.miloapis.com',
        kind: 'Conversation',
        namespace: 'default',
        name: 'selftest-conversation',
      },
    },
  };
}

async function main() {
  const captureFile = join(mkdtempSync(join(tmpdir(), 'sink-selftest-')), 'captured-events.jsonl');
  const sinkPath = join(dirname(fileURLToPath(import.meta.url)), 'sink.mjs');
  const child = spawn(process.execPath, [sinkPath], {
    env: { ...process.env, SINK_HOST: HOST, SINK_PORT: String(PORT), CAPTURE_FILE: captureFile },
    stdio: ['ignore', 'inherit', 'inherit'],
  });

  const deadline = Date.now() + 10_000;
  for (;;) {
    try {
      const res = await fetch(`${BASE}/healthz`, { signal: AbortSignal.timeout(500) });
      if (res.ok) break;
    } catch {
      /* not up yet */
    }
    if (Date.now() > deadline) throw new Error('sink did not become healthy within 10s');
    if (child.exitCode !== null) throw new Error(`sink exited early (${child.exitCode})`);
    await sleep(200);
  }

  try {
    const batch = [
      sampleEvent('01JZSELFTEST0000000000000A', 'streamco__pipeline_diagnose'),
      sampleEvent('01JZSELFTEST0000000000000B', 'streamco__streams_get'),
    ];
    const post = await fetch(`${BASE}/cloudevents`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-api-key': 'ignored-local' },
      body: JSON.stringify(batch),
    });
    const postBody = await post.json();
    check('POST /cloudevents batch', post.status === 200 && postBody.accepted === 2, `status=${post.status} accepted=${postBody.accepted}`);

    const events = await (await fetch(`${BASE}/events`)).json();
    check(
      'GET /events returns captured batch',
      events.length === 2 && events[1].data.dimensions.service === 'streaming.streamco.example',
      `count=${events.length} service=${events[1]?.data?.dimensions?.service}`,
    );

    const lines = readFileSync(captureFile, 'utf8').trim().split('\n');
    check(
      'capture file is JSONL (one event per line)',
      lines.length === 2 && JSON.parse(lines[0]).id === '01JZSELFTEST0000000000000A',
      `${captureFile}: ${lines.length} lines`,
    );

    const bad = await fetch(`${BASE}/cloudevents`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: 'this is not json',
    });
    check('POST invalid JSON -> 400', bad.status === 400, `status=${bad.status}`);

    const wipe = await fetch(`${BASE}/events`, { method: 'DELETE' });
    const after = await (await fetch(`${BASE}/events`)).json();
    check('DELETE /events truncates', wipe.status === 200 && after.length === 0, `remaining=${after.length}`);
  } finally {
    child.kill('SIGTERM');
    await sleep(300);
    if (child.exitCode === null) child.kill('SIGKILL');
  }

  console.log(failures === 0 ? '\nSELFTEST PASS (usage capture sink)' : `\nSELFTEST FAIL: ${failures} check(s) failed`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('[selftest] fatal:', err);
  process.exit(1);
});
