// src/a2a/client.ts
//
// A tiny A2A HTTP client — the shared protocol layer for consumers of the
// service (the `patch` CLI today; other consumers later). It reuses the
// service's own A2A types and JSON-RPC framing (src/a2a/{types,jsonrpc,
// agent-card}) so there is ONE definition of the wire shapes — no
// duplicate protocol code.
//
// It is transport-injectable (`fetchImpl`) so it drives an in-process
// service instance in tests and the real network in production.
import type { AgentCard } from './agent-card';
import type { JsonRpcId, JsonRpcResponse } from './jsonrpc';
import type {
  Message,
  MessageSendParams,
  StreamEvent,
  Task,
  TaskQueryParams,
} from './types';

export interface A2AClientOptions {
  /** Service base URL, e.g. http://localhost:7820 (no /a2a suffix). */
  baseUrl: string;
  /** Bearer token; omitted ⇒ requests are unauthenticated (→ 401). */
  token?: string;
  /** Injectable fetch (tests drive an in-process app). Defaults to global fetch. */
  fetchImpl?: typeof fetch;
  /** Injectable id generator for message ids. Defaults to crypto.randomUUID. */
  generateId?: () => string;
}

/** Error carrying the HTTP status and/or JSON-RPC code, for clean CLI exit. */
export class A2AClientError extends Error {
  constructor(
    message: string,
    public readonly status?: number,
    public readonly rpcCode?: number
  ) {
    super(message);
    this.name = 'A2AClientError';
  }
}

const WELL_KNOWN_CARD_PATH = '/.well-known/agent-card.json';

export class A2AClient {
  private readonly baseUrl: string;
  private readonly a2aUrl: string;
  private readonly cardUrl: string;
  private readonly token?: string;
  private readonly fetchImpl: typeof fetch;
  private readonly generateId: () => string;
  private idCounter = 0;

  constructor(options: A2AClientOptions) {
    this.baseUrl = options.baseUrl.replace(/\/$/, '');
    this.a2aUrl = `${this.baseUrl}/a2a`;
    this.cardUrl = `${this.baseUrl}${WELL_KNOWN_CARD_PATH}`;
    this.token = options.token;
    this.fetchImpl = options.fetchImpl ?? fetch;
    this.generateId = options.generateId ?? (() => crypto.randomUUID());
  }

  /** GET the A2A agent card. */
  async agentCard(): Promise<AgentCard> {
    const res = await this.fetchImpl(this.cardUrl, { headers: { accept: 'application/json' } });
    if (!res.ok) throw await errorFromResponse(res);
    return (await res.json()) as AgentCard;
  }

  /** message/send — run the task to completion and return the final Task. */
  async messageSend(params: MessageSendParams): Promise<Task> {
    return this.rpc<Task>('message/send', params);
  }

  /**
   * message/stream — yield A2A stream events (Task, status-update,
   * artifact-update) as they arrive. Throws A2AClientError on an auth or
   * validation failure surfaced before/at the start of the stream.
   */
  async *messageStream(params: MessageSendParams): AsyncGenerator<StreamEvent> {
    const res = await this.fetchImpl(this.a2aUrl, {
      method: 'POST',
      headers: this.headers(),
      body: JSON.stringify(this.envelope('message/stream', params)),
    });
    if (!res.ok) throw await errorFromResponse(res);

    const contentType = res.headers.get('content-type') ?? '';
    // Validation failures (e.g. missing projectName) come back as an
    // HTTP-200 JSON-RPC error, not an SSE stream — surface them.
    if (contentType.includes('application/json')) {
      const json = (await res.json()) as JsonRpcResponse<StreamEvent>;
      if ('error' in json) {
        throw new A2AClientError(json.error.message, res.status, json.error.code);
      }
      if (json.result) yield json.result;
      return;
    }
    if (!res.body) throw new A2AClientError('service returned an empty stream body', res.status);
    yield* readJsonRpcSse(res.body);
  }

