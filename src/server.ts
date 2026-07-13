// src/server.ts
//
// Hono app: GET /healthz, GET /.well-known/agent-card.json, and the
// POST /a2a JSON-RPC 2.0 endpoint (message/send, message/stream,
// tasks/get, tasks/cancel). Auth is enforced on /a2a only; the health
// check and the agent card are public (clients read the card to learn
// the auth scheme).
//
// `createApp` takes fully-constructed dependencies so tests inject fakes;
// `buildApp` wires the real dependencies from Config.
import { Hono, type Context } from 'hono';
import { runAgentDepsFromConfig } from './agent-deps';
import { buildAgentCard } from './a2a/agent-card';
import {
  errorFrom,
  failure,
  JsonRpcError,
  JSONRPC_METHOD_NOT_FOUND,
  JSONRPC_PARSE_ERROR,
  parseJsonRpcRequest,
  success,
  type JsonRpcId,
} from './a2a/jsonrpc';
import { A2AService } from './a2a/methods';
import { sseStreamFrom } from './a2a/sse';
import type { MessageSendParams, StreamEvent, TaskIdParams, TaskQueryParams } from './a2a/types';
import {
  AuthError,
  createAuthenticator,
  createAuthorizer,
  extractBearerToken,
  type Authenticator,
} from './auth';
import type { Config } from './config';
import { createLogger, type Logger } from './logger';

export interface AppDeps {
  config: Config;
  logger: Logger;
  authenticator: Authenticator;
  service: A2AService;
}

export function createApp(deps: AppDeps): Hono {
  const { config, logger, authenticator, service } = deps;
  const app = new Hono();

  app.get('/healthz', (c) => c.json({ status: 'ok' }));

  app.get('/.well-known/agent-card.json', (c) => c.json(buildAgentCard(config)));
  // Alias used by some A2A clients (pre-1.0 well-known path).
  app.get('/.well-known/agent.json', (c) => c.json(buildAgentCard(config)));

  app.post('/a2a', async (c) => {
    // ── AuthN ────────────────────────────────────────────────
    const token = extractBearerToken(c.req.header('authorization'));
    if (!token) {
      c.header('www-authenticate', 'Bearer');
      return c.json({ error: 'Missing bearer token' }, 401);
    }
    let principal;
    try {
      principal = await authenticator.authenticate(token);
    } catch (err) {
      if (err instanceof AuthError) return authErrorResponse(c, err);
      logger.error('a2a.auth.error', { error: err instanceof Error ? err.message : String(err) });
      return c.json({ error: 'Authentication failed' }, 401);
    }

    // ── Parse JSON-RPC ───────────────────────────────────────
    let request;
    try {
      const raw = await c.req.json();
      request = parseJsonRpcRequest(raw);
    } catch (err) {
      if (err instanceof JsonRpcError) return c.json(errorFrom(null, err));
      return c.json(
        failure(null, { code: JSONRPC_PARSE_ERROR, message: 'Invalid JSON in request body' })
      );
    }

    const id: JsonRpcId = request.id ?? null;

    // ── Dispatch ─────────────────────────────────────────────
    try {
      switch (request.method) {
        case 'message/send': {
          const task = await service.messageSend(request.params as MessageSendParams, principal);
          return c.json(success(id, task));
        }
        case 'message/stream': {
          return await streamResponse(
            c,
            id,
            service.messageStream(request.params as MessageSendParams, principal),
            logger
          );
        }
        case 'tasks/get': {
          const task = await service.tasksGet(request.params as TaskQueryParams, principal);
          return c.json(success(id, task));
        }
        case 'tasks/cancel': {
          const task = await service.tasksCancel(request.params as TaskIdParams, principal);
          return c.json(success(id, task));
        }
        default:
          return c.json(
            failure(id, {
              code: JSONRPC_METHOD_NOT_FOUND,
              message: `Method not found: ${request.method}`,
            })
          );
      }
    } catch (err) {
      if (err instanceof AuthError) return authErrorResponse(c, err);
      logger.error('a2a.method.error', {
        method: request.method,
        error: err instanceof Error ? err.message : String(err),
      });
      return c.json(errorFrom(id, err));
    }
  });

  return app;
}

function authErrorResponse(c: Context, err: AuthError): Response {
  if (err.status === 401) c.header('www-authenticate', 'Bearer');
  return c.json({ error: err.message }, err.status);
}

/**
 * Drive the first event of a message/stream generator eagerly so
 * validation/auth failures become a normal JSON-RPC / HTTP error rather
 * than a malformed SSE stream. Once the first event is in hand, stream
 * it and the remainder as SSE.
 */
async function streamResponse(
  c: Context,
  id: JsonRpcId,
  gen: AsyncGenerator<StreamEvent>,
  logger: Logger
): Promise<Response> {
  let first;
  try {
    first = await gen.next();
  } catch (err) {
    if (err instanceof AuthError) return authErrorResponse(c, err);
    return c.json(errorFrom(id, err));
  }
  if (first.done) {
    return c.json(errorFrom(id, new JsonRpcError(-32603, 'Stream produced no events')));
  }

  const events = prependEvent(first.value, gen);
  const stream = sseStreamFrom(id, events, (err) => {
    logger.error('a2a.stream.error', {
      error: err instanceof Error ? err.message : String(err),
    });
    return undefined;
  });

  return new Response(stream, {
    headers: {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      connection: 'keep-alive',
    },
  });
}

async function* prependEvent(
  first: StreamEvent,
  rest: AsyncGenerator<StreamEvent>
): AsyncGenerator<StreamEvent> {
  yield first;
  yield* rest;
}

/**
 * Wire the real dependency graph from Config: logger, authenticator,
 * binding source, model, usage emitter, A2A service, Hono app.
 */
export function buildApp(config: Config, injectedLogger?: Logger): {
  app: Hono;
  service: A2AService;
  logger: Logger;
} {
  const logger = injectedLogger ?? createLogger(config.logLevel);
  const authenticator = createAuthenticator(config, logger);
  const authorizer = createAuthorizer(config, logger);
  const agentDeps = runAgentDepsFromConfig(config, logger);
  const service = new A2AService({ agentDeps, authorizer, logger });
  const app = createApp({ config, logger, authenticator, service });
  return { app, service, logger };
}
