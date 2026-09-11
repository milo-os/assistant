/**
 * Manual `text/event-stream` parsing over a `fetch` response body.
 *
 * The `sendmessage` subresource (assistant repo Part 1,
 * `internal/apiserver/registry/conversation/`) streams Server-Sent Events in
 * response to a POST, so the native `EventSource` API (GET-only) can't be
 * used. Instead we read `response.body.getReader()` ourselves and split on
 * blank-line-delimited SSE events, extracting `data:` lines per the SSE spec.
 *
 * Event payloads are JSON matching the discriminated union below — the same
 * event vocabulary the A2A path already streams (`internal/a2a/runner.go`'s
 * `OnTextDelta`/`OnToolStart`/`OnToolFinish`, plus a terminal `done`), just
 * carried over SSE instead of JSON-RPC framing.
 */

export interface TextDeltaEvent {
  type: 'text_delta';
  text: string;
}

export interface ToolStartEvent {
  type: 'tool_start';
  id?: string;
  name: string;
  summary?: string;
}

export interface ToolFinishEvent {
  type: 'tool_finish';
  id?: string;
  name: string;
  ok: boolean;
  elapsedMs: number;
}

export interface DoneEvent {
  type: 'done';
  state: 'completed' | 'failed' | 'canceled';
  text?: string;
  error?: string;
}

export type AssistantStreamEvent = TextDeltaEvent | ToolStartEvent | ToolFinishEvent | DoneEvent;

function isAssistantStreamEvent(value: unknown): value is AssistantStreamEvent {
  if (typeof value !== 'object' || value === null) return false;
  const type = (value as { type?: unknown }).type;
  return (
    type === 'text_delta' || type === 'tool_start' || type === 'tool_finish' || type === 'done'
  );
}

/**
 * Parses a single raw SSE event chunk (the text between two blank lines,
 * possibly with a leading `event:` line and one or more `data:` lines) into
 * a JSON payload. Multiple `data:` lines are joined with `\n` per spec.
 */
function parseEventChunk(chunk: string): unknown | undefined {
  const dataLines: string[] = [];
  for (const line of chunk.split('\n')) {
    if (line.startsWith('data:')) {
      dataLines.push(line.slice(5).replace(/^ /, ''));
    }
  }
  if (dataLines.length === 0) return undefined;
  const raw = dataLines.join('\n');
  if (raw === '' || raw === '[DONE]') return undefined;
  try {
    return JSON.parse(raw);
  } catch {
    return undefined;
  }
}

/**
 * Reads `body` as a UTF-8 `text/event-stream` and yields each parsed
 * `AssistantStreamEvent` as it arrives. Malformed/unknown-shaped events are
 * skipped rather than throwing, so one bad frame doesn't kill the turn.
 */
export async function* parseAssistantEventStream(
  body: ReadableStream<Uint8Array>
): AsyncGenerator<AssistantStreamEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder('utf-8');
  let buffer = '';

  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      // SSE events are separated by a blank line; normalize CRLF first.
      buffer = buffer.replace(/\r\n/g, '\n');
      let boundary = buffer.indexOf('\n\n');
      while (boundary !== -1) {
        const chunk = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + 2);
        const payload = parseEventChunk(chunk);
        if (isAssistantStreamEvent(payload)) yield payload;
        boundary = buffer.indexOf('\n\n');
      }
    }

    // Flush a trailing event that wasn't terminated by a final blank line.
    if (buffer.trim() !== '') {
      const payload = parseEventChunk(buffer);
      if (isAssistantStreamEvent(payload)) yield payload;
    }
  } finally {
    reader.releaseLock();
  }
}
