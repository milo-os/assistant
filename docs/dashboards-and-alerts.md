# Dashboards and alerts

This service exposes Prometheus metrics at `GET /metrics`
(`internal/server/metrics.go`). Two declarative artifacts consume them,
both under `config/observability/`:

- `grafana-dashboard.json` — a Grafana dashboard model, importable via
  Grafana's "Import Dashboard" flow. Uses a `datasource` template variable
  (Prometheus type) instead of a hardcoded UID, so it's portable across
  Grafana instances — pick your Prometheus datasource on import.
- `alerts.yaml` — a `PrometheusRule` custom resource (the
  `monitoring.coreos.com/v1` CRD from prometheus-operator), containing the
  alerting rules described below.

## What's in the dashboard

Panels are grouped into rows:

- **Service health** — HTTP request rate, 4xx/5xx error rate, in-flight
  requests, p50/p95/p99 request latency, and readiness-probe failure rate.
  Built from `assistant_http_requests_total`,
  `assistant_http_request_duration_seconds`,
  `assistant_http_requests_in_flight`, and
  `assistant_readiness_failures_total`.
- **Conversation turns** — turn rate and duration percentiles by `state`
  (`completed`/`failed`/`canceled`), plus failed+canceled rate as a
  percentage of all turns. Built from
  `assistant_conversation_turn_duration_seconds`.
- **Tool calls** — call rate by `tool`, an error-rate bar panel by `tool`
  (easier to spot one bad tool than a tangled time series), and a table
  breaking out success vs. error counts per tool. Built from
  `assistant_tool_call_total`.
- **Model calls** — latency percentiles and error rate. Built from
  `assistant_model_call_duration_seconds`.
- **History compaction** — compaction rate by outcome, and a dedicated,
  visually distinct panel for the `failed_open` rate. `failed_open` means
  summarization errored and the turn fell back to plain truncation rather
  than blocking (see `docs/conversation-summarization-design.md`), so it
  never shows up as a turn failure or an HTTP error — this panel is the only
  place it's visible. Built from `assistant_history_compaction_total`.
- **Capability gap reports** — report rate by outcome. A business signal,
  not just an ops one: a sustained spike means a provider's tooling has a
  real, active gap users keep hitting (see
  `docs/capability-gap-reporting-design.md`). Built from
  `assistant_gap_report_total`.

`assistant_conversation_turn_duration_seconds`, `assistant_tool_call_total`,
`assistant_model_call_duration_seconds`, `assistant_history_compaction_total`,
and `assistant_gap_report_total` are new metrics landing alongside this
dashboard (not yet in `main` as of this writing) — panels referencing them
will show "No data" until that lands and is deployed.

## What's in alerts.yaml

Five alert names, seven rules total (two are severity-tiered pairs):

| Alert | Trigger | Severity | Notes |
|---|---|---|---|
| `AssistantHighErrorRate` | HTTP 5xx rate > 5% for 5m | warning | second rule at >20% for 5m | critical |
| `AssistantNotReady` | `assistant_readiness_failures_total` increasing for 5m | critical | readiness probe sustained failing |
| `AssistantTurnFailureRateHigh` | `state="failed"` turn rate > 10% for 10m | warning | can fire even when HTTP looks healthy |
| `AssistantCompactionFailingOpen` | `failed_open` compaction rate > 0 for 30m | info | silent-degradation signal, deliberately non-paging |
| `AssistantModelCallErrorRateHigh` | model-call error rate > 10% for 5m | warning | second rule at >40% for 5m | critical — usually points at the model provider, not this service |

Every rule carries `for:`, a `severity` label, and `summary`/`description`
annotations that name the underlying metric and threshold, so an on-call
engineer doesn't need to read the YAML or PromQL to understand what fired.

## Deployment status — read this before assuming these are "live"

**Neither of these files is wired up to anything yet.** Checked
`config/` (base, components, dependencies, overlays) for prometheus-operator
or any Prometheus deployment: there isn't one. There's no `ServiceMonitor`,
no `Prometheus` custom resource, no Prometheus Helm release or manifest
anywhere in this repo. `config/dependencies/` currently only has `cnpg`
(Postgres operator) and `ai-gateway`.

Concretely, to make `alerts.yaml` actually evaluate and page anyone, someone
still needs to:

1. Install prometheus-operator (CRDs + operator) in the cluster, likely as a
   new `config/dependencies/prometheus/` component analogous to
   `config/dependencies/cnpg/`.
2. Deploy or point at a `Prometheus` custom resource whose
   `ruleSelector`/`ruleNamespaceSelector` matches the `release: prometheus`
   label this `PrometheusRule` carries (adjust the label to match whatever
   selector convention is chosen).
3. Add a `ServiceMonitor` (or equivalent scrape config) targeting this
   service's `/metrics` endpoint — `config/base/assistant.yaml` and
   `config/base/conversations-apiserver/service.yaml` don't currently
   declare Prometheus scrape annotations or a `ServiceMonitor`.
4. Wire an Alertmanager (or hosted equivalent) to actually route the
   `severity` labels to a paging/notification channel.
5. Import `grafana-dashboard.json` into a real Grafana instance pointed at
   that Prometheus — there's no Grafana deployment in `config/` either.

Until that's done, these files are the *specification* for the consumption
layer, not a working pipeline. Treat this as the immediate follow-up scope,
not something already covered.

## Proposed SLOs (draft — not a commitment)

The following are a starting proposal for discussion, not decided targets.
They should be reviewed against real traffic/latency data once the new
conversation-turn and model-call metrics have been running in production
for a representative period, and adjusted accordingly.

- **Turn completion latency**: 99% of conversation turns that don't get
  canceled should reach a terminal state (`completed` or `failed`) within
  30 seconds, measured via
  `histogram_quantile(0.99, sum(rate(assistant_conversation_turn_duration_seconds_bucket{state!="canceled"}[5m])) by (le))`.
  30s is a placeholder — pick a number after looking at real p99s once the
  metric has data.
- **Readiness availability**: the service should report ready (readiness
  probe not failing) at least 99.9% of the time over a rolling 30-day
  window, derived from `assistant_readiness_failures_total` relative to
  probe frequency. This mirrors a fairly standard availability SLO and is
  the easier of the two to start enforcing since the metric already exists
  today.

Both are intentionally simple starting points; they are not meant to
preclude a more detailed SLO (e.g. per-tool or per-model-provider) once
there's enough data to make one meaningful.
