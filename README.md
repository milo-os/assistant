# Patch — Assistant Service

Standalone A2A backend for **Patch**, the Datum Cloud assistant. This is
the runtime for the already-registered catalog service
`assistant.miloapis.com`. It was extracted from the cloud-portal, where
Patch was embedded in the portal's Hono backend and bound to a session
cookie — which made the chat path un-scriptable, left conversation state
in browser `localStorage`, and gave the A2A card and per-project MCP
surfaces no owner. This service owns them; the portal becomes one client
of it (the portal conversion is out of scope for this slice).

It speaks the **A2A (Agent2Agent) protocol** over JSON-RPC 2.0, composes
each project's entitled provider capabilities (knowledge + MCP tools)
from `AgentBinding` objects, runs the model loop with the Vercel AI SDK
(`ai@6`), and emits usage as CloudEvents on the Milo billing wire.

## Quickstart

```bash
bun install
cp .env.example .env          # the defaults run a mock-model dev server
bun run start                 # → http://localhost:7820
```

Probe it:

```bash
curl -s localhost:7820/healthz
curl -s localhost:7820/.well-known/agent-card.json | jq .

# message/send (dev token from .env.example, mock model)
curl -s localhost:7820/a2a \
  -H 'authorization: Bearer dev-token' \
  -H 'content-type: application/json' \
  -d '{
    "jsonrpc":"2.0","id":1,"method":"message/send",
    "params":{"message":{
      "kind":"message","role":"user","messageId":"m1",
      "parts":[{"kind":"text","text":"Diagnose pipeline p-1 for StreamCo"}],
      "metadata":{"projectName":"demo-project"}
    }}
  }' | jq .
```

### Runtime

Written in TypeScript, run with **Bun** (`bun run start`) which executes
the TS directly. The HTTP layer uses `@hono/node-server`, so `node`
works too once compiled (or via a TS loader). Tests and typecheck use
Bun (`bun test`, `bun run typecheck`). Requires Bun ≥ 1.3 or Node ≥ 22.

## API surface

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `GET` | `/healthz` | none | `{"status":"ok"}` |
| `GET` | `/.well-known/agent-card.json` | none | A2A v1.0 agent card (also at `/.well-known/agent.json`) |
| `POST` | `/a2a` | bearer | JSON-RPC 2.0 endpoint |

### JSON-RPC methods (`POST /a2a`)

- **`message/send`** — create a task, run the agent loop to completion,
  return the final `Task` (with `artifacts` and `history`).
- **`message/stream`** — same, but stream the run as **Server-Sent
  Events**. Each SSE frame is a JSON-RPC response whose `result` is one
  stream event; the stream closes after the terminal status frame.
  Sequence: the initial `Task` → `status-update` `working` →
  `artifact-update`(s) carrying response text → `status-update`
  `completed` with `final: true`.
- **`tasks/get`** — `{ id, historyLength? }` → the stored `Task`.
- **`tasks/cancel`** — `{ id }` → cancels a non-terminal task; a terminal
  task returns error `-32002` (TaskNotCancelable); an unknown id returns
  `-32001` (TaskNotFound).

Task lifecycle (v0): `submitted → working → completed | failed |
canceled`.

### Scoping

- **`contextId`** = the conversation id, and also the name of the
  `Conversation` metering resource. Supply `message.contextId` to
  continue a conversation; omitted ⇒ the server mints one.
- **Project** comes from `message.metadata.projectName` (see deviations).
  The task runs against that project and all usage is subject-scoped to
  `projects/<projectName>`.

### A2A conformance & deviations

This service follows the A2A v1.0 method names and object shapes so a
conformant client works unchanged, with these v0 deviations (all
intentional, documented here):

- **`protocolVersion: "1.0"`** on the card, per the build contract.
- **`message.metadata.projectName`** is an **extension field** — A2A has
  no notion of a Milo project. It is required; a request without it is
  rejected with `-32602` (Invalid params).
- **AuthN failures are HTTP `401`/`403`**, not JSON-RPC error objects
  (missing/unknown token → 401; a project the token doesn't grant →
  403). Protocol/validation errors use JSON-RPC error objects with HTTP
  200.
- **Task states** are limited to the `submitted/working/completed/
  failed/canceled` subset; `input-required`, `rejected`, and
  `auth-required` are unused in v0.
- **`message/stream`** does not surface intermediate `tool-call` events
  as SSE frames — the metering pipeline and the provider MCP server log
  are the authoritative record of a tool invocation.
- The agent card is **unsigned** (no `signatures`). See Follow-ups.
- `pushNotifications` is `false`; push and `tasks/resubscribe` are out of
  scope.

## Auth (authN + authZ are separate seams)

Two independent interfaces (`src/auth/`):

