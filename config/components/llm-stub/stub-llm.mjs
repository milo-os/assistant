// stub-llm — a tiny OpenAI-compatible /v1/chat/completions upstream for the
// Envoy AI Gateway e2e (CONTRACT-GATEWAY.md §"Stub LLM upstream").
//
// It stands in for a real model provider so the production metering/policy
// path can be proven with no API key and no model-provider network:
//   * streaming SSE (with a trailing `usage` frame) AND non-streaming JSON,
//     both carrying REAL token counts so the gateway's llmRequestCosts has
//     something true to extract and the service's self-reported usage can be
//     cross-checked against the gateway's;
//   * a deterministic script that mirrors src/agent/mock-model.ts — a
//     "diagnose" prompt with a `*pipeline_diagnose*` tool available yields a
//     single tool call; once the tool result is in the messages it yields a
//     final answer quoting the findings; otherwise a short generic reply;
//   * 401 when the injected credential is absent/wrong — the assistant sends
//     NO key in gateway mode, so a 200 here proves the key was injected by
//     the gateway's BackendSecurityPolicy (and a direct call, bypassing the
//     gateway, 401s).
//
// Zero dependencies (node:http only), so it runs from a ConfigMap-mounted
// single file on a stock node image. Run: node stub-llm.mjs
//
// Env:
//   STUB_PORT       (default 8080)   listen port
//   STUB_HOST       (default 0.0.0.0)
//   STUB_MODEL      (default patch-stub-v1)  id echoed in responses
//   STUB_API_KEY    (default sk-stub-datum-local-key)  the credential the
//                   gateway injects; requests must send `Authorization:
//                   Bearer <STUB_API_KEY>` or get 401.

import { createServer } from 'node:http';

const HOST = process.env.STUB_HOST ?? '0.0.0.0';
const PORT = Number(process.env.STUB_PORT ?? 8080);
const MODEL = process.env.STUB_MODEL ?? 'patch-stub-v1';
const API_KEY = process.env.STUB_API_KEY ?? 'sk-stub-datum-local-key';

function log(msg, extra) {
  const line = `[stub-llm] ${new Date().toISOString()} ${msg}`;
  if (extra !== undefined) console.log(line, JSON.stringify(extra));
  else console.log(line);
}

// ── Deterministic "real" usage ────────────────────────────────
// A rough whitespace tokenizer: stable across runs (the inputs are canned),
// non-zero, and genuinely proportional to content — so the numbers the
// gateway meters are true, not invented constants.
function countTokens(text) {
  if (!text) return 0;
  const t = String(text).trim();
  return t ? t.split(/\s+/).length : 0;
}

function promptTokens(messages, tools) {
  let n = 0;
  for (const m of messages ?? []) {
    n += countTokens(typeof m.content === 'string' ? m.content : JSON.stringify(m.content ?? ''));
    n += 3; // per-message structural overhead (role/formatting), like real tokenizers
  }
  for (const tool of tools ?? []) {
    n += countTokens(tool?.function?.name) + countTokens(tool?.function?.description);
  }
  return n;
}

// ── Prompt inspection (mirrors mock-model.ts) ─────────────────
function latestUserText(messages) {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.role !== 'user') continue;
    if (typeof m.content === 'string') return m.content.trim();
    if (Array.isArray(m.content)) {
      return m.content.filter((p) => p?.type === 'text').map((p) => p.text).join(' ').trim();
    }
  }
  return '';
}

function latestToolResult(messages) {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.role === 'tool') {
      return typeof m.content === 'string' ? m.content : JSON.stringify(m.content ?? '');
    }
  }
  return undefined;
}

function wantsDiagnose(userText) {
  return /diagnose/i.test(userText);
}

// Every user message BEFORE the latest one — i.e. what conversation-history
// replay put back into the prompt (mirrors mockmodel's recall probe).
function priorUserTexts(messages) {
  const texts = [];
  for (const m of messages) {
    if (m.role !== 'user') continue;
    let t = '';
    if (typeof m.content === 'string') t = m.content.trim();
    else if (Array.isArray(m.content)) {
      t = m.content.filter((p) => p?.type === 'text').map((p) => p.text).join(' ').trim();
    }
    if (t) texts.push(t);
  }
  return texts.slice(0, -1);
}

function wantsRecall(userText) {
  return /what did i (say|ask)/i.test(userText);
}

