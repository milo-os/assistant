# Conversation storage

Conversations outlive the process that served them. A customer can close a
terminal, come back the next day, list their threads, and resume one with its
context intact.

## What is stored

Four kinds of state, each scoped to a project and each with an in-memory and a
PostgreSQL implementation behind one interface.

| State | Purpose | Lifetime |
|---|---|---|
| Conversation history | The turns of a thread, for context and for reading back | Durable |
| Task state | The A2A lifecycle of a turn in flight | Durable |
| Project memory | Facts the assistant is asked to remember across threads | Durable |
| Capability gaps | Requests no entitled tool could satisfy | Durable |

The two implementations are held to the same behavior by a shared conformance
suite, so local development runs without a database while production gets
durability, and the pair cannot drift apart unnoticed.

## Scoping

Every read and write is scoped to a project. The store's interface is
tenant-safe by default: a list with no scope on the context returns an empty
page rather than everything. Where a query cannot be scoped, the surface is not
exposed — see the treatment of `ListTasks` in
[Identity and access](./identity-and-access.md).

## Compaction

A long conversation eventually exceeds what a model can read. Rather than
silently dropping the oldest turns, Patch summarizes them.

When the history crosses a threshold, the oldest turns are replaced by a single
summary turn that stands in for them. The thread keeps its shape: a reader sees
that a summary occurred and what it covers, rather than encountering a gap.
Customers can also compact on demand with a `/compact` command when they want to
reset context before changing subject.

Compaction fails open. If a summary cannot be produced, the turn proceeds with
the untruncated history — the behavior that existed before compaction — so a
summarization problem degrades context quality rather than breaking the chat.

## Reading conversations back

Storage is written by the assistant but read through a Kubernetes API. The
aggregated apiserver projects conversations and capability gap reports as
resources under `assistant.miloapis.com`, so listing and resuming threads uses
ordinary Kubernetes identity and RBAC instead of a bespoke endpoint.

That split matters for authorization: the read path is governed by the same
platform permissions as any other resource a customer owns, and Patch does not
need a second access model for it. See
[Assistant apiserver](../components/assistant-apiserver.md).

## Related documentation

- [Architecture overview](./README.md)
- [Conversation turn](./conversation-turn.md) — what produces a turn to store.
- [Assistant apiserver](../components/assistant-apiserver.md) — the read view.
- [Identity and access](./identity-and-access.md) — how project scope is
  decided.
