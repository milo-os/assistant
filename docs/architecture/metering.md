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
| Tool invocation | Patch calls a provider tool | The provider's service name, project, conversation |

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
