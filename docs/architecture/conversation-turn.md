# Conversation turn

A turn is one exchange in a conversation: a customer message, any tool calls it
triggers, and the answer. It is the path every other surface feeds into, and the
unit that Patch authorizes, remembers, and meters.

## Overview

![Assistant service C4 container diagram](../diagrams/container.png)

A turn crosses four trust boundaries. The consumer proves who it is to the
control plane, Patch proves the caller may act on the project, the model runs
behind the AI gateway rather than in the service, and provider tools run in the
provider's own service. Patch holds no credential for any of them.

## The path a question takes

1. **The consumer sends a message.** A JSON-RPC request arrives at `POST /a2a`
   with a bearer token and a project name.
2. **The control plane identifies the caller.** A TokenReview resolves the token
   to a subject; a SubjectAccessReview decides whether that subject may act on
   the project. Either can reject the turn before any work begins. See
   [Identity and access](./identity-and-access.md).
3. **Patch composes the project's capabilities.** It fetches the capability
   documents the project is entitled to, retrieves each provider's knowledge,
   and opens an MCP client per provider, exposing only the reviewed tools. See
   [Capabilities](./capabilities.md).
4. **Patch loads the conversation.** Prior turns come from durable storage. When
   the history outgrows the model's context, Patch replaces the oldest turns
   with a summary. See [Conversation storage](./conversation-storage.md).
5. **The model runs.** Patch sends the system prompt, the history, and the tool
   set to the model, then runs whatever tools the model asks for and feeds the
   results back. The loop repeats until the model answers. See
   [Model providers](./model-providers.md).
6. **Patch records what happened.** It appends the turn to the conversation and
   emits one usage event per model call and per provider tool invocation. See
   [Metering](./metering.md).

## Tools available in a turn

The model sees two kinds of tool, and cannot tell them apart from the names.

- **Provider tools**: the reviewed allow-list from each entitled service,
  namespaced by provider so two services can publish the same tool name.
- **Built-in tools**: capabilities Patch itself offers — loading a provider
  skill on demand, writing to project-scoped memory, and reporting a capability
  gap back to the provider that owns the missing surface.

A provider tool call travels through the AI gateway, which enforces the
allow-list a second time. A tool that is absent from the gateway's route is not
callable even if a capability document names it.

## Streaming and tasks

Patch implements the A2A task lifecycle, so a turn is addressable while it runs.

| Method | Purpose |
|---|---|
| `SendMessage` | Run a turn and return the answer |
| `SendStreamingMessage` | Run a turn, streaming events as they occur |
| `GetTask` | Read the state of a turn already submitted |
| `CancelTask` | Stop a turn in flight |

Those four methods are the entire authorized surface. Every other method the A2A
library dispatches is rejected, because none of them can be scoped to a project
safely today. See [Identity and access](./identity-and-access.md).

## Failure posture

A turn degrades rather than fails wherever the failure is not the customer's.

- **A provider is unreachable**: its knowledge and tools are omitted and the
  turn proceeds with what remains. A provider outage never fails a chat.
- **A capability document is malformed**: that document is skipped with a
  warning; valid documents from other providers still apply.
- **The model is unavailable**: the turn retries with backoff, then fails
  visibly. Patch does not invent an answer.
- **Summarization fails**: the turn falls back to sending the untruncated
  history, which is the behavior that existed before compaction.

Identity and access are the deliberate exception: they fail closed, and an
undecidable answer rejects the turn.

## Related documentation

- [Architecture overview](./README.md)
- [Capabilities](./capabilities.md) — what a provider contributes to a turn.
- [Identity and access](./identity-and-access.md) — who may run one.
- [Conversation storage](./conversation-storage.md) — what a turn remembers.
- [Metering](./metering.md) — what a turn reports.
- [API and consumers](../api.md) — the wire format and JSON-RPC methods.