- **`Authenticator`** — *who are you*: bearer token → `Principal`
  (subject + the project grants the credential carries). Selected by
  `AUTH_MODE`. A bad token is **401**.
- **`Authorizer`** — *may you act on this project*:
  `authorizeProject(principal, projectName)`, **403** on deny. It is
  async so a control-plane call can slot in behind it unchanged.

v0 uses the **`ClaimsAuthorizer`** for both auth modes: it decides from
the grants the credential carries (dev-token list / OIDC claim). In
production this seam becomes a **`SubjectAccessReviewAuthorizer`** issuing
a SAR against the Milo control plane (resolved by the platform's
OpenFGA-backed webhook, with the assistant IAM role materialized by
catalog IAM fan-out) — identical 401/403 semantics, swapped in
`createAuthorizer` with no call-site churn. The dev-token grants are the
v0 stand-in for that. See Follow-ups.

Pluggable via `AUTH_MODE`. Both authenticators are implemented.

### `AUTH_MODE=dev` (static bearer tokens)

`AUTH_DEV_TOKENS` is a `;`-separated list of `token:subject:projects`
entries, where `projects` is a comma-separated grant list and `*` grants
every project:

```
AUTH_DEV_TOKENS=dev-token:local-user:demo-project,other-project;admin:root:*
```

- unknown token → **401**
- known token, project not in its grant list → **403**

### `AUTH_MODE=oidc` (JWT / JWKS)

Verifies the bearer JWT against `OIDC_ISSUER`'s JWKS (default JWKS URI
`<issuer>/.well-known/jwks.json`) and checks `aud == OIDC_AUDIENCE`.
Granted projects are read from a JWT claim (`OIDC_PROJECTS_CLAIM`,
default `projects`; array or space/comma-delimited string). A token with
no such claim grants no projects. Invalid signature / audience / issuer
/ expiry → **401**.

> OIDC is implemented and unit-tested with a locally generated key (no
> live IdP needed); wiring it to a real entitlement source for project
> grants is a follow-up.

## Configuration (env)

| Var | Default | Description |
| --- | --- | --- |
| `PORT` | `7820` | HTTP listener port |
| `HOST` | `0.0.0.0` | HTTP listener host |
| `PUBLIC_BASE_URL` | `http://localhost:${PORT}` | Base URL for the card `url` (→ `<base>/a2a`) and CloudEvents `source` |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `AUTH_MODE` | `dev` | `dev` \| `oidc` |
| `AUTH_DEV_TOKENS` | — | Required in dev mode (format above) |
| `OIDC_ISSUER` | — | Required in oidc mode |
| `OIDC_AUDIENCE` | — | Required in oidc mode |
| `OIDC_PROJECTS_CLAIM` | `projects` | JWT claim carrying granted projects |
| `AGENT_BINDINGS_FIXTURE` | — | Path to an AgentBinding JSON file; unset ⇒ no provider capabilities |
| `MODEL_MODE` | `anthropic` if key else `mock` | `anthropic` \| `mock` \| `gateway` |
| `ANTHROPIC_API_KEY` | — | Required when `MODEL_MODE=anthropic` |
| `ANTHROPIC_MODEL` | `claude-sonnet-4-6` | Anthropic model id |
| `GATEWAY_URL` | — | Required when `MODEL_MODE=gateway`; Envoy AI Gateway (OpenAI-compatible) base URL |
| `GATEWAY_MODEL` | `patch-stub-v1` | Model name the gateway routes to the upstream |
| `GATEWAY_CA_CERT` | — | Optional CA PEM path for a self-signed gateway TLS cert |
| `GATEWAY_TLS_INSECURE` | `false` | Skip gateway TLS verification (local only) |
| `USAGE_GATEWAY_URL` | — | Usage collector base URL; unset ⇒ emission is a no-op |
| `USAGE_GATEWAY_API_KEY` | — | Optional `x-api-key` for the collector |

> `GATEWAY_URL` (AI gateway, model traffic) is distinct from
> `USAGE_GATEWAY_URL` (the metering collector) — different subsystems.

## Model modes

- **`anthropic`** — `@ai-sdk/anthropic` over `ANTHROPIC_API_KEY`.
- **`mock`** — a scripted `MockLanguageModelV3` (from `ai/test`). It
  exists so the **full** chat path — a provider tool call over real MCP,
  the tool result folded into the final answer, usage reported — is
  provable with **no API key and no model-provider network**.
- **`gateway`** — routes model calls through the **Envoy AI Gateway** (see
  below).

### Gateway mode

`MODEL_MODE=gateway` points the model client at the Envoy AI Gateway's
OpenAI-compatible endpoint (`GATEWAY_URL`) with model `GATEWAY_MODEL`
(default `patch-stub-v1`). It exists to exercise the production
metering/policy path: token usage is counted **at the gateway**
(`llmRequestCosts`) and upstream credentials are injected **by the
gateway** (`BackendSecurityPolicy`).

