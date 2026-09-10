# Metering

Patch is a catalog service, so what it consumes is billed like any other catalog
service. Every model call and every provider tool invocation is reported,
attributed to the project and the conversation that caused it.

## What is reported

Two event kinds, emitted as [CloudEvents][cloudevents] to the platform's usage
pipeline.

| Event | Emitted when | Carries |
|---|---|---|
| Model usage | Patch calls a model | Token counts, model identity, project, conversation |
| Tool invocation | Patch calls an **entitled** provider's tool | The provider's service name, project, conversation |

The tool event uses the service name from the provider's capability document, so
usage attributes to the service that did the work rather than to Patch. That is
what makes a provider's tools a billable contribution rather than a cost Patch
absorbs.

## Where the numbers come from

Token counts come from the AI gateway rather than from the service's own
accounting. The gateway sits in the model data path, holds the upstream
credential, and observes the real request and response, so its numbers are the
ones the vendor would bill against. Patch reports what the gateway measured
instead of estimating.

## What is not metered

Loading a skill is not a tool invocation, and no event fires for it. A skill is
provider content the model reads, so its cost is the tokens it adds to the
prompt, billed as input like any other part of the prompt. Treating it as a tool
call would bill the same work twice.

The base platform tools are not metered either, for the same reason read
differently. The tool event names the provider service that did the work, and
there is no provider in a base tool — the work is the platform reading the
customer's own project, as the customer. There is nobody to attribute it to
except Patch itself, and billing a customer twice for reading their own project
is not a charge anyone would defend. A provider tool that happens to perform the
same read still meters, because that provider ran it.

**Platform capabilities are not metered either**, and this one is about consent
rather than attribution. A platform capability is composed into every project
whether or not anything entitled that project to it — it is how the platform's
own services, the ones that keep the record of where a service is offered or
what an allowance has left, reach a conversation at all (see
[Capabilities](../capability-reference.md#platform-capabilities-every-project-no-entitlement)).
The customer did not choose it. Billing a tool invocation for a capability
nobody opted into is a charge with no purchase behind it, and an operator who
declares a platform source should get the right billing from that one setting
rather than having to remember a second one. So every platform document's
service is off the meter by virtue of being a platform document.
`CAPABILITY_UNMETERED_SERVICES` extends that set to any other service an
operator names; it cannot put a platform service back on it.

Not metered means **no event, on any meter**. It is tempting to keep emitting
something under a different name so the volume stays visible, and that is the
one thing this must not do:
`assistant.miloapis.com/conversation/tool-invocations` is a wire contract with
the billing pipeline, pinned by a golden test, and a new meter name is a change
to that pipeline — new plumbing, new rating rules, a new line on somebody's
invoice — not a change composition may make on its own. Volume that needs
watching is operational telemetry, which
[Observability](./observability.md) covers and billing does not.

## Delivery posture

Usage reporting never blocks a turn. Events are emitted alongside the
conversation rather than in its critical path, so a usage pipeline that is slow
or unavailable degrades billing telemetry rather than customer-visible chat.

The wire format is pinned by a golden file in the test suite, so a change to the
event shape shows up as a failing test rather than as a billing discrepancy
discovered later.

## Related documentation

- [Architecture overview](./README.md)
- [Conversation turn](./conversation-turn.md) — what generates usage.
- [Model providers](./model-providers.md) — where token counts are measured.
- [Observability](./observability.md) — operational telemetry, which is
  separate.

[cloudevents]: https://cloudevents.io
