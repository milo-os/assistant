# Model providers

Patch treats the model as a replaceable part. The agent loop, the tool protocol,
and the conversation format are all independent of which vendor answers, so the
platform is not locked to one model supplier.

## The seam

One interface separates the loop from the vendor. The loop turns a message into
a model call, runs whatever tools the model asks for, feeds the results back,
and repeats until the model answers — without knowing who answered.

| Backend | Used for | Credential |
|---|---|---|
| AI gateway | The deployed posture | Held by the gateway |
| Anthropic direct | Development against a real model | Held by the service |
| Mock model | Tests and offline work | None |

## Why the gateway is the deployed path

Routing model calls through the [Envoy AI Gateway][aigw] moves three concerns
out of the service:

- **Credential isolation**: the upstream key lives at the gateway. The service
  runs with no model credential in its environment, so a compromised assistant
  leaks no vendor key.
- **Metering**: the gateway observes the real request and response, so token
  counts come from the data path rather than from the service's own estimate.
  See [Metering](./metering.md).
- **Tool enforcement**: the gateway fronts provider MCP endpoints and applies
  the reviewed allow-list independently of the service. See
  [Capabilities](./capabilities.md).

Because the gateway speaks an OpenAI-compatible API, changing model vendor is a
gateway route change rather than a service change.

## Resilience

Model calls are the least reliable part of a turn, so the loop expects failure.
Transient errors retry with backoff; a model that stays unavailable fails the
turn visibly rather than returning an answer the model did not produce.

Streaming is supported end to end, so a customer sees the answer forming instead
of waiting for a complete response.

## Testing without a vendor

The mock model makes the loop testable with no network and no key. It is a real
implementation of the same interface, so tests exercise the production tool-call
path — including multi-step tool use — rather than a simplified stand-in.

## Related documentation

- [Architecture overview](./README.md)
- [Conversation turn](./conversation-turn.md) — where the model runs.
- [Metering](./metering.md) — how usage is measured.
- [Configuration](../configuration.md) — model modes and their settings.

[aigw]: https://aigateway.envoyproxy.io
