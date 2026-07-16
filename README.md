# Patch — Assistant Service

**Patch** is Datum Cloud's AI operator: a conversational agent that
answers questions and diagnoses problems using the live capabilities of
the services a customer is entitled to. This repository is Patch's
runtime — a standalone Go backend (catalog service
`assistant.miloapis.com`) that any consumer reaches over an open
protocol: the Cloud Portal, the bundled `patch` CLI, or any other agent.

What it does:

- Speaks the **A2A (Agent2Agent) protocol v1.0** over JSON-RPC 2.0 on
  the official [`a2a-go`](https://github.com/a2aproject/a2a-go) library
  — consumers integrate against a standard, not a Datum API.
- Composes each project's entitled provider capabilities — knowledge
  and MCP tools — per conversation from **capability documents**, a
  schema this service owns and publishes. Providers plug in without the
  assistant redeploying.
- Holds **durable, project-scoped conversation memory**: follow-up
  messages in the same context are answered with the prior exchange in
  the prompt, backed by Postgres.
- Meters every model call and provider tool invocation as CloudEvents
  on the Milo billing wire, aggregated across all agentic steps with
  full cache-token detail — designed to be gateway-verifiable.

## Quickstart

```bash
go build ./cmd/assistant ./cmd/patch     # → ./assistant and ./patch
cp .env.example .env                      # the defaults run a mock-model dev server
env $(grep -v '^#' .env | xargs) ./assistant     # → http://localhost:7820
```

Probe it:

```bash
curl -s localhost:7820/healthz
curl -s localhost:7820/.well-known/agent-card.json | jq .

# SendMessage (dev token from .env.example, mock model). A2A v1.0 shape:
# PascalCase method, role enum ROLE_USER, content parts {text} (no kind).
curl -s localhost:7820/a2a \
  -H 'authorization: Bearer dev-token' \
  -H 'content-type: application/json' \
  -d '{
    "jsonrpc":"2.0","id":1,"method":"SendMessage",
    "params":{"message":{
      "role":"ROLE_USER","messageId":"m1",
      "parts":[{"text":"Diagnose pipeline p-1 for StreamCo"}],
      "metadata":{"projectName":"demo-project"}
    }}
  }' | jq '.result.task.status.state'   # → "TASK_STATE_COMPLETED"
```

### Runtime

Written in **Go** (module `github.com/milo-os/assistant`, see `go.mod`
for the toolchain version). `go build ./cmd/assistant` produces the
service binary; `go build ./cmd/patch` produces the `patch` CLI consumer.
Standard library `net/http` for the mux. Tests and vet use the Go
toolchain (`go vet ./...`, `go test ./...`). The wire-level acceptance
harness under `e2e/` runs on bun/Node.

## API surface

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| `GET` | `/healthz` | none | `{"status":"ok"}` |
| `GET` | `/.well-known/agent-card.json` | none | A2A v1.0 agent card (also at `/.well-known/agent.json`) |
| `POST` | `/a2a` | bearer | JSON-RPC 2.0 endpoint |

### JSON-RPC methods (`POST /a2a`)

A2A v1.0 method names are **PascalCase** (a2a-go binding):

- **`SendMessage`** — create a task, run the agent loop to completion,
  return the final result (a `{task}` with `artifacts` and `history`).
- **`SendStreamingMessage`** — same, but stream the run as **Server-Sent
  Events**. Each SSE frame is a JSON-RPC response whose `result` is one
  `StreamResponse` (a oneOf: `task` / `statusUpdate` / `artifactUpdate` /
  `message`). The stream **closes on the terminal status** — there is no
  `final` flag. Sequence: the initial `task` → `statusUpdate`
  `TASK_STATE_WORKING` → `artifactUpdate`(s) carrying response text →
  `statusUpdate` `TASK_STATE_COMPLETED`.
- **`GetTask`** — `{ id, historyLength? }` → the stored task.
- **`CancelTask`** — `{ id }` → cancels a non-terminal task; a terminal
  task returns error `-32002` (TaskNotCancelable); an unknown id returns
  `-32001` (TaskNotFound).

### A2A v1.0 wire

Client-facing reference for the v1.0 wire shape (clients written against
pre-1.0 A2A drafts should note these — the older forms are rejected):

- **Method names are PascalCase**: `SendMessage` / `SendStreamingMessage`
  / `GetTask` / `CancelTask` (draft-style `message/send`, `message/stream`,
  `tasks/get`, `tasks/cancel` return `-32601` MethodNotFound).
- **Task states are `TASK_STATE_*` enums**: `TASK_STATE_SUBMITTED`,
  `TASK_STATE_WORKING`, `TASK_STATE_COMPLETED`, `TASK_STATE_FAILED`,
  `TASK_STATE_CANCELED`.
- **Stream events are a `StreamResponse` oneOf** keyed
  `task` / `statusUpdate` / `artifactUpdate` / `message`. There is **no
  `kind` discriminator and no `final` flag** — the stream closes on the
  terminal state.
- **Messages** use `role` as the enum `ROLE_USER` / `ROLE_AGENT`, and
  content parts are `{ "text": "…" }` (no `kind`). The message carries no
  top-level `kind`.
- **Agent card** uses `supportedInterfaces[]` — the transport, protocol
  version, and url live there (`{ url: <base>/a2a, protocolBinding:
  "JSONRPC", protocolVersion: "1.0" }`), not as top-level fields. The
  bearer scheme is nested under
  `securitySchemes.bearer.httpAuthSecurityScheme.scheme`.

Deviations (intentional, documented):

- **`message.metadata.projectName`** is an **extension field** — A2A has
  no notion of a Milo project. It is required; a request without it is
  rejected with `-32602` (Invalid params).
- **AuthN failures are HTTP `401`/`403`**, not JSON-RPC error objects
  (missing/unknown token → 401; a project the token doesn't grant →
  403). Protocol/validation errors use JSON-RPC error objects with HTTP
  200.
- **`SendStreamingMessage`** does not surface intermediate tool-call
  events as SSE frames — the metering pipeline and the provider MCP
  server log are the authoritative record of a tool invocation.
- The agent card is **unsigned** (no `signatures`). See Follow-ups.
- Push notifications and `SubscribeToTask` (resubscribe) are unimplemented
  and rejected cleanly.

### Scoping

- **`contextId`** = the conversation id, and also the name of the
  `Conversation` metering resource. Supply `message.contextId` to
  continue a conversation; omitted ⇒ the server mints one.
- **Project** comes from `message.metadata.projectName`. The task runs
  against that project and all usage is subject-scoped to
  `projects/<projectName>`.

## Auth (authN + authZ are separate seams)

Two independent interfaces (`internal/auth/`):

- **Authenticator** — *who are you*: bearer token → principal (subject +
  the project grants the credential carries). Selected by `AUTH_MODE`. A
  bad token is **401**.
- **Authorizer** — *may you act on this project*, **403** on deny. It is
  fail-closed and async so a control-plane call can slot in behind it
  unchanged.

v0 uses a claims authorizer for both auth modes: it decides from the
grants the credential carries (dev-token list / OIDC claim). In
production this seam becomes a **SubjectAccessReview authorizer** issuing
a SAR against the Milo control plane (resolved by the platform's
OpenFGA-backed webhook) — identical 401/403 semantics, swapped with no
call-site churn. The dev-token grants are the v0 stand-in.

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
no such claim grants no projects. Invalid signature / audience / issuer /
expiry → **401**. Unit-tested with a locally generated key (no live IdP
needed).

## Configuration (env)

| Var | Default | Description |
| --- | --- | --- |
| `PORT` | `7820` | HTTP listener port |
| `HOST` | `0.0.0.0` | HTTP listener host |
| `PUBLIC_BASE_URL` | `http://localhost:${PORT}` | Base URL for the card interface `url` (→ `<base>/a2a`) and CloudEvents `source` |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `AUTH_MODE` | `dev` | `dev` \| `oidc` |
| `AUTH_DEV_TOKENS` | — | Required in dev mode (format above) |
| `OIDC_ISSUER` | — | Required in oidc mode |
| `OIDC_AUDIENCE` | — | Required in oidc mode |
| `OIDC_PROJECTS_CLAIM` | `projects` | JWT claim carrying granted projects |
| `CAPABILITY_DOCS_FIXTURE` | — | Path to a capability-documents JSON file (fixture source); mutually exclusive with `CAPABILITY_PROVIDER_URL` |
| `CAPABILITY_PROVIDER_URL` | — | Base URL of the capability-provider HTTP API (HTTP source); mutually exclusive with `CAPABILITY_DOCS_FIXTURE`. Both unset ⇒ no provider capabilities |
| `CONVERSATION_STORE_URL` | — | `postgres://` URL for durable conversation history. Unset ⇒ in-memory (process lifetime). Set but unreachable ⇒ boot fails (no silent fallback to amnesia) |
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

The model/loop layer is the in-repo **`agentcore`** package — a
provider-neutral library (unified stream parts, a tool loop with
per-step usage aggregation, and adapters). Modes:

- **`anthropic`** — `agentcore/anthropic` over the official
  `anthropic-sdk-go`, keyed by `ANTHROPIC_API_KEY`. Full usage fidelity
  including cache read/write tokens.
- **`mock`** — `agentcore/mockmodel`, a scripted in-process model. It
  exists so the **full** chat path — a provider tool call over real MCP,
  the tool result folded into the final answer, usage reported — is
  provable with **no API key and no model-provider network**.
- **`gateway`** — `agentcore/openaicompat` over the official `openai-go`,
  routed through the **Envoy AI Gateway** (see below).

### Gateway mode

`MODEL_MODE=gateway` points the model client at the Envoy AI Gateway's
OpenAI-compatible endpoint (`GATEWAY_URL`) with model `GATEWAY_MODEL`. It
exercises the production metering/policy path: token usage is counted **at
the gateway** (`llmRequestCosts`) and upstream credentials are injected
**by the gateway** (`BackendSecurityPolicy`).

Two properties this mode guarantees from the service side:

- **No upstream credential in the service.** The client sends **no
  `Authorization` header** — the gateway owns the real key. There is no
  model API key in the service env in this mode.
- **Consumer attribution on every model call.** Each request carries
  `x-datum-project: <projectName>`, `x-datum-conversation: <contextId>`,
  and `x-datum-agent: patch`, so the gateway can meter and attribute usage
  per consumer. (These are attached only in gateway mode — the service
  never leaks project/conversation ids to the real Anthropic API.)

For local TLS, use plain `http://` or set `GATEWAY_CA_CERT` /
`GATEWAY_TLS_INSECURE`.

### Mock model caveat

The mock is a **canned script**, not a language model: if the latest
user message mentions **"diagnose"** and a `…pipeline_diagnose` tool is
available it emits one tool call, then quotes the tool's findings in the
final text (a two-step run); otherwise it returns a short generic reply.
Every response reports **fake-but-nonzero** token usage. This proves
plumbing and event shapes — **not** answer quality or real tool
selection. Treat mock-green as "the wiring holds", not "the assistant is
good".

## Capability documents (provider capabilities)

A **capability document** describes one provider service's contribution
to the assistant: its MCP endpoint(s), the reviewed tool allow-list, and
its knowledge sources. The document schema is **owned by this service**
(`internal/capability`) — there is no Milo/catalog client code in the
data path. Given a project's documents, composition produces:

- **Knowledge** — each document's knowledge sources fetched over HTTP
  (short timeout, per-source byte cap) and rendered under a provenance
  header, appended to the system prompt.
- **Tools** — one MCP client per `tools.mcpServers[]` entry (official
  `modelcontextprotocol/go-sdk`, Streamable HTTP transport), exposing
  **only** the `toolSelector.include` tools, namespaced `<server>__<tool>`
  (sanitized `[a-zA-Z0-9_-]`, first-wins on collision). The allow-list is
  enforced client-side too. MCP clients are opened per task, given a 5s
  connect timeout, and closed at the terminal state.

Document shape (JSON; the KRM-style envelope — `apiVersion`/`kind`/
`metadata`/`spec` — carries provenance from producers like the catalog's
`AgentBinding` projection; the parser ignores unknown fields and rejects
invalid documents with clear errors):

```jsonc
{
  "kind": "AgentBinding",
  "metadata": { "name": "streamco-binding", "namespace": "demo-project" },
  "spec": {
    "serviceRef":  { "name": "streamco" },
    "serviceName": "streaming.streamco.example",   // used as the tool-invocation meter dimension
    "knowledge": {
      "sources":  [{ "type": "LLMDocs", "url": "https://…/llms-full.txt" }],
      "concepts": [{ "gvk": { "group": "…", "kind": "Stream" }, "summary": "…" }]
    },
    "tools": {
      "mcpServers": [{
        "name": "streamco",
        "endpoint": "http://provider/mcp",
        "toolSelector": { "include": ["streams_list", "pipeline_diagnose"] }
      }]
    }
  }
}
```

### Capability provider API (published contract, v1)

Capability documents reach the assistant through the `Source` seam
(`internal/capability`) — the stable interface between the assistant and
wherever documents come from:

```go
// Source yields the capability documents entitled to a project.
type Source interface {
    Documents(ctx context.Context, projectName string) ([]CapabilityDocument, error)
}
```

Two implementations ship, selected by env and **mutually exclusive** (the
config loader rejects setting both):

- **Fixture source** (`CAPABILITY_DOCS_FIXTURE`) — a local JSON file (bare
  array or a `{"items": […]}` List). Good for local dev and e2e.
- **HTTP source** (`CAPABILITY_PROVIDER_URL`) — the **capability-provider
  API** below. Documents are fetched per conversation (no cache in v0).

The schema below is the wire contract for **both** the fixture file and
the HTTP response body. **The assistant owns this schema** — a capability
provider (the control-plane adapter) serves documents in this shape; if
the shape changes, it changes here first.

#### Endpoint

```
GET {CAPABILITY_PROVIDER_URL}/projects/{projectName}/capability-documents
Accept: application/json
```

- `{projectName}` is path-escaped; it is the caller's authenticated
  project. The provider returns exactly the documents that project is
  entitled to (server-side scoping — the assistant does not filter).
- **200** with a JSON body (see schema) is the only success. The body is
  either a bare array of documents or a `{"items": […]}` List.
- **Degradation contract:** any transport error, a non-2xx status, an
  unreadable body, or a malformed root is logged and treated as **no
  capabilities** (empty set, chat proceeds with built-ins only) — a
  provider outage never fails a chat. Individual documents that fail
  validation are **skipped with a warning**; the valid ones still apply.
  Fetches use a **5s timeout**.

#### Capability document schema (v1)

Derived from the Go types in `internal/capability/document.go`. Unknown
fields are ignored (forward-compatible); required fields are marked.
`configurationVersion` is the provider's own config revision, distinct
from this **document schema version (v1)**.

```jsonc
{
  "apiVersion": "services.miloapis.com/v1alpha1", // optional, provenance
  "kind": "AgentBinding",                          // optional, provenance
  "metadata": {                                    // optional
    "name":      "string",
    "namespace": "string"
  },
  "spec": {                                        // REQUIRED
    "serviceRef":           { "name": "string" },  // REQUIRED, name REQUIRED
    "serviceName":          "string",              // REQUIRED (tool-invocation meter dimension)
    "serviceAgentRef":      { "name": "string" },  // REQUIRED, name REQUIRED
    "configurationVersion": "string",              // REQUIRED (provider config revision)

    "knowledge": {                                 // optional
      "sources": [{
        "type":  "LLMDocs | Runbook | Markdown",   // REQUIRED, must be one of the enum
        "title": "string",                         // optional
        "url":   "string"                          // REQUIRED
      }],
      "concepts": [{
        "gvk":     { "group": "string", "kind": "string" },
        "summary": "string"
      }]
    },

    "tools": {                                     // optional
      "mcpServers": [{
        "name":         "string",                  // REQUIRED
        "endpoint":     "string",                  // REQUIRED (Streamable HTTP MCP URL)
        "toolSelector": { "include": ["string"] }, // client-side allow-list
        "mutating":     ["string"]                 // optional; tools flagged mutating
      }]
    },

    "authority": {                                 // optional
      "reads": [{ "gvk": { "group": "string", "kind": "string" } }],
      "maxTaskDurationSeconds": 0                   // optional int
    }
  },

  "status": {                                      // optional, ignored by the assistant
    "conditions": [{ "type": "string", "status": "string", "reason": "string", "message": "string" }]
  }
}
```

#### Example response (the StreamCo fixture)

```json
{
  "items": [
    {
      "apiVersion": "services.miloapis.com/v1alpha1",
      "kind": "AgentBinding",
      "metadata": { "name": "streamco-binding", "namespace": "demo-project" },
      "spec": {
        "serviceRef": { "name": "streamco" },
        "serviceName": "streaming.streamco.example",
        "serviceAgentRef": { "name": "streamco-agent" },
        "configurationVersion": "v1",
        "knowledge": {
          "sources": [
            { "type": "LLMDocs", "title": "Overview", "url": "http://127.0.0.1:7810/llms-full.txt" }
          ],
          "concepts": [
            { "gvk": { "group": "streaming.streamco.example", "kind": "Stream" }, "summary": "A live stream" }
          ]
        },
        "tools": {
          "mcpServers": [
            {
              "name": "streamco",
              "endpoint": "http://127.0.0.1:7810/mcp",
              "toolSelector": { "include": ["streams_list", "streams_get", "pipeline_diagnose"] },
              "mutating": []
            }
          ]
        },
        "authority": {
          "reads": [{ "gvk": { "group": "streaming.streamco.example", "kind": "*" } }],
          "maxTaskDurationSeconds": 60
        }
      },
      "status": { "conditions": [{ "type": "Ready", "status": "True" }] }
    }
  ]
}
```

The catalog-side **capability-provider adapter** projects `AgentBinding`
resources into this shape (rewriting MCP `endpoint`s to the gateway
MCPRoute URL). Because the assistant owns the schema, the adapter is
written against **this** contract, not the other way around.

## Conversation memory (multi-turn chat)

A2A groups related turns under a **`contextId`**: send a follow-up
message carrying the `contextId` from an earlier response and the service
treats it as the same conversation — the prior turns are **replayed into
the model prompt**, so "what about the second one?" works. A message
without a `contextId` starts a fresh conversation (the service assigns
one, visible on every streamed event).

Semantics:

- **What is remembered**: the user's text and the assistant's final
  answer per completed turn. Tool transcripts are not replayed; failed
  or canceled turns are not recorded.
- **Scope**: history is keyed by **(project, contextId)** — the project
  is the authorization boundary, so a guessed `contextId` from another
  project inherits nothing.
- **Truncation**: replayed history is capped by an estimated token
  budget (default 6000; oldest turns drop first, whole turns at a time),
  bounding the input-token cost of long conversations. Replayed tokens
  are real input tokens and are metered as such.
- **Durability**: set `CONVERSATION_STORE_URL` (a `postgres://` URL) and
  history is stored durably — conversations and messages live in two
  project-scoped Postgres tables and survive service restarts. Every
  query is keyed by `(project_name, context_id)`; the replay query is
  bounded (newest ~200 turns) regardless of conversation length; a
  turn's two message rows are written transactionally so concurrent
  appends never interleave. Unset, history is in-memory (process
  lifetime). Both stores implement the same `history.Store` seam, plus
  a `Lister` (per-project conversation listing, newest activity first)
  for a future conversation API.

## Metering

Usage is emitted as CloudEvents to `<USAGE_GATEWAY_URL>/cloudevents` (a
JSON array, ≤100/batch, optional `x-api-key`). The wire format is a
**pinned contract** with the platform's billing pipeline — a golden test
and the e2e sink byte-diff hold it stable; do not "improve" it. Emission
is a no-op when the gateway is unset and **never throws** (it can't fail
a chat). Per completed task:

- **token meters** — `assistant.miloapis.com/conversation/{input-tokens,
  output-tokens, messages}` (+ cache meters when the provider reports
  them), `dimensions.model`, resource `{group: assistant.miloapis.com,
  kind: Conversation, name: <contextId>}`. Values are int64 strings.
  Multi-step usage is aggregated into the total — every agentic step is
  billed, not just the final one (pinned by tests; under-billing here is
  a known failure class).
- **`assistant.miloapis.com/conversation/tool-invocations`** — one per
  provider tool call, `dimensions.service = <serviceName>`.
- **subject** — `projects/<projectName>` on every event.

## Consumers — the `patch` CLI

The portal is just one client of this service; the `patch` CLI is a
second, minimal consumer that proves the boundary. It is built on the
official `a2a-go` client (`a2aclient`) — no duplicate protocol code.

```bash
go build ./cmd/patch          # → ./patch
export PATCH_URL=http://localhost:7820
export PATCH_TOKEN=dev-token

./patch card                                          # fetch + print the agent card
./patch chat "Diagnose pipeline p-1 for StreamCo" \
  --project demo-project                              # stream a reply
./patch chat -i --project demo-project                # interactive multi-turn session
./patch chat "and the second finding?" \
  --project demo-project --context-id <c>             # continue a conversation
./patch task get <taskId>
./patch task cancel <taskId>
```

Behaviour:

- **`patch card`** — fetches the agent card and pretty-prints it (`--json`
  for raw).
- **`patch chat "<msg>" --project <p>`** — opens `SendStreamingMessage`.
  The **answer streams to stdout**; **status transitions go to stderr**,
  so `patch chat … > answer.txt` captures just the reply. Exit code is
  **0** on completed, **1** on a failed/canceled task or transport error,
  **2** on a usage error. `--json` emits the raw A2A events (one JSON
  object per line) to stdout instead.
  A one-shot chat prints `context: <id>` to stderr — pass it back with
  `--context-id` to continue that conversation with memory.
- **`patch chat -i --project <p>`** — interactive session (REPL): each
  line is a turn, the conversation id is threaded automatically, so the
  whole session shares memory. `Ctrl-D` or `/quit` to leave. An optional
  positional message is sent as the first turn.
- **`patch task get|cancel <id>`** — the corresponding A2A methods.
- Auth/transport failures print a clear `patch: …` message to stderr and
  exit non-zero (401 → "unauthorized", 403 → "forbidden").

## Repo layout

```
cmd/
  assistant/          service binary: config → runner → server
  patch/              the `patch` CLI consumer (on a2aclient)
agentcore/            extractable model/loop library (no env, no internal/ imports)
  model.go, usage.go, loop.go   unified stream parts, usage, tool loop
  anthropic/          adapter on anthropic-sdk-go
  openaicompat/       adapter on openai-go (gateway mode)
  mockmodel/          scripted in-process model
  mcptool/            MCP go-sdk client → agentcore ToolSet adapter
internal/
  capability/         capability documents: schema, fixture source, compose
  agent/              run-conversation orchestration (prompt, loop, usage events)
  a2a/                a2asrv AgentExecutor glue, task store wiring
  auth/               dev tokens + OIDC, fail-closed authorizer seam
  usage/              CloudEvents emitter (golden-pinned billing wire)
  config/             env → Config
  server/             net/http mux: /healthz, agent-card, /a2a
  logger/             slog setup
config/               kustomize dev environment (house pattern)
  base/               assistant Deployment/Service + namespace
  components/         gateway (Envoy AI Gateway resources), llm-stub,
                      streamco, sink
  overlays/dev        fixture-capability mode (default), images :dev
  overlays/dev-catalog  catalog capability-provider mode
  dependencies/       cnpg (vendored operator + Cluster), ai-gateway
                      (EG HelmRelease extension patch)
test/e2e/             chainsaw environment tests (env-health, chat-smoke)
fixtures/             capability-documents.json (sample)
e2e/                  wire-level A2A acceptance harness (bun/Node)
deploy/               Dockerfiles for the dev images
Taskfile.yaml         dev-environment entrypoints (see below)
```

## Dev environment (kind via datum-cloud/test-infra)

The full environment — assistant behind the Envoy AI Gateway (metered,
keyless, tool allow-lists), StreamCo MCP provider, usage sink, and a
CloudNativePG-backed durable conversation store — runs on a kind
cluster bootstrapped by [datum-cloud/test-infra](https://github.com/datum-cloud/test-infra),
consumed as a pinned remote Taskfile include (the datum-cloud house
pattern; requires `TASK_X_REMOTE_TASKFILES=1`, see `.env.example`).

```bash
cp .env.example .env
task dev:setup            # kind cluster + operators + images + full stack
task dev:forward          # assistant → localhost:1986, gateway → localhost:1987

PATCH_URL=http://localhost:1986 PATCH_TOKEN=pg-demo-token \
  go run ./cmd/patch chat -i --project demo-project

task dev:redeploy         # fast loop: rebuild + roll the assistant
task e2e                  # chainsaw environment tests
task dev:clean            # remove the environment (shared operators stay)
```

`task dev:deploy OVERLAY=dev-catalog` switches capabilities from the
fixture file to the service catalog's capability-provider API (requires
the catalog side from milo-os/service-catalog). The managed Envoy
Service name is pinned (`patch-ai-gateway`), so every URL in the config
tree is static — no discovery or templating anywhere.

## Testing

```bash
task test        # go vet + unit tests (TEST_DATABASE_URL adds Postgres store tests)
task e2e         # chainsaw against the dev environment
task e2e:local   # wire-level A2A harness against locally-booted binaries
```

Highlights: an in-process MCP round-trip in `internal/capability` /
`agentcore/mcptool`; full httptest integration through the real mux
(agent card, auth 401/403/200, `SendMessage`/`SendStreamingMessage`
driving a tool call over real MCP with usage landing at an in-process
sink, `GetTask`/`CancelTask`); OIDC with a locally generated key; the
usage emitter golden test pinning the billing wire; and the agent
loop's exit/usage-aggregation rules pinned against the mock model.

The **e2e acceptance harness** (`e2e/`, bun) drives the built binaries
end to end (core + consumers + gateway legs) and byte-diffs the sink
wire against the recorded golden — see `e2e/E2E-REPORT.md`.

## Roadmap

- **Portal A2A v1.0 client** — bring the cloud-portal's thin client up
  to the v1.0 wire (method names + SSE parsing) so the portal consumes
  this service like any other A2A client.
- **SAR/OpenFGA caller authorization** — replace the claims authorizer
  with a SubjectAccessReview authorizer against the Milo control plane.
- **OAuth token exchange (RFC 8693)** — on-behalf-of exchange for
  downstream calls instead of forwarding the caller's raw token.
- **`patch` CLI as a `datumctl` plugin** — the production distribution
  path for the CLI consumer.
- **Agent card signing** — the card is currently unsigned.
- **Durable task store** — the in-memory task store is behind an
  interface; swap for a persistent backend (tasks are lost on restart).
- **Conversation API surface** — the Postgres storage layer for
  conversations/messages is in (`CONVERSATION_STORE_URL`, see
  "Conversation memory"), including per-project listing. What remains
  is the consumer-facing API on top: Conversation as a KRM resource
  (aggregated apiserver) with messages behind a subresource, so portal
  and CLI can list and reopen conversations with platform authz.
- **SSRF hardening** — knowledge fetches trust operator-reviewed document
  URLs; add an egress allow-list at the gateway for production.
```
