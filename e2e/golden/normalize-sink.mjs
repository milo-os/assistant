// Sink CloudEvent normalizer (QA-owned) — zero deps, plain Node >= 22.
//
// The usage CloudEvents the service posts to the sink must be BYTE-COMPATIBLE
// across the TS→Go port (contract: "the sink asserts this — do not improve it").
// This tool turns a raw capture (out/captured-events.jsonl, one CloudEvent per
// line) into a canonical, volatile-field-normalized golden so the Go emitter's
// wire can be diffed against the TS emitter's recorded wire.
//
// Normalization (only genuinely volatile fields — everything else is asserted):
//   id                 -> "<ULID>"        (per-event ULID)
//   time               -> "<TIME>"        (emit timestamp)
//   data.resource.name -> "<CONTEXTID>"   (conversation id / metering resource)
// Every other field (type, source, subject, datacontenttype, data.value,
// data.dimensions, resource group/kind/namespace, specversion) is preserved
// byte-for-byte and therefore part of the golden contract.
//
// The mock model reports fixed per-step token counts, so all turns normalize to
// an IDENTICAL set of events; the canonical golden is the sorted, de-duplicated
// set (one entry per distinct normalized event). Turn/meter COUNTS are asserted
// separately by the drivers — this golden pins the ENVELOPE bytes.
//
// Usage:
//   node normalize-sink.mjs < raw.jsonl            # canonical golden to stdout
//   node normalize-sink.mjs raw.jsonl golden.jsonl # generate/update a golden file
//   node normalize-sink.mjs --check raw.jsonl golden.jsonl  # diff, exit 1 on drift

import { readFileSync, writeFileSync } from 'node:fs';

function canonicalKeySort(value) {
  // Deterministic key ordering so JSON.stringify is stable regardless of the
  // emitter's field insertion order.
  if (Array.isArray(value)) return value.map(canonicalKeySort);
  if (value && typeof value === 'object') {
    const out = {};
    for (const k of Object.keys(value).sort()) out[k] = canonicalKeySort(value[k]);
    return out;
  }
  return value;
}

function normalizeEvent(ev) {
  const e = structuredClone(ev);
  if ('id' in e) e.id = '<ULID>';
  if ('time' in e) e.time = '<TIME>';
  if (e?.data?.resource && 'name' in e.data.resource) e.data.resource.name = '<CONTEXTID>';
  return e;
}

function readJsonl(text) {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => JSON.parse(l));
}

// Canonical golden = sorted, de-duplicated set of normalized events, each
// serialized with sorted keys.
export function canonicalize(rawEvents) {
  const seen = new Map();
  for (const ev of rawEvents) {
    const line = JSON.stringify(canonicalKeySort(normalizeEvent(ev)));
    seen.set(line, (seen.get(line) ?? 0) + 1);
  }
  return [...seen.keys()].sort();
}

function main() {
  const args = process.argv.slice(2);
  const check = args[0] === '--check';
  const rest = check ? args.slice(1) : args;

  let rawText;
  let goldenPath;
  if (rest.length === 0) {
    rawText = readFileSync(0, 'utf8'); // stdin
  } else {
    rawText = readFileSync(rest[0], 'utf8');
    goldenPath = rest[1];
  }

  const canonical = canonicalize(readJsonl(rawText));
  const out = canonical.join('\n') + (canonical.length ? '\n' : '');

  if (check) {
    if (!goldenPath) {
      console.error('--check requires: --check <raw.jsonl> <golden.jsonl>');
      process.exit(2);
    }
    const golden = readFileSync(goldenPath, 'utf8');
    if (out === golden) {
      console.log(`GOLDEN MATCH — ${canonical.length} canonical event(s) byte-identical to ${goldenPath}`);
      process.exit(0);
    }
    console.error(`GOLDEN DRIFT — canonical capture differs from ${goldenPath}`);
    const goldenLines = new Set(golden.split('\n').filter(Boolean));
    const gotLines = new Set(canonical);
    for (const l of gotLines) if (!goldenLines.has(l)) console.error(`  + only in capture: ${l}`);
    for (const l of goldenLines) if (!gotLines.has(l)) console.error(`  - only in golden:  ${l}`);
    process.exit(1);
  }

  if (goldenPath) {
    writeFileSync(goldenPath, out);
    console.error(`wrote ${canonical.length} canonical event(s) to ${goldenPath}`);
  } else {
    process.stdout.write(out);
  }
}

main();
