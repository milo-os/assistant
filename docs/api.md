# API & consumers

The assistant's wire API (A2A v1.0 over JSON-RPC) and the consumers that speak it. New to the service? Start with [development.md](development.md) to run it, then this for the protocol details.

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

