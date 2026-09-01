# Assistant service architecture

Patch is the Datum Cloud assistant. A customer asks about their project in plain
language — "why is my pipeline lagging?" — and Patch answers using the live
state of their services, running the diagnostic tools those services published
and explaining what it finds.

Patch is itself a service in the Milo catalog (`assistant.miloapis.com`), and it
runs standalone. Consumers reach it over open protocols, so the Cloud Portal,
the bundled CLI, and any third-party agent are all the same kind of client.

## How it works

A provider that wants to be part of Patch publishes its agent capabilities
alongside its catalog registration: what the service is, which of its tools an
assistant may call, and which reviewed procedures it may follow. Entitlement
decides who receives them. When a customer's project is entitled to a service,
that service's capabilities appear in that project's Patch, and revoking the
entitlement removes them from the next conversation turn.

Every turn is therefore composed per project. Patch assembles the knowledge and
tools of exactly the services the caller's project is entitled to, remembers the
conversation durably, calls a model, runs the tools the model asks for, and
meters what it used.

- **Capability document**: the contract a provider fills in to contribute
  knowledge, tools, and skills to a project's assistant.
- **Conversation**: a durable, project-scoped thread a customer can leave and
  resume.
- **Turn**: one exchange within a conversation — a customer message, any tool
  calls it triggers, and the answer.

## System context

![Assistant service C4 context diagram](../diagrams/context.png)

Patch sits between the people asking questions and the services that can answer
them. It holds no provider credentials and no customer data beyond the
conversation itself: provider tools run behind the AI gateway, and identity and
access are decided by the Milo control plane.

## Protocols

Nothing in the consumer or provider interface is a proprietary Datum API.

| Protocol | Role | Direction |
|---|---|---|
| [A2A][a2a] v1.0 | How consumers chat with Patch | Inbound |
| [MCP][mcp] | How Patch calls provider tools | Outbound |
| Capability documents | How the catalog tells Patch what a project is entitled to | Inbound |
| [CloudEvents][cloudevents] | How Patch reports what a turn consumed | Outbound |

## Surface areas

Each document below covers one surface end to end.

- [Conversation turn](./conversation-turn.md) — the path a question takes, from
  request to answer.
- [Capabilities](./capabilities.md) — how a provider's knowledge, tools, and
  skills reach a project's assistant.
- [Identity and access](./identity-and-access.md) — who is calling, and whether
  they may act on a project.
- [Conversation storage](./conversation-storage.md) — durable memory, history
  compaction, and the read view.
- [Metering](./metering.md) — how model and tool usage reaches billing.
- [Model providers](./model-providers.md) — how Patch stays independent of any
  one model vendor.
- [Observability](./observability.md) — what Patch reports about itself.

## Components

- [Assistant](../components/assistant.md) — the service that runs conversations.
- [Assistant apiserver](../components/assistant-apiserver.md) — conversations
  and gap reports as Kubernetes resources.

## Technology

| Component | Technology | Purpose |
|---|---|---|
| Runtime | Go | The service and the `patch` CLI |
| Consumer API | [A2A][a2a] v1.0 over JSON-RPC | Chat, streaming, task lifecycle |
| Provider tools | [MCP][mcp] Streamable HTTP | Calling service-owned tools |
| Model access | [Envoy AI Gateway][aigw] | Credential isolation and token metering |
| Storage | PostgreSQL ([CloudNativePG][cnpg]) | Conversations, tasks, memory, gap reports |
| Read API | Kubernetes aggregated apiserver | Listing and resuming conversations under platform authorization |
| Telemetry | OpenTelemetry, Prometheus | Traces and metrics |

## Related documentation

- [API and consumers](../api.md) — the wire, the JSON-RPC methods, and the CLI.
- [Configuration](../configuration.md) — environment variables and model modes.
- [Development](../development.md) — running the service locally.
- [Deployment](../deployment.md) — dev and production posture.

[a2a]: https://a2a-protocol.org
[mcp]: https://modelcontextprotocol.io
[cloudevents]: https://cloudevents.io
[aigw]: https://aigateway.envoyproxy.io
[cnpg]: https://cloudnative-pg.io