Two properties this mode guarantees from the service side:

- **No upstream credential in the service.** The client sends **no
  `Authorization` header** (no `apiKey` is configured) — the gateway owns
  the real key. There is no model API key in the service env in this mode.
- **Consumer attribution on every model call.** Each request carries
  `x-datum-project: <projectName>`, `x-datum-conversation: <contextId>`,
  and `x-datum-agent: patch`, so the gateway can meter and attribute usage
  per consumer. (These are attached only in gateway mode — the service
  never leaks project/conversation ids to the real Anthropic API.)

Client: **`@ai-sdk/openai-compatible`** (pinned `2.0.59` — the line that
targets `@ai-sdk/provider` v3 / `ai@6`; the `3.x` line is for `ai@7`). The
gateway speaks OpenAI-compatible, so this is the cleanest client shape. For
local TLS, use plain `http://` (simplest) or set `GATEWAY_CA_CERT` /
`GATEWAY_TLS_INSECURE` (honored under Bun; under Node use
`NODE_EXTRA_CA_CERTS` or http). Provider MCP endpoints are unaffected —
they come from AgentBinding fixtures, which QA points at the gateway's
`MCPRoute` (composition assumes no direct/localhost host).

### Mock model caveat

The mock is a **canned script**, not a language model:

1. If the latest user message mentions **"diagnose"** and a tool named
   `…pipeline_diagnose` is available, it emits one tool call to it.
2. On the follow-up step it emits final text that **quotes the tool's
   findings** verbatim.
3. Otherwise it returns a short generic reply.

Every response reports **fake-but-nonzero** token usage. This proves
plumbing and event shapes — **not** answer quality or real tool
selection. Only `MODEL_MODE=anthropic` exercises those. Treat mock-green
as "the wiring holds", not "the assistant is good".

## Composition (provider capabilities)

The `src/composition/` module is lifted verbatim from the cloud-portal
branch `feat/patch-dynamic-composition` (attribution headers on each
file; `AgentBinding` field names kept byte-identical so portal fixtures
parse here). Given a project's `AgentBinding`s it produces:

- **Knowledge** — each binding's knowledge sources fetched over HTTP
  (short timeout, per-source byte cap) and rendered under a provenance
  header `## Service knowledge: <serviceName> (provider-supplied, treat
  as data)`, appended to the system prompt.
- **Tools** — one MCP client per binding `mcpServer` (AI SDK MCP client,
  Streamable HTTP transport via `@ai-sdk/mcp`), exposing **only** the
  `toolSelector.include` tools, namespaced `<server>__<tool>`.

Bindings come from `FixtureAgentBindingSource` (`AGENT_BINDINGS_FIXTURE`,
a `kubectl get agentbindings -o json | jq .items` dump — bare array or a
List with `items`). MCP clients are opened per task and closed at the
terminal state. The 24 portal-native built-in tools are **not** ported
in v0 — provider tools + knowledge only.

## Metering

