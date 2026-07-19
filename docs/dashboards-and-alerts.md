# Dashboards and alerts

This service exposes Prometheus metrics at `GET /metrics`
(`internal/server/metrics.go`). Two kinds of declarative artifacts consume
them, all under `config/components/observability/` (moved there from
`config/observability/` when this component was wired up — see "Deployment
status" below for why they live inside the component now):

- `grafana-dashboard-sre.json` and `grafana-dashboard-product.json` — two
  Grafana dashboard models (split from a single 27-panel dashboard — see
  "Why two dashboards" below). Both use a `datasource` template variable
  (Prometheus type) instead of a hardcoded UID, so they're portable across
  Grafana instances. In this repo they're imported automatically via
  `grafanadashboard.yaml` (two `GrafanaDashboard` custom resources, see
  below) rather than a manual "Import Dashboard" click, but the raw JSON is
  still importable by hand into any other Grafana instance if you want it
  elsewhere.
- `alerts.yaml` — a `PrometheusRule` custom resource (the
  `monitoring.coreos.com/v1` CRD from prometheus-operator), containing the
  alerting rules described below.

## Why two dashboards

The original single dashboard mixed two audiences with different review
cadences: on-call engineers who need "is the service up" during an incident,
and product/quality reviewers who care about conversation and tooling health
signals that never page anyone. One dashboard meant both audiences waded
through panels that weren't relevant to them. It's now split:

- **`assistant-sre`** ("Assistant — SRE / On-Call", tags
  `assistant`/`milo-os`/`sre`/`on-call`) — for on-call triage during an
  incident, and pairs directly with `alerts.yaml`. Contains: **Service
  health** (HTTP request rate, 4xx/5xx error rate, in-flight requests,
  p50/p95/p99 request latency, readiness-probe failure rate — built from
  `assistant_http_requests_total`, `assistant_http_request_duration_seconds`,
  `assistant_http_requests_in_flight`, `assistant_readiness_failures_total`),
  **Conversation turns — failure signal** (turn rate by state, and
  failed+canceled rate as a percentage of all turns — pairs with the
  `AssistantTurnFailureRateHigh` alert), and **Model calls — failure signal**
  (model-call error rate — pairs with `AssistantModelCallErrorRateHigh`).
  Turn/model *duration* percentiles were deliberately left off this
  dashboard — they're useful context but not what you triage an incident
  against; they moved to the product/quality dashboard instead.
- **`assistant-product-quality`** ("Assistant — Product & Quality", tags
  `assistant`/`milo-os`/`product`/`quality`) — signals that don't page
  anyone but matter for conversation and tooling quality. Contains:
  **Conversation turns — duration & quality** (turn duration percentiles,
  and p95 by state), **Tool calls** (call rate by `tool`, an error-rate bar
  panel by `tool`, and a success/error breakdown table — kept together here
  in full, since tool-call health is a quality/reliability-of-integrations
  signal rather than an uptime one), **Model calls — latency** (latency
  percentiles), **History compaction** (compaction rate by outcome, and the
  dedicated, visually distinct `failed_open` panel — see below), **Capability
  gap reports** (report rate by outcome — a business signal), and
  **Span-derived RED metrics** (rate/error/duration derived from trace spans
  — exploratory/debugging-oriented, fits here better than on-call triage).

Panel-by-panel detail:

- **Service health** (`assistant-sre`) — HTTP request rate, 4xx/5xx error
  rate, in-flight requests, p50/p95/p99 request latency, and readiness-probe
  failure rate. Built from `assistant_http_requests_total`,
  `assistant_http_request_duration_seconds`,
  `assistant_http_requests_in_flight`, and
  `assistant_readiness_failures_total`.
- **Conversation turns** (split) — turn rate by state and failed+canceled
  rate live on `assistant-sre`; duration percentiles and p95-by-state live on
  `assistant-product-quality`. All built from
  `assistant_conversation_turn_duration_seconds`.
- **Tool calls** (`assistant-product-quality`) — call rate by `tool`, an
  error-rate bar panel by `tool` (easier to spot one bad tool than a tangled
  time series), and a table breaking out success vs. error counts per tool.
  Built from `assistant_tool_call_total`.
- **Model calls** (split) — error rate lives on `assistant-sre`; latency
  percentiles live on `assistant-product-quality`. Both built from
  `assistant_model_call_duration_seconds`.
- **History compaction** (`assistant-product-quality`) — compaction rate by
  outcome, and a dedicated, visually distinct panel for the `failed_open`
  rate. `failed_open` means summarization errored and the turn fell back to
  plain truncation rather than blocking (see
  `docs/conversation-summarization-design.md`), so it never shows up as a
  turn failure or an HTTP error — this panel is the only place it's visible.
  Built from `assistant_history_compaction_total`.
- **Capability gap reports** (`assistant-product-quality`) — report rate by
  outcome. A business signal, not just an ops one: a sustained spike means a
  provider's tooling has a real, active gap users keep hitting (see
  `docs/capability-gap-reporting-design.md`). Built from
  `assistant_gap_report_total`.
