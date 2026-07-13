// src/a2a/jsonrpc.ts
//
// JSON-RPC 2.0 envelope helpers plus the A2A-specific error codes. The
// method handlers speak in results and JsonRpcError; this module owns
// the wire framing.
export type JsonRpcId = string | number | null;

export interface JsonRpcRequest {
  jsonrpc: '2.0';
  id?: JsonRpcId;
  method: string;
  params?: unknown;
}

export interface JsonRpcErrorObject {
  code: number;
  message: string;
  data?: unknown;
}

export interface JsonRpcSuccess<T> {
  jsonrpc: '2.0';
  id: JsonRpcId;
  result: T;
}

export interface JsonRpcFailure {
  jsonrpc: '2.0';
  id: JsonRpcId;
  error: JsonRpcErrorObject;
}

export type JsonRpcResponse<T = unknown> = JsonRpcSuccess<T> | JsonRpcFailure;

// Standard JSON-RPC 2.0 error codes.
export const JSONRPC_PARSE_ERROR = -32700;
export const JSONRPC_INVALID_REQUEST = -32600;
export const JSONRPC_METHOD_NOT_FOUND = -32601;
export const JSONRPC_INVALID_PARAMS = -32602;
export const JSONRPC_INTERNAL_ERROR = -32603;

// A2A-specific error codes (spec §JSON-RPC errors).
export const A2A_TASK_NOT_FOUND = -32001;
export const A2A_TASK_NOT_CANCELABLE = -32002;
export const A2A_PUSH_NOT_SUPPORTED = -32003;
export const A2A_UNSUPPORTED_OPERATION = -32004;
export const A2A_CONTENT_TYPE_NOT_SUPPORTED = -32005;
export const A2A_INVALID_AGENT_RESPONSE = -32006;

/** A thrown error a method handler uses to signal a JSON-RPC failure. */
export class JsonRpcError extends Error {
  constructor(
    public readonly code: number,
    message: string,
    public readonly data?: unknown
  ) {
    super(message);
    this.name = 'JsonRpcError';
  }
}

export function success<T>(id: JsonRpcId, result: T): JsonRpcSuccess<T> {
  return { jsonrpc: '2.0', id, result };
}

export function failure(id: JsonRpcId, error: JsonRpcErrorObject): JsonRpcFailure {
  return { jsonrpc: '2.0', id, error };
}

export function errorFrom(id: JsonRpcId, err: unknown): JsonRpcFailure {
  if (err instanceof JsonRpcError) {
    return failure(id, { code: err.code, message: err.message, data: err.data });
  }
  return failure(id, {
    code: JSONRPC_INTERNAL_ERROR,
    message: err instanceof Error ? err.message : 'Internal error',
  });
}

/**
 * Validate the shape of a decoded JSON body as a JSON-RPC request.
 * Returns the request or throws JsonRpcError(INVALID_REQUEST).
 */
export function parseJsonRpcRequest(body: unknown): JsonRpcRequest {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) {
    throw new JsonRpcError(JSONRPC_INVALID_REQUEST, 'Request must be a JSON-RPC 2.0 object');
  }
  const obj = body as Record<string, unknown>;
  if (obj.jsonrpc !== '2.0') {
    throw new JsonRpcError(JSONRPC_INVALID_REQUEST, 'Missing or invalid "jsonrpc": must be "2.0"');
  }
  if (typeof obj.method !== 'string' || obj.method.length === 0) {
    throw new JsonRpcError(JSONRPC_INVALID_REQUEST, 'Missing or invalid "method"');
  }
  const id = obj.id;
  if (id !== undefined && id !== null && typeof id !== 'string' && typeof id !== 'number') {
    throw new JsonRpcError(JSONRPC_INVALID_REQUEST, '"id" must be a string, number, or null');
  }
  return { jsonrpc: '2.0', id: id as JsonRpcId, method: obj.method, params: obj.params };
}