function recallReply(prior) {
  if (prior.length === 0) {
    return "This is the first thing you've said in this conversation — I have no earlier messages from you.";
  }
  return `Earlier in this conversation you said: ${prior.map((t) => `"${t.slice(0, 200)}"`).join(', then ')}.`;
}

function findDiagnoseTool(tools) {
  const fns = (tools ?? []).filter((t) => t?.type === 'function' && t.function?.name);
  return fns.find((t) => /pipeline_diagnose/i.test(t.function.name))?.function.name;
}

function extractPipelineId(userText) {
  const explicit = /\bp-[a-z0-9]+\b/i.exec(userText);
  if (explicit) return explicit[0];
  const afterPipeline = /pipeline\s+([^\s.,;]+)/i.exec(userText);
  if (afterPipeline?.[1]) return afterPipeline[1];
  return 'p-1';
}

// ── Response templates (mirror mock-model.ts) ─────────────────
function summarizeToolResult(toolResultText) {
  const compact = String(toolResultText).replace(/\s+/g, ' ').trim().slice(0, 800);
  return `Ran the pipeline diagnosis. The provider tool reported: ${compact}. In short, that's the signal to chase down.`;
}

function genericReply(userText) {
  if (!userText) {
    return "I'm Patch, the Datum Cloud assistant (stub-llm). Ask me to diagnose a provider pipeline to see the tool path.";
  }
  return `Patch here (stub-llm canned reply). You said: "${userText.slice(0, 200)}". Ask me to diagnose a pipeline to exercise the tool path.`;
}

// Decide the response for a parsed request body.
// Returns { kind: 'text'|'tool', text?, toolName?, toolArgs?, finish }.
function decide(body) {
  const messages = Array.isArray(body.messages) ? body.messages : [];
  const tools = Array.isArray(body.tools) ? body.tools : [];
  const toolResult = latestToolResult(messages);
  if (toolResult !== undefined) {
    return { kind: 'text', text: summarizeToolResult(toolResult), finish: 'stop' };
  }
  const userText = latestUserText(messages);
  if (wantsRecall(userText)) {
    return { kind: 'text', text: recallReply(priorUserTexts(messages)), finish: 'stop' };
  }
  const diagnoseTool = wantsDiagnose(userText) ? findDiagnoseTool(tools) : undefined;
  if (diagnoseTool) {
    return {
      kind: 'tool',
      toolName: diagnoseTool,
      toolArgs: JSON.stringify({ id: extractPipelineId(userText) }),
      finish: 'tool_calls',
    };
  }
  return { kind: 'text', text: genericReply(userText), finish: 'stop' };
}

// ── OpenAI wire shapes ────────────────────────────────────────
const nowSec = () => Math.floor(Date.now() / 1000);
const reqId = () => `chatcmpl-stub-${Math.random().toString(36).slice(2, 10)}`;

function usageBlock(body, decision) {
  const input = promptTokens(body.messages, body.tools);
  const completionText = decision.kind === 'tool' ? decision.toolArgs : decision.text;
  const output = Math.max(1, countTokens(completionText));
  return { prompt_tokens: input, completion_tokens: output, total_tokens: input + output };
}

function nonStreaming(body, decision, usage) {
  const id = reqId();
  const message =
    decision.kind === 'tool'
      ? {
          role: 'assistant',
          content: null,
          tool_calls: [
            {
              id: 'call_stub_0',
              type: 'function',
              function: { name: decision.toolName, arguments: decision.toolArgs },
            },
          ],
        }
      : { role: 'assistant', content: decision.text };
  return {
    id,
    object: 'chat.completion',
    created: nowSec(),
    model: MODEL,
    choices: [{ index: 0, message, finish_reason: decision.finish }],
    usage,
  };
}

