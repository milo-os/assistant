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

## Degrading, not failing

A provider outage never fails a chat. A transport error, a non-2xx response, an
unreadable body, or a malformed document is logged and treated as no
capabilities: the turn proceeds with what remains. Individual documents that
fail validation are skipped with a warning while valid ones still apply.

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
