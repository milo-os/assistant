# Observability

Patch reports on two questions that need different answers: is the service
healthy, and are the answers any good. Operational failure and answer quality
fail independently — a service can be perfectly healthy while giving useless
replies — so they are measured and displayed separately.

## Metrics

The service exports Prometheus metrics covering the stages of a turn.

| Metric | Records |
|---|---|
| `assistant_conversation_turn_duration_seconds` | End-to-end turn latency |
| `assistant_model_call_duration_seconds` | Time spent waiting on the model |
| `assistant_tool_call_total` | Provider tool invocations |
| `assistant_history_compaction_total` | Conversations summarized |
| `assistant_gap_report_total` | Capability gaps recorded |

The last two are product signals rather than health signals. Rising compaction
means conversations are outgrowing context; rising gap reports mean customers
keep asking for something no entitled tool can do.

## Traces

Patch emits OpenTelemetry spans across the hop chain of a turn, so a slow answer
can be attributed to a stage rather than guessed at. Because a turn crosses the
AI gateway and one or more provider services, the trace is the only place the
whole path is visible at once.

## Dashboards

Two dashboards, matching the two questions:

- **Service health**: request rate, error rate, latency, and dependency
  availability. The view an operator opens during an incident.
- **Product and answer quality**: tool use, compaction, and capability gaps. The
  view a product owner reads weekly to see what customers are hitting.

Splitting them keeps an incident view uncluttered by trend data, and keeps a
trend view from being read as an alarm.

## Health and readiness

The service separates liveness from readiness. Readiness accounts for the
dependencies a turn actually needs, so a pod that cannot reach its store or its
model gateway reports itself unready rather than accepting traffic it cannot
serve.

## Related documentation

- [Architecture overview](./README.md)
- [Conversation turn](./conversation-turn.md) — the stages being measured.
- [Metering](./metering.md) — billing telemetry, which is separate from this.
- [Dashboards and alerts](../operations/dashboards-and-alerts.md) — panel and
  alert detail.
