# Conversations & metering

Durable multi-turn memory scoped per project, and the usage CloudEvents the billing pipeline consumes.

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