- **Span-derived RED metrics (OTel Collector spanmetrics connector)**
  (`assistant-product-quality`) — rate, error-rate, and p50/p95/p99 duration
  panels built entirely from trace spans, not app-side metrics. The shared
  telemetry stack's OTel Collector runs a `spanmetrics` connector that
  derives these from every span this service emits (see
  `docs/observability.md`'s Tracing section for the span list), with zero
  extra app-side instrumentation. Confirmed live (see "Deployment status"
  below) with the exact metric/label names: `span_metrics_calls_total` and
  `span_metrics_duration_milliseconds_{bucket,count,sum}`, labeled
  `service_name`, `span_name` (`assistant.http`, `conversation.turn`,
  `model.stream`, `tool.execute`, and outbound `HTTP POST` client spans),
  `span_kind`, and `status_code` (`STATUS_CODE_UNSET`/`STATUS_CODE_ERROR`).
  This is a second, independent confirmation of the same rate/error/duration
  signal the app-side metrics already provide — useful as a cross-check, and
  it comes for free once tracing reaches the collector.

### Deferred: a per-provider dashboard

A third tier — a per-provider dashboard scoped to just one provider's own
tool-call and capability-gap-report data — was discussed and explicitly
deferred, not built. It needs real tenant-scoped RBAC first: there is no
`Role`/`ProtectedResource` config today for the `conversations` or
`capabilitygapreports` resources, so there's no way to actually restrict a
per-provider dashboard's underlying data to that provider's own tenant —
building it now would either require fabricating scoping that doesn't
enforce anything, or shipping a dashboard that leaks every other provider's
data. Revisit once tenant-scoped RBAC exists for those resource types.

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

## Deployment status — what's actually wired up, and what was verified live

**Corrected from an earlier version of this doc**, which said no
Prometheus/Grafana/prometheus-operator existed anywhere and that these files
were pure specification. That was wrong, or at least incomplete: the shared
dev cluster environment this repo builds on
(`datum-cloud/test-infra`, pinned at `v0.7.0` in `Taskfile.yaml`) ships a
full, **optional** telemetry stack — it's just not deployed by default. What
follows is what this repo now wires on top of it, and exactly what was
exercised end to end on a live kind cluster while building this, not what
should theoretically work.

### The stack itself (test-infra, not this repo)

`task test-infra:install-observability` deploys, via Flux `HelmRelease`s into
a `telemetry-system` namespace:

- **Victoria Metrics** (`victoria-metrics-k8s-stack` chart) — `vmsingle`
  (Prometheus-API-compatible store, `http://vmsingle-telemetry-system-vm.telemetry-system.svc.cluster.local:8428`),
  `vmagent` (scraping, `selectAllByDefault: true` — confirmed live, no label
  restriction needed on `ServiceMonitor`/`PodMonitor` objects anywhere in the
  cluster), `vmalert` (rule evaluation, also `selectAllByDefault: true`), and
  Alertmanager. The operator auto-converts standard prometheus-operator
  `ServiceMonitor` and `PrometheusRule` CRDs into its own `VMServiceScrape`/
  `VMRule` — confirmed live via `kubectl get vmrule -A`, which shows this
  service's `assistant-alerts` rule as `operational`.
- **Tempo** (traces) — `http://telemetry-system-tempo.telemetry-system.svc.cluster.local:3100`
  (the chart exposes its query API on port 3100, not the more common 3200).
- **Grafana**, via the Grafana Operator — a `Grafana` CR with a NodePort
  Service. **Confirmed live: `http://localhost:30000` (admin/datum123) is
  directly reachable with no port-forward**, because kind's cluster config
  maps that NodePort to the host (`docker inspect` on the kind control-plane
  container shows `0.0.0.0:30000->30000/tcp`) — not merely an echoed claim
  from `cluster-up`'s output.
- An `OpenTelemetryCollector` CR (`otel-collector`, daemonset mode) — OTLP
  receivers on grpc `:4317` / http `:4318`. Its generated Service, confirmed
  live via `kubectl -n telemetry-system get svc`, is
  `otel-collector-collector.telemetry-system.svc.cluster.local`. Its trace
  pipeline fans out to `otlphttp/tempo` **and** a `spanmetrics` connector
  (metrics namespace `span_metrics`, not the `http.method`/`http.status_code`
  dimensions an older connector version would use — see the dashboard section
  above for the exact confirmed label set) feeding `prometheusremotewrite`
  into Victoria Metrics.

One live-cluster wrinkle worth recording: on first bring-up, `cert-manager`'s
`cainjector` OOM-killed repeatedly under the extra CRD/webhook watch load
(its default 64Mi limit), which stalled the OTel Collector's admission
webhook cert injection; bumping its memory limit unblocked it. Separately,
the Loki subchart's default `chunksCache`/`resultsCache` (memcached
sidecars) request ~9.8Gi each — larger than this kind node's entire
allocatable memory (~7.9Gi) — so those were disabled
(`chunksCache.enabled: false`, `resultsCache.enabled: false`) to let Loki's
single-binary pod schedule. Both are live-cluster/test-infra-chart-defaults
issues, not something this repo's own config caused or fixes; noted here so
a future from-zero bring-up isn't surprised by them. Loki/log aggregation
itself was not otherwise exercised as part of this work (this repo doesn't
ship structured logs to it) — traces and metrics were the focus.