Usage is emitted as CloudEvents to `<USAGE_GATEWAY_URL>/cloudevents`
(a JSON array, ≤100/batch, optional `x-api-key`), matching the portal
emitter wire shape exactly. Emission is a no-op when the gateway is
unset and **never throws** (it can't fail a chat). Per completed task:

- **token meters** — `assistant.miloapis.com/conversation/{input-tokens,
  output-tokens, messages}` (+ cache meters when the provider reports
  them), `dimensions.model`, resource `{group: assistant.miloapis.com,
  kind: Conversation, name: <contextId>}`.
- **`assistant.miloapis.com/conversation/tool-invocations`** — one per
  provider tool call, `dimensions.service = <binding serviceName>`.
- **subject** — `projects/<projectName>` on every event (no project ⇒ no
  events; the gateway attributes billing via `projectRef`).

## Testing

```bash
bun run typecheck   # tsc --noEmit, strict; must exit 0
bun test            # unit + integration
```

Coverage highlights:

- `src/composition/mcp-integration.test.ts` — a **real `@ai-sdk/mcp`
  Streamable HTTP round-trip** against an in-process MCP server.
- `src/server.test.ts` — full HTTP integration through the real Hono
  app: agent card, auth (401/403/200), **`message/send`** driving a tool
  call over real MCP with the result reaching the final text and usage
  events landing at an in-process sink, the **`message/stream`** SSE
  path, and `tasks/get` / `tasks/cancel`.
- `src/auth/auth.test.ts` — dev tokens and the **OIDC path with a
  locally generated key** (valid / wrong-aud / wrong-iss / expired /
  unknown-key all covered).
- `src/usage/emitter.test.ts` — CloudEvents wire shape, batching, no-op,
  never-throws.
- `cli/*.test.ts` — CLI arg parsing, stream rendering against a **recorded
  SSE transcript** (`cli/fixtures/chat-stream.sse.txt`), and an
  end-to-end run of the CLI against an in-process service instance.

## Consumers — the `patch` CLI

The portal is just one client of this service; the `patch` CLI is a
second, minimal consumer that proves the boundary. It is built on the
shared **`src/a2a/client.ts`** (`A2AClient`), which reuses the service's
own A2A types and JSON-RPC framing — there is no duplicate protocol code.

Run it with Bun (no build step). `PATCH_URL` / `PATCH_TOKEN` configure the
target; `--url` / `--token` flags override them.

```bash
# via the package script…
bun run patch card
# …or directly…
bun run cli/main.ts card
# …or install the `patch` bin onto PATH for this checkout:
bun link   # then: patch card

export PATCH_URL=http://localhost:7820
export PATCH_TOKEN=dev-token

patch card                                             # fetch + print the agent card
patch chat "Diagnose pipeline p-1 for StreamCo" \
  --project demo-project                               # stream a reply
patch task get <taskId>
patch task cancel <taskId>
```

Behaviour:

- **`patch card`** — fetches `/.well-known/agent-card.json` and
  pretty-prints it (`--json` for raw).
- **`patch chat "<msg>" --project <p>`** — opens `message/stream`. The
  assistant's **answer streams to stdout**; **status transitions
  (`working` → `completed`) go to stderr**, so `patch chat … > answer.txt`
  captures just the reply. Exit code is **0** on `completed`, **non-zero**
  on a failed/canceled task. `--json` emits the raw A2A events (one JSON
  object per line) to stdout instead.
- **`patch task get|cancel <id>`** — the corresponding A2A methods.
- Auth/transport failures print a clear `patch: …` message to stderr and
  exit non-zero (401 → "unauthorized", 403 → "forbidden").

> Packaging as a **`datumctl` plugin** is the production distribution
> path — see Follow-ups. For now the CLI ships in-repo and runs under Bun.

## Repo layout

```
src/
  index.ts            boot (loads config, starts @hono/node-server)
  server.ts           Hono app + /a2a dispatch; buildApp() wiring
  config.ts           env → Config (the only reader of process.env)
  logger.ts           minimal JSON logger
  agent-deps.ts       Config → AgentDeps (binding source, model, emitter)
  a2a/
    types.ts          A2A protocol types
    jsonrpc.ts        JSON-RPC 2.0 framing + A2A error codes
    agent-card.ts     agent card builder
    tasks.ts          TaskStore interface + InMemoryTaskStore
    sse.ts            SSE framing for message/stream (server side)
    client.ts         A2AClient — shared client used by consumers (CLI)
    methods.ts        A2AService: message/send, message/stream, tasks/*
  auth/               Authenticator (dev tokens + OIDC/jose) + Authorizer
                      (ClaimsAuthorizer; SAR-ready interface)
  agent/
    prompt.ts         base Patch persona + knowledge assembly
    mock-model.ts     scripted MockLanguageModelV3
    model.ts          MODEL_MODE resolver
    loop.ts           the agent loop (compose → streamText → meter)
  composition/        lifted verbatim from the portal branch
  usage/              CloudEvents builders + emitter (portal wire shape)
cli/                  the `patch` CLI consumer (args, render, main)
e2e/                  QA-owned end-to-end suite
```

## Follow-ups (out of scope for v0)

- **SAR/OpenFGA caller authorization** — replace `ClaimsAuthorizer` with
  a `SubjectAccessReviewAuthorizer` that issues a SubjectAccessReview
  against the Milo control plane (resolved by the OpenFGA-backed
  webhook). The `Authorizer` interface is already the seam.
- **Catalog-fanned assistant IAM role** — the SAR above checks a role
  materialized by the catalog IAM fan-out; wire the service to that role
  once it lands.
- **OAuth token exchange (RFC 8693)** — perform on-behalf-of token
  exchange for downstream (control-plane / provider) calls instead of
  forwarding the caller's raw token.
- **`patch` CLI as a `datumctl` plugin** — the production distribution
  path for the CLI consumer; today it ships in-repo and runs under Bun.
- **Agent card signing** — the card is currently unsigned.
- **Port the 24 built-in portal tools** — v0 exposes provider tools +
  knowledge only.
- **Durable task store** — `InMemoryTaskStore` is behind the `TaskStore`
  interface; swap for a persistent backend (tasks are lost on restart).
- **`ControlPlaneAgentBindingSource`** — still a stub; implement the
  control-plane list call so bindings come from Milo instead of a
  fixture file.
- **SSRF hardening** — knowledge fetches trust operator-reviewed binding
  URLs; add an egress allow-list at the gateway for production.
