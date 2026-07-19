# Observability

This is the top-level map of how the assistant service is observed in
production: health checks, structured logs, distributed tracing, and
Prometheus metrics. Each area has one authoritative source (a package doc
comment or a dedicated doc); this page just ties them together and says what
to read next.

## Health checks

`internal/server/server.go` exposes two probes, deliberately different:

- `GET /healthz` — liveness. Always 200 while the process is up; carries no
  dependency check. A Kubernetes `livenessProbe` should point here — a
  failing dependency must never restart the pod (that just crashloops
  without fixing anything).
- `GET /readyz` — readiness. 200 only when configured dependencies are
  reachable (the durable task store, and in gateway mode the model gateway),
  503 otherwise, via the injected `Deps.ReadyCheck` (see
  `cmd/assistant/main.go`'s `readyCheck`). A `readinessProbe` should point
  here so traffic is withheld until dependencies are up, without killing the
  pod. Every 503 increments `assistant_readiness_failures_total` (see
  Metrics below).

## Logging

Structured `slog` logs, one JSON object per line, via `internal/logger`.
Every log call in `internal/agent` follows the same field convention:
`taskId`/`projectName`/`contextId` for correlation, `error` (the error's
`.Error()` string) on failure paths — and, deliberately, never user message
text or tool-call arguments. `internal/agent/conversation.go`'s
`Stream.finalize` logs one `agent.turn.completed` line per turn (`outcome`,
`durationMs`, `projectName`, `contextId`) — the turn-level completion signal;
tool invocations get their own `agent.tool.invoked` line as they happen. A
turn-start line was deliberately skipped: the completion line already
carries duration, and doubling every turn's log volume for a marker with no
extra information isn't worth it.

## Tracing

OpenTelemetry, wired in `internal/tracing` (`tracing.Setup`, called from both
binaries' `main.go`). With `OTEL_EXPORTER_OTLP_ENDPOINT` unset the global
TracerProvider is the OTel no-op implementation — every `tracer.Start` call
still runs, but allocates no span state and calls no exporter, so tracing
costs nothing until it's configured.

Spans, one per binary/package that emits them:

- `assistant.http` / `conversations-apiserver.http` — the outermost span on
  every inbound HTTP request (`otelhttp`, wired in each `server.New`),
  extracting an inbound W3C `traceparent` or starting a new trace.
- `conversation.turn` — one per `Conversation.Run` call, closed by
  `endTurnSpan` in `internal/agent/tracing.go` with a `conversation.outcome`
  attribute (`completed`/`failed`/`canceled`). This is the parent of every
  span below.
- `model.stream` — one per model inference call (`tracedModel` decorator),
  spanning the whole streamed response, not just the time to open it.
- `tool.execute` — one per tool call, provider MCP or built-in
  (`tracedTools` decorator), carrying only the tool's name.
- Outbound MCP calls propagate the trace context to provider servers, so a
  provider's own tracing (if any) links back into the same trace.

Every span here carries only infrastructure attributes (tool/model
identifiers, outcome, error presence) — never user message text, tool-call
arguments, or tool output. This mirrors the privacy posture
`report_capability_gap`'s tool description establishes for its own
model-authored summary field (see `internal/capability/gapreport.go`) and is
pinned by a dedicated privacy-regression test in `internal/agent`.

## Metrics

`GET /metrics` (Prometheus exposition) is served by `internal/server`, which
owns the HTTP-level collectors (`assistant_http_requests_total`,
`assistant_http_request_duration_seconds`, `assistant_http_requests_in_flight`,
`assistant_taskstore_errors_total`, `assistant_readiness_failures_total`).

The application-level collectors — conversation turns, tool calls, model
calls, history compaction, capability-gap reports — live in `internal/metrics`
(package `Metrics`) instead, specifically so `internal/agent`,
`internal/history`, and `internal/capability` (none of which import
`internal/server`) can record them without an import cycle. `cmd/assistant/main.go`
constructs a single `*metrics.Metrics` at boot and injects the SAME instance
into both `server.Deps.Metrics` (so its collectors land on the `/metrics`
registry) and `agent.Deps.Metrics` (so the agent layer's calls observe into
it) — the same dependency-injection convention already used for
`internal/usage.Emitter`. Unlike optional collaborators such as
`agent.Deps.Memory` or `agent.Deps.GapReports` (nil disables the feature),
`Deps.Metrics` is never optional: an unset value is backed by a fresh
instance so metrics always record, just unshared with the HTTP endpoint in
that case.

The five metrics, what records them, and where:

| Metric | Type | Labels | Recorded in |
|---|---|---|---|
| `assistant_conversation_turn_duration_seconds` | histogram | `state` (completed/failed/canceled) | `Stream.finalize`, next to `endTurnSpan` |
| `assistant_tool_call_total` | counter | `tool`, `outcome` (success/error) | `tracedTools`' `tracingTool.Execute`, next to the `tool.execute` span |
| `assistant_model_call_duration_seconds` | histogram | `outcome` (success/error) | `tracedModel`'s stream-reader `endSpan`, next to the `model.stream` span |
| `assistant_history_compaction_total` | counter | `outcome` (success/failed_open) | `Conversation.maybeCompact`, at the same summarize/`Compact` fail-open branches described in `docs/conversation-summarization-design.md` |
| `assistant_gap_report_total` | counter | `outcome` (success/error) | `reportCapabilityGapTool.Execute` in `internal/capability/gapreport.go` |

For what these look like on a dashboard, the exact alert thresholds, and how
the consumption pipeline (Victoria Metrics, Tempo, Grafana, the OTel
Collector's spanmetrics connector) is wired up and what was verified live,
see [`dashboards-and-alerts.md`](./dashboards-and-alerts.md); that page is
the source of truth for `config/components/observability/grafana-dashboard.json`
and `config/components/observability/alerts.yaml` and is not duplicated
here.
