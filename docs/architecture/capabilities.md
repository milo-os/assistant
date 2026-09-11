# Capabilities

A capability is what one service contributes to another service's assistant. A
provider publishes knowledge, tools, and skills once; every entitled customer's
Patch gains them. This is the distribution channel that makes Patch a platform
surface rather than a single application.

## The contract

A **capability document** is the unit of contribution. It names the provider's
tool endpoints, the reviewed list of tools an assistant may call, the knowledge
sources it should read, and the procedures it may follow.

Patch owns this schema. Producers conform to it, never the reverse, which is why
the service catalog, a fixture file in development, and a different control
plane can all feed the same runtime.

| Contribution | What it is | Cost per turn |
|---|---|---|
| Knowledge | Documents the assistant reads about the service | Fetched and added to the prompt |
| Tools | A reviewed subset of the provider's API | Listed in the prompt, called on demand |
| Skills | Reviewed step-by-step procedures | Name and one line only, body loaded on demand |

## What every project already has

Before any provider contributes anything, a project's Patch can already work
with that project: list its resources of any kind and read one whole, describe
what a kind's fields are, say where a named service is offered to it, and say
how much of its allowance is left.

These are the **base platform tools**. They are not one service's contribution
and are not entitled per project, so they are not namespaced under a provider
and no capability document asks for them — they are simply there. They run as
the person who asked, in the project the turn was authorized for, so a tool call
reads nothing that person could not read themselves.

The point is what a provider then has to publish. A service that wants an
assistant to work with its resources publishes what only it knows — what its
fields mean, how to build one, what to check when one is unwell — and inherits
the rest. Before these existed, every provider that wanted more than a read had
to build the same list, read, describe, and allowance machinery again, and each
one answered slightly differently.

| Tool | Answers |
|---|---|
| `resources_list` / `resources_get` | What is in this project, and what does this one look like |
| `schema_get` | What fields does this kind take, and which are required |
| `locations_list` | Where is this service offered to this project |
| `quota_get` | How much of the project's allowance is left |
| `resources_validate` / `resources_plan` / `resources_apply` | Change something, once the person has seen and agreed to exactly what |

## One confirmation contract

A tool that changes a customer's resources is safe only if what changes is what
somebody saw. Patch owns that check, the same way it owns the tool allow-list,
so there is one of it rather than one per provider.

Changing anything takes two steps:

1. `resources_plan` works out what would happen. It returns the exact manifests
   and a token that stands for them.
2. `resources_apply` takes those manifests and that token. If either has moved,
   it refuses and nothing changes.

That catches an edited manifest, a reordered list, a token from another project,
and a resource someone else changed in between. A change nobody saw has no token
at all, so nobody can apply it.

The token proves the change is the one shown. It cannot prove the person agreed.
Agreement is a human step, required by Patch's fixed operating rules: nothing
the assistant reads in a tool result, a provider document, or a resource's
status counts as the person's answer. Those rules sit outside the persona a
deployer can replace, so rebranding cannot drop them.

A plan can also carry several manifests and order them, so a change that spans
services is one plan. A first deployment that needs a network from another
service is shown once and agreed once.

## Composition

Given the documents a project is entitled to, Patch builds that project's
assistant for the turn:

- **Knowledge** is fetched over HTTP under a short timeout and a per-source byte
  cap, then rendered with a provenance header so the model can attribute what it
  read.
- **Tools** become one MCP client per server entry, exposing only the tools the
  allow-list names, namespaced by provider so two services may publish the same
  tool name. Clients open per turn and close when it ends.
- **Skills** contribute only a name and a one-line description to the prompt.
- **Base platform tools** are added last and unconditionally, bound to the
  caller's own credential and the turn's project. A provider tool cannot shadow
  one: every provider name carries its service prefix, and a base tool's does
  not.

Composition is per project and per turn. Nothing is cached across projects, so
an entitlement change takes effect on the next message rather than at redeploy.

## Why skills load on demand

A skill sits between knowledge and tools: a reviewed procedure that tells the
model which tool to run, how to interpret the result, and what to check before
recommending an action.

Only the name and description enter the prompt. When a request matches, the
model calls a built-in tool that fetches the body. A provider can therefore
publish many procedures at almost no prompt cost, and the assistant reads one
only when it is relevant.

A skill is provider content the model follows, which is exactly why it passes
through the platform's review gate before any customer's assistant sees it. A
skill grants no privilege: it can only point at tools that are independently on
the enforced allow-list. Executable skill bundles are deliberately unsupported.

## Allow-lists are enforced twice

The tool allow-list is enforced in Patch, when it builds the tool set, and again
at the AI gateway, which fronts every provider endpoint. A tool absent from the
gateway's route is not callable even if a capability document names it.

The duplication is intentional. The two checks fail independently, and the
gateway is the one a compromised or misconfigured assistant cannot bypass.

## Reading as the customer

A provider tool that reads the customer's own resources gets the caller's
credential and the turn's project on every request, so it acts as that user
rather than holding standing access. Which endpoints may receive a credential is
an operator's decision, not a document's — see
[Identity and access](./identity-and-access.md#acting-as-the-caller).

## Degrading, not failing

A provider outage never fails a chat. A transport error, a non-2xx response, an
unreadable body, or a malformed document is logged and treated as no
capabilities: the turn proceeds with what remains. Individual documents that
fail validation are skipped with a warning while valid ones still apply.

## Advertising what a project has

Discovery is per project as well. The public agent card is served
unauthenticated and stays generic: it names no provider, tool, or endpoint. An
authenticated caller that names a project it is authorized for gets the extended
card, which carries one skill per service that project's capability documents
entitle it to, with the allow-listed tool names and MCP endpoints.

The extended card is derived from the same documents, through the same project
scope gate, that the next turn would compose — so it cannot promise a service
that turn would not build. It stops there: no MCP connection is opened, and the
tool list is the declared allow-list rather than a live `tools/list`. The card
therefore claims **entitlement, not health**. A service that is entitled but down
still appears, and a router must not read the card as a liveness signal.

## Reporting what is missing

When a customer asks for something no entitled tool can do, the assistant
records a capability gap against the provider that owns the surface. Gaps land
in that provider's own project, giving providers a demand signal drawn from real
customer questions rather than from speculation.

## Related documentation

- [Architecture overview](./README.md)
- [Conversation turn](./conversation-turn.md) — where capabilities are composed.
- [Identity and access](./identity-and-access.md) — how entitlement is decided.
- [Capability reference](../capability-reference.md) — the document schema and
  provider API.
