# Assistant

The assistant is the service that runs conversations. It serves the A2A API,
composes each project's capabilities, drives the model loop, and writes to the
conversation store.

## Responsibilities

- **Consumer API**: serves A2A v1.0 over JSON-RPC at `POST /a2a`, including
  streaming and the task lifecycle.
- **Request gating**: authenticates and authorizes every turn against the
  control plane before any work begins.
- **Capability composition**: builds the tool set and prompt for the caller's
  project, per turn.
- **Agent loop**: calls the model, runs tools, and repeats until the model
  answers.
- **Persistence**: appends turns, tasks, memory, and gap reports to the store.
- **Reporting**: emits usage events for billing and telemetry for operations.

## Internal structure

The service is assembled at one composition root, which is the clearest single
read of how the parts fit.

### Transport

The HTTP layer owns routing, middleware, and the A2A task lifecycle. It runs
against an agent-runner interface rather than the agent itself, so the transport
and the runtime are independently testable and the server can be reviewed
without the agent present.

### Request gating

Middleware in front of the A2A handler resolves the caller and checks the
project. It inspects the JSON-RPC method before dispatch and permits only the
four project-scoped methods. See
[Identity and access](../architecture/identity-and-access.md).

### Capability composition

For each turn, this layer reads the project's capability documents, fetches
knowledge, and opens one MCP client per provider limited to the reviewed tools.
Clients close when the turn reaches a terminal state. See
[Capabilities](../architecture/capabilities.md).

### Agent runtime

The runtime holds the conversation: it loads history, compacts it when it grows
too large, assembles the prompt, and runs the model loop. It also provides the
built-in tools — loading a skill, project memory, and gap reporting.

### Model access

A single interface separates the loop from the vendor, with implementations for
the AI gateway, Anthropic directly, and a mock used in tests. See
[Model providers](../architecture/model-providers.md).

### Stores

History, tasks, memory, and gap reports each sit behind an interface with an
in-memory and a PostgreSQL implementation. See
[Conversation storage](../architecture/conversation-storage.md).

## Endpoints

| Path | Purpose |
|---|---|
| `POST /a2a` | The A2A JSON-RPC surface |
| `POST /v1/compact` | Manual conversation compaction |
| `POST /v1/conversations/rename` | Give a conversation a user-chosen name |
| `GET /.well-known/agent-card.json` | Capability discovery for A2A clients |
| `GET /healthz` | Liveness |
| `GET /readyz` | Readiness, including dependency reachability |
| `GET /metrics` | Prometheus metrics |

## Identity

The pod runs with its own service account holding `system:auth-delegator`, which
permits creating TokenReviews and SubjectAccessReviews and nothing else. It
holds no model credential — the gateway does — and no read access to project
resources.

## Related documentation

- [Architecture overview](../architecture/README.md)
- [Conversation turn](../architecture/conversation-turn.md) — the path through
  these parts.
- [Assistant apiserver](./assistant-apiserver.md) — the read view over the
  store.
- [Configuration](../configuration.md) — environment variables and modes.
- [API and consumers](../api.md) — the wire format.