function* streamingFrames(body, decision, usage) {
  const id = reqId();
  const created = nowSec();
  const base = { id, object: 'chat.completion.chunk', created, model: MODEL };
  // role delta first
  yield { ...base, choices: [{ index: 0, delta: { role: 'assistant' }, finish_reason: null }] };

  if (decision.kind === 'tool') {
    // Tool call: name+id in the first tool_calls delta, then the arguments.
    yield {
      ...base,
      choices: [
        {
          index: 0,
          delta: {
            tool_calls: [
              { index: 0, id: 'call_stub_0', type: 'function', function: { name: decision.toolName, arguments: '' } },
            ],
          },
          finish_reason: null,
        },
      ],
    };
    yield {
      ...base,
      choices: [
        { index: 0, delta: { tool_calls: [{ index: 0, function: { arguments: decision.toolArgs } }] }, finish_reason: null },
      ],
    };
  } else {
    // Text: a few word-group chunks so the SSE stream is genuinely incremental.
    for (const chunk of chunkText(decision.text)) {
      yield { ...base, choices: [{ index: 0, delta: { content: chunk }, finish_reason: null }] };
    }
  }

  yield { ...base, choices: [{ index: 0, delta: {}, finish_reason: decision.finish }] };
  // Final usage frame (OpenAI stream_options.include_usage shape): empty
  // choices + a usage object. The AI Gateway reads token costs from here.
  yield { ...base, choices: [], usage };
}

function chunkText(text) {
  const words = String(text).split(/(\s+)/).filter((w) => w.length > 0);
  const chunks = [];
  let buf = '';
  let count = 0;
  for (const tok of words) {
    buf += tok;
    if (/\S/.test(tok)) count++;
    if (count >= 6) {
      chunks.push(buf);
      buf = '';
      count = 0;
    }
  }
  if (buf) chunks.push(buf);
  return chunks.length ? chunks : [String(text)];
}

// ── HTTP ──────────────────────────────────────────────────────
function unauthorized(res, reason) {
  const body = JSON.stringify({
    error: { message: reason, type: 'invalid_request_error', code: 'invalid_api_key' },
  });
  res.writeHead(401, { 'content-type': 'application/json' }).end(body);
}

function authOk(req) {
  const auth = req.headers['authorization'];
  return typeof auth === 'string' && auth === `Bearer ${API_KEY}`;
}

const server = createServer((req, res) => {
  const url = new URL(req.url ?? '/', `http://${req.headers.host ?? `${HOST}:${PORT}`}`);
  const path = url.pathname;

  if (req.method === 'GET' && (path === '/health' || path === '/healthz')) {
    res.writeHead(200, { 'content-type': 'text/plain' }).end('ok\n');
    return;
  }

  // Models list — handy for gateway/model probes; also credential-gated.
  if (req.method === 'GET' && path === '/v1/models') {
    if (!authOk(req)) return unauthorized(res, 'missing or invalid api key');
    res.writeHead(200, { 'content-type': 'application/json' }).end(
      JSON.stringify({ object: 'list', data: [{ id: MODEL, object: 'model', owned_by: 'datum-stub' }] }),
    );
    return;
  }

  if (req.method === 'POST' && (path === '/v1/chat/completions' || path === '/chat/completions')) {
    if (!authOk(req)) {
      log('401 chat/completions (no injected key)', { hasAuth: Boolean(req.headers['authorization']) });
      return unauthorized(res, 'missing or invalid api key — the gateway BackendSecurityPolicy must inject it');
    }
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      let body;
      try {
        body = JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
      } catch {
        res.writeHead(400, { 'content-type': 'application/json' }).end(
          JSON.stringify({ error: { message: 'invalid JSON body', type: 'invalid_request_error' } }),
        );
        return;
      }
      const decision = decide(body);
      const usage = usageBlock(body, decision);
      const stream = body.stream === true;
      log('chat/completions', {
        model: body.model,
        stream,
        decision: decision.kind,
        tool: decision.toolName,
        usage,
      });

      if (!stream) {
        res.writeHead(200, { 'content-type': 'application/json' }).end(
          JSON.stringify(nonStreaming(body, decision, usage)),
        );
        return;
      }
      res.writeHead(200, {
        'content-type': 'text/event-stream',
        'cache-control': 'no-cache',
        connection: 'keep-alive',
      });
      for (const frame of streamingFrames(body, decision, usage)) {
        res.write(`data: ${JSON.stringify(frame)}\n\n`);
      }
      res.write('data: [DONE]\n\n');
      res.end();
    });
    return;
  }

  res.writeHead(404, { 'content-type': 'application/json' }).end(
    JSON.stringify({ error: { message: `no route for ${req.method} ${path}`, type: 'invalid_request_error' } }),
  );
});

server.listen(PORT, HOST, () => {
  log(`listening on http://${HOST}:${PORT} (model=${MODEL}; POST /v1/chat/completions)`);
});

for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => {
    log(`received ${sig}, shutting down`);
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 1000).unref();
  });
}
