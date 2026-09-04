# Patch — Assistant Service

**Patch** is the Datum Cloud assistant: ask it about your project in
plain language — "why is my pipeline lagging?" — and it answers using
the real state of your services, running their own diagnostic tools and
explaining what it finds.

Patch is itself a service in the **Milo service catalog**
(`assistant.miloapis.com`), and it works the way everything on the
platform works:

1. **Providers register services in the catalog.** A service that wants
   to be part of Patch publishes its agent capabilities alongside its
   catalog registration: what the service is, what an assistant should
   know about it, which of its tools the assistant may call (a reviewed
   allow-list — never the whole API), and which skills — reviewed,
   step-by-step procedures — it may follow.
2. **Entitlements decide who gets what.** When a customer's project is
   entitled to a service, that service's capabilities appear in the
   project's Patch — automatically. No entitlement, no capability;
   revoke it and the next conversation turn no longer has it.
3. **Every conversation is composed per project.** Patch assembles the
   knowledge and tools of exactly the services *your* project is
   entitled to, remembers your conversation (durably, scoped to the
   project), and does the work.
4. **Usage is metered like any other catalog service.** Every model
   call and every provider tool invocation lands on the platform's
   billing pipeline, attributed to the project and conversation — the
   same metrics-to-billing path the rest of the catalog uses.

Who gets what out of it:

| Audience | What Patch gives them |
|---|---|
| Customers / developers | An operator that can *do things* with their services — diagnose, inspect, explain — across every service their project is entitled to. |
| Service providers | A distribution channel: publish knowledge + tools once through the catalog, reach every entitled customer's assistant. |
| The platform | Usage-based bill-back, model-vendor independence, and one enforced boundary for what agents may touch. |

This repository is Patch's runtime: a standalone Go backend that any
consumer talks to over open standards — the Cloud Portal, the bundled
`patch` CLI, or any other agent. **A2A** (Agent2Agent) is the protocol
consumers use to chat with Patch; **MCP** (Model Context Protocol) is
how Patch calls provider tools; **capability documents** are the small
JSON contract through which the catalog (or anything else) tells Patch
what a project is entitled to. Nothing here is a proprietary Datum API,
and the assistant runs standalone — the catalog is one producer of its
configuration, not a dependency.

## Try it

The full environment — the assistant behind the AI gateway, a demo
provider, and a durable store — runs on a local kind cluster:

```bash
cp .env.example .env
task dev:setup            # kind cluster + operators + the stack
task dev:forward          # assistant → localhost:1986

PATCH_URL=http://localhost:1986 \
  PATCH_TOKEN=$(kubectl -n patch-playground create token patch-dev --duration=1h) \
  go run ./cmd/patch chat -i --project demo-project
```

Then ask it to *"Diagnose pipeline p-1 for StreamCo"* and watch it call
the provider's tool and explain the result. Full walkthrough (and a
no-cluster local run) in [docs/development.md](docs/development.md).

Conversations are durable. Browse and resume past threads — this reads the
aggregated apiserver (`assistant.miloapis.com`) with your k8s identity, so it
uses `KUBECONFIG`, not `PATCH_TOKEN`:

```bash
task dev:chats                       # list this project's conversations
task dev:chats ID=<context-id>       # show one transcript
task dev:chat CTX=<context-id>       # resume it

# or directly:
go run ./cmd/patch conversations list --project demo-project
go run ./cmd/patch conversations show <context-id> --project demo-project
```

## Documentation

Start with the [architecture overview](docs/architecture/README.md); it indexes
every surface below.

**Architecture**

- [Conversation turn](docs/architecture/conversation-turn.md) — the path a
  question takes, from request to answer.
- [Capabilities](docs/architecture/capabilities.md) — how a provider's
  knowledge, tools, and skills reach a project.
- [Identity and access](docs/architecture/identity-and-access.md) — who is
  calling, and whether they may act on a project.
- [Conversation storage](docs/architecture/conversation-storage.md) — durable
  memory, compaction, and the read view.
- [Metering](docs/architecture/metering.md) — how usage reaches billing.
- [Model providers](docs/architecture/model-providers.md) — staying independent
  of any one model vendor.
- [Observability](docs/architecture/observability.md) — what the service reports
  about itself.

**Components**

- [Assistant](docs/components/assistant.md) — the service that runs
  conversations.
- [Assistant apiserver](docs/components/assistant-apiserver.md) — conversations
  as Kubernetes resources.

**Reference and guides**

- [API and consumers](docs/api.md) — the A2A wire, JSON-RPC methods, and the
  `patch` CLI.
- [Capability reference](docs/capability-reference.md) — the document schema and
  provider API.
- [Configuration](docs/configuration.md) — environment variables and model modes.
- [Development](docs/development.md) — running the service locally.
- [Deployment](docs/deployment.md) — dev and production posture.
- [Operations](docs/operations/observability.md) — dashboards, alerts, and SLOs.

Design records for shipped work live in
[docs/enhancements](docs/enhancements/).

## Status

Implemented and running: the A2A v1.0 runtime, per-project capability
composition (knowledge + tools + skills), durable conversation memory
and task store on PostgreSQL, gateway-metered billing, control-plane
TokenReview + fail-closed SubjectAccessReview authorization, and a production deployment overlay.

On the roadmap:

- **Portal A2A v1.0 client** — bring the Cloud Portal onto the v1.0 wire
  so it consumes this service like any other A2A client.
- **Conversation API surface** — a consumer-facing API (Conversation as a
  KRM resource) on top of the storage layer, to list and reopen
  conversations under platform authz.
- **Skills through the catalog** — project provider-published skills via
  the catalog's AgentBinding, not just the fixture path.
- **Untrusted-provider SSRF posture** — config-wire the capability host
  allow-list for third-party providers (the guard already supports it).
- **Signed agent cards** — enterprise trust.
