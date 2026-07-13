import { readJsonRpcSse } from '../src/a2a/client';
import type { StreamEvent } from '../src/a2a/types';
import type { AgentCard } from '../src/a2a/agent-card';
import { renderCard, renderChat, renderTask, type Io } from './render';
import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const FIXTURE = readFileSync(join(import.meta.dir, 'fixtures/chat-stream.sse.txt'), 'utf8');

function captureIo(): { io: Io; out: () => string; err: () => string } {
  const outChunks: string[] = [];
  const errChunks: string[] = [];
  return {
    io: { out: (t) => outChunks.push(t), err: (t) => errChunks.push(t) },
    out: () => outChunks.join(''),
    err: () => errChunks.join(''),
  };
}

/** A ReadableStream of the string, chunked to exercise frame reassembly. */
function streamFromString(s: string, chunkSize: number): ReadableStream<Uint8Array> {
  const bytes = new TextEncoder().encode(s);
  let offset = 0;
  return new ReadableStream({
    pull(controller) {
      if (offset >= bytes.length) {
        controller.close();
        return;
      }
      controller.enqueue(bytes.subarray(offset, offset + chunkSize));
      offset += chunkSize;
    },
  });
}

async function collect<T>(gen: AsyncIterable<T>): Promise<T[]> {
  const out: T[] = [];
  for await (const x of gen) out.push(x);
  return out;
}

describe('readJsonRpcSse (recorded transcript)', () => {
  it('parses every frame from the recorded stream', async () => {
    const events = await collect(readJsonRpcSse(streamFromString(FIXTURE, 4096)));
    const kinds = events.map((e) => e.kind);
    expect(kinds[0]).toBe('task');
    expect(kinds).toContain('status-update');
    expect(kinds).toContain('artifact-update');
    const terminal = events.find(
      (e): e is Extract<StreamEvent, { kind: 'status-update' }> =>
        e.kind === 'status-update' && e.status.state === 'completed'
    );
    expect(terminal?.final).toBe(true);
  });

  it('reassembles frames split across tiny chunk boundaries', async () => {
    const whole = await collect(readJsonRpcSse(streamFromString(FIXTURE, 4096)));
    const split = await collect(readJsonRpcSse(streamFromString(FIXTURE, 7)));
    expect(split.length).toBe(whole.length);
    expect(split.map((e) => e.kind)).toEqual(whole.map((e) => e.kind));
  });
});

describe('renderChat', () => {
  it('streams the answer to stdout, status to stderr, exit 0 on completed', async () => {
    const { io, out, err } = captureIo();
    const code = await renderChat(readJsonRpcSse(streamFromString(FIXTURE, 64)), { json: false }, io);

    expect(code).toBe(0);
    expect(out()).toContain('CONSUMER_LAG');
    expect(out().endsWith('\n')).toBe(true);
    // status transitions land on stderr, not stdout
    expect(err()).toContain('working');
    expect(err()).toContain('completed');
    expect(out()).not.toContain('working');
  });

  it('--json emits raw events to stdout and no decoration', async () => {
    const { io, out, err } = captureIo();
    const code = await renderChat(readJsonRpcSse(streamFromString(FIXTURE, 4096)), { json: true }, io);

    expect(code).toBe(0);
    expect(err()).toBe('');
    const lines = out().trim().split('\n');
    expect(JSON.parse(lines[0]!).kind).toBe('task');
    expect(lines.every((l) => JSON.parse(l).kind !== undefined)).toBe(true);
  });

  it('exits non-zero and prints the error on a failed task', async () => {
    async function* failed(): AsyncGenerator<StreamEvent> {
      yield { kind: 'status-update', taskId: 't', contextId: 'c', status: { state: 'working', timestamp: '' }, final: false };
      yield {
        kind: 'status-update',
        taskId: 't',
        contextId: 'c',
        status: {
          state: 'failed',
          timestamp: '',
          message: { kind: 'message', role: 'agent', messageId: 'm', parts: [{ kind: 'text', text: 'Agent run failed: boom' }] },
        },
        final: true,
      };
    }
    const { io, err } = captureIo();
    const code = await renderChat(failed(), { json: false }, io);
    expect(code).toBe(1);
    expect(err()).toContain('failed');
    expect(err()).toContain('boom');
  });
});

describe('renderCard / renderTask', () => {
  const card: AgentCard = {
    protocolVersion: '1.0',
    name: 'Patch',
    description: 'desc',
    url: 'http://x/a2a',
    preferredTransport: 'JSONRPC',
    version: '0.1.0',
    provider: { organization: 'Datum', url: 'http://datum' },
    capabilities: { streaming: true, pushNotifications: false, stateTransitionHistory: false },
    defaultInputModes: ['text/plain'],
    defaultOutputModes: ['text/plain'],
    securitySchemes: { bearer: { type: 'http', scheme: 'bearer' } },
    security: [{ bearer: [] }],
    skills: [{ id: 'project-assistant', name: 'PA', description: 'd', tags: [] }],
  };

  it('pretty-prints the card and its json form', () => {
    const pretty = captureIo();
    renderCard(card, false, pretty.io);
    expect(pretty.out()).toContain('Patch');
    expect(pretty.out()).toContain('http://x/a2a');
    expect(pretty.out()).toContain('project-assistant');
    expect(pretty.out()).toContain('http bearer');

    const asJson = captureIo();
    renderCard(card, true, asJson.io);
    expect(JSON.parse(asJson.out()).name).toBe('Patch');
  });

  it('renders a task with its answer', () => {
    const { io, out } = captureIo();
    renderTask(
      {
        kind: 'task',
        id: 't-1',
        contextId: 'c-1',
        status: { state: 'completed', timestamp: '' },
        artifacts: [{ artifactId: 'response', parts: [{ kind: 'text', text: 'the answer' }] }],
      },
      false,
      io
    );
    expect(out()).toContain('t-1');
    expect(out()).toContain('completed');
    expect(out()).toContain('the answer');
  });
});