  /** tasks/get. */
  async taskGet(id: string, historyLength?: number): Promise<Task> {
    const params: TaskQueryParams = { id, ...(historyLength !== undefined ? { historyLength } : {}) };
    return this.rpc<Task>('tasks/get', params);
  }

  /** tasks/cancel. */
  async taskCancel(id: string): Promise<Task> {
    return this.rpc<Task>('tasks/cancel', { id });
  }

  /** Build message/send|stream params from user text + project. */
  buildMessageParams(text: string, projectName: string, contextId?: string): MessageSendParams {
    const message: Message = {
      kind: 'message',
      role: 'user',
      parts: [{ kind: 'text', text }],
      messageId: this.generateId(),
      ...(contextId ? { contextId } : {}),
      metadata: { projectName },
    };
    return { message };
  }

  // ── internals ─────────────────────────────────────────────

  private async rpc<T>(method: string, params: unknown): Promise<T> {
    const res = await this.fetchImpl(this.a2aUrl, {
      method: 'POST',
      headers: this.headers(),
      body: JSON.stringify(this.envelope(method, params)),
    });
    if (!res.ok) throw await errorFromResponse(res);
    const json = (await res.json()) as JsonRpcResponse<T>;
    if ('error' in json) {
      throw new A2AClientError(json.error.message, res.status, json.error.code);
    }
    return json.result;
  }

  private envelope(method: string, params: unknown): {
    jsonrpc: '2.0';
    id: JsonRpcId;
    method: string;
    params: unknown;
  } {
    return { jsonrpc: '2.0', id: ++this.idCounter, method, params };
  }

  private headers(): Record<string, string> {
    const headers: Record<string, string> = { 'content-type': 'application/json' };
    if (this.token) headers.authorization = `Bearer ${this.token}`;
    return headers;
  }
}

/**
 * Parse a JSON-RPC-over-SSE response body into a stream of A2A events.
 * Each SSE frame carries `data: {jsonrpc, id, result: <event>}`; a frame
 * whose payload is a JSON-RPC error throws. Handles frames split across
 * chunk boundaries.
 */
export async function* readJsonRpcSse(
  body: ReadableStream<Uint8Array>
): AsyncGenerator<StreamEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let sep: number;
      while ((sep = buffer.indexOf('\n\n')) !== -1) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const event = parseSseFrame(frame);
        if (event) yield event;
      }
    }
    buffer += decoder.decode();
    const event = parseSseFrame(buffer);
    if (event) yield event;
  } finally {
    reader.releaseLock();
  }
}

function parseSseFrame(frame: string): StreamEvent | undefined {
  const dataPayload = frame
    .split('\n')
    .filter((line) => line.startsWith('data:'))
    .map((line) => line.slice('data:'.length).replace(/^ /, ''))
    .join('\n')
    .trim();
  if (!dataPayload) return undefined;

  let parsed: JsonRpcResponse<StreamEvent>;
  try {
    parsed = JSON.parse(dataPayload) as JsonRpcResponse<StreamEvent>;
  } catch {
    throw new A2AClientError(`malformed SSE frame: ${dataPayload.slice(0, 120)}`);
  }
  if ('error' in parsed) {
    throw new A2AClientError(parsed.error.message, undefined, parsed.error.code);
  }
  return parsed.result;
}

async function errorFromResponse(res: Response): Promise<A2AClientError> {
  const body = await res.text().catch(() => '');
  let message = body;
  try {
    const json = JSON.parse(body) as { error?: unknown };
    if (typeof json.error === 'string') message = json.error;
    else if (json.error && typeof json.error === 'object' && 'message' in json.error) {
      message = String((json.error as { message: unknown }).message);
    }
  } catch {
    // non-JSON body; use the raw text
  }
  const detail = message.trim() || res.statusText || 'request failed';
  if (res.status === 401) {
    return new A2AClientError(`unauthorized: ${detail} (check PATCH_TOKEN / --token)`, 401);
  }
  if (res.status === 403) {
    return new A2AClientError(`forbidden: ${detail} (token does not grant this project)`, 403);
  }
  return new A2AClientError(detail, res.status);
}
