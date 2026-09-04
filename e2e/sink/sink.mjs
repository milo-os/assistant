// Usage capture sink — local stand-in for the in-cluster usage collector
// (Vector -> billing ingestion gateway). Zero dependencies.
//
// Wire contract (mirrors cloud-portal app/modules/usage/emitter.ts):
//   POST /cloudevents   body = JSON array of CloudEvents (<=100/batch);
//                       content-type: application/json; optional x-api-key
//                       header (accepted and ignored here).
//   Response: 200 {"accepted": N} — the portal only checks res.ok.
//
// Each received CloudEvent is appended as one JSON line to the capture file
// (default: ../out/captured-events.jsonl next to this script, override with
// CAPTURE_FILE). Extra read-side endpoints for assertions:
//   GET    /events   -> JSON array of all captured events
//   DELETE /events   -> truncate the capture file (test isolation)
//   GET    /healthz  -> ok
//
// Run: node sink.mjs   (port 7811, override with SINK_PORT)

import { createServer } from 'node:http';
import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HOST = process.env.SINK_HOST ?? '127.0.0.1';
const PORT = Number(process.env.SINK_PORT ?? 7811);
const CAPTURE_FILE =
  process.env.CAPTURE_FILE ??
  join(dirname(fileURLToPath(import.meta.url)), '..', 'out', 'captured-events.jsonl');

mkdirSync(dirname(CAPTURE_FILE), { recursive: true });

function log(message) {
  console.log(`[sink] ${new Date().toISOString()} ${message}`);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (chunk) => {
      size += chunk.length;
      if (size > 5 * 1024 * 1024) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function capturedEvents() {
  if (!existsSync(CAPTURE_FILE)) return [];
  return readFileSync(CAPTURE_FILE, 'utf8')
    .split('\n')
    .filter((line) => line.trim() !== '')
    .map((line) => JSON.parse(line));
}

function respondJson(res, status, value) {
  res.writeHead(status, { 'content-type': 'application/json' });
  res.end(JSON.stringify(value));
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url ?? '/', `http://${req.headers.host ?? `${HOST}:${PORT}`}`);

  try {
    if (req.method === 'POST' && url.pathname === '/cloudevents') {
      const raw = await readBody(req);
      let parsed;
      try {
        parsed = JSON.parse(raw);
      } catch {
        log(`REJECT invalid JSON (${raw.length} bytes)`);
        respondJson(res, 400, { error: 'body must be a JSON array of CloudEvents' });
        return;
      }
      // The portal always posts an array; accept a bare object defensively.
      const events = Array.isArray(parsed) ? parsed : [parsed];
      const lines = events.map((e) => JSON.stringify(e)).join('\n');
      if (events.length > 0) appendFileSync(CAPTURE_FILE, lines + '\n');

      const types = [...new Set(events.map((e) => e?.type ?? '<no type>'))];
      const services = [
        ...new Set(events.map((e) => e?.data?.dimensions?.service).filter(Boolean)),
      ];
      log(
        `accepted batch of ${events.length} event(s) types=[${types.join(', ')}]` +
          (services.length > 0 ? ` service=[${services.join(', ')}]` : ''),
      );
      respondJson(res, 200, { accepted: events.length });
      return;
    }

    if (req.method === 'GET' && url.pathname === '/events') {
      respondJson(res, 200, capturedEvents());
      return;
    }

    if (req.method === 'DELETE' && url.pathname === '/events') {
      writeFileSync(CAPTURE_FILE, '');
      log('capture file truncated');
      respondJson(res, 200, { truncated: true });
      return;
    }

    if (req.method === 'GET' && url.pathname === '/healthz') {
      res.writeHead(200, { 'content-type': 'text/plain' }).end('ok\n');
      return;
    }

    respondJson(res, 404, { error: 'not found' });
  } catch (err) {
    log(`ERROR: ${err instanceof Error ? err.message : String(err)}`);
    if (!res.headersSent) respondJson(res, 500, { error: 'internal error' });
    else res.end();
  }
});

server.listen(PORT, HOST, () => {
  log(`listening on http://${HOST}:${PORT} (POST /cloudevents), capturing to ${CAPTURE_FILE}`);
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    log(`received ${signal}, shutting down`);
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 1500).unref();
  });
}