### What this repo adds on top (new in this change)

All of it lives in `config/components/observability/`, a kustomize
Component — **opt-in only**. The plain `dev`/`dev-anthropic` overlays are
untouched (verify with `kubectl kustomize config/overlays/dev | grep OTEL` —
nothing), so `task dev:setup` with no observability stack installed keeps
working exactly as before: `internal/tracing.Setup` installs a genuine no-op
tracer when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset (no dial, no export
errors), so there's nothing to break. Two new overlays layer the component
on: `config/overlays/dev-observability` and
`config/overlays/dev-anthropic-observability`.

The component adds:

1. `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector-collector.telemetry-system.svc.cluster.local:4317`
   on the assistant and assistant-apiserver Deployments (grpc port 4317,
   matching `internal/tracing`'s `otlptracegrpc` exporter).
2. `servicemonitor-assistant.yaml` — a `ServiceMonitor` for the assistant's
   plaintext `:7820/metrics`. **Not** added for assistant-apiserver: its
   `/metrics` (from the generic `k8s.io/apiserver` library) is served over
   HTTPS with delegated `SubjectAccessReview` authorization
   (`--authorization-always-allow-paths` only covers `/healthz,/readyz,/livez`
   — see `config/base/assistant-apiserver/deployment.yaml`), so scraping
   it would need a bearer token bound to a ClusterRole granting `get` on the
   nonResourceURL `/metrics`, wired to `vmagent`'s ServiceAccount. That's a
   real, if small, RBAC change to a shared operator's ServiceAccount; left as
   a follow-up rather than half-wiring it. Separately, and independent of
   metrics: assistant-apiserver's own `NetworkPolicy`
   (`config/base/assistant-apiserver/networkpolicy.yaml`) has a fixed
   egress allow-list (DNS, the Postgres store, the kube-apiserver) that does
   **not** include `telemetry-system:4317` — so even though this component
   sets `OTEL_EXPORTER_OTLP_ENDPOINT` on that Deployment too, traces from
   assistant-apiserver would currently be silently dropped at the
   network layer if that pod ever emitted any (it wasn't exercised in this
   pass — the assistant's chat path was the one driven end to end). Widening
   that NetworkPolicy is a one-line follow-up, deliberately not made here to
   keep this change scoped to what was actually verified.
3. `alerts.yaml` (moved here from the old `config/observability/`) and
   `grafanadashboard.yaml` — two `GrafanaDashboard` CRs (`assistant-sre`,
   `assistant-product-quality`), each wrapping its own JSON model
   (`grafana-dashboard-sre.json`, `grafana-dashboard-product.json`) via its
   own generated ConfigMap. Both live in `patch-playground` (alongside the
   app), not `telemetry-system`, with `spec.allowCrossNamespaceImport: true`
   — the Grafana Operator's cross-namespace import is gated per-dashboard by
   that field, not by a global restriction from the Grafana instance's own
   `allowCrossNamespaceImport` (that one only governs the
   victoria-metrics-k8s-stack chart's *own* auto-generated dashboards, which
   already live in `telemetry-system`). Confirmed live: both CRs' status show
   `"Dashboard was successfully applied to 1 instances"`, and
   `GET /api/search?query=assistant` against the Grafana API lists both.

### What was verified live, end to end

On the shared kind cluster, with `task test-infra:install-observability` run
and this repo's `dev-observability` overlay deployed (image rebuilt from
current source — the `:dev` image already loaded in the cluster predated the
tracing/metrics work and had to be rebuilt with `task dev:build && task
dev:load` before any of this would show up; a stale image is the one failure
mode worth calling out explicitly since it produces no errors anywhere, just
silent absence):

- **Traces in Tempo**: `POST /a2a` chat turns via `patch chat` produced a
  full trace — queried Tempo's search API
  (`http://telemetry-system-tempo.telemetry-system.svc.cluster.local:3100/api/search`)
  and fetched a specific trace by ID, confirming the span tree
  `assistant.http` → `conversation.turn` → `model.stream` (×2, one per model
  call) + `tool.execute` → nested outbound `HTTP POST` (the MCP tool call to
  StreamCo), exactly matching `docs/observability.md`'s Tracing section.
- **App metrics in Victoria Metrics**: queried `vmsingle`'s
  `/api/v1/query?query=assistant_conversation_turn_duration_seconds_count`
  and got back real series with `state="completed"`, labeled with
  `job="assistant"` and the pod/namespace vmagent adds — confirming the
  `ServiceMonitor` scrape path works.
- **Spanmetrics-derived metrics in Victoria Metrics**: queried
  `/api/v1/label/__name__/values` and found `span_metrics_calls_total` and
  `span_metrics_duration_milliseconds_{bucket,count,sum}` (exact names, not
  a guess), then queried `span_metrics_calls_total` directly and got
  per-span-name series (`assistant.http`, `conversation.turn`,
  `model.stream`, `tool.execute`, `HTTP POST`) with real label sets
  (`span_kind`, `status_code`) — confirming the spanmetrics-connector path
  from spans to metrics.
- **Alert rule conversion**: `kubectl get vmrule -n patch-playground` shows
  `assistant-alerts` as `operational` with no sync error, confirming Victoria
  Metrics' operator converted the `PrometheusRule` and `vmalert` has it
  loaded. **Not** verified: that any individual rule actually *fires* under
  real error/latency conditions (e.g. deliberately driving the 5xx rate above
  5% to watch `AssistantHighErrorRate` go active) — only that the rules
  parsed, loaded, and are in vmalert's active set.
- **Dashboard rendering (both dashboards)**: confirmed via the Grafana API
  (`/api/search`, `/api/dashboards/uid/assistant-sre`,
  `/api/dashboards/uid/assistant-product-quality`) that both CRs synced with
  `"Dashboard was successfully applied to 1 instances"`, and that panel
  counts match the split: `assistant-sre` has 11 panel objects (3 row headers
  + 8 data panels: Service health in full, plus the turn/model
  failure-focused panels), `assistant-product-quality` has 18 (6 row headers
  + 12 data panels: turn/model duration, all of Tool calls, both History
  compaction panels, Capability gap reports, and the spanmetrics row). Drove
  a real chat turn (`task dev:chat MSG="Diagnose pipeline p-1 for
  StreamCo"`) and then queried representative panel PromQL from each
  dashboard directly through Grafana's own datasource proxy
  (`/api/datasources/proxy/uid/victoria-metrics-datasource/api/v1/query`):
  `assistant-sre`'s HTTP request rate panel returned real nonzero series
  (`GET /readyz`, `GET /healthz`, etc.), and `assistant-product-quality`'s
  turn-duration-p50 and spanmetrics call-rate panels returned real nonzero
  values (e.g. p50 = 0.0375s, `assistant.http` span rate = 0.327/s)
  immediately after that chat turn. The old single `assistant-overview`
  dashboard CR and its ConfigMap were deleted from the live cluster (`kubectl
  apply -k` doesn't prune resources removed from kustomize output, so this
  was a manual `kubectl delete` after applying the split) — confirmed
  `kubectl -n patch-playground get grafanadashboard` now lists only the two
  new CRs. Not opened in an actual browser to eyeball pixel rendering —
  API-level confirmation only.

The live cluster was left with all of this deployed (the `telemetry-system`
namespace, the `dev-observability`-overlay wiring in `patch-playground`) so
it can be inspected directly rather than torn down.

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
