# e2e/gateway — Envoy AI Gateway environment

Stands up **Envoy AI Gateway** in front of the assistant stack on a local
kind cluster and proves the production metering/policy path the earlier
slices stubbed:

- model traffic flows **service → gateway → stub LLM**, with token usage
  counted **at the gateway** (`llmRequestCosts`) and attributed to a consumer
  identity (`x-datum-*`);
- provider MCP traffic flows through an **`MCPRoute`** whose `toolSelector`
  enforces the reviewed allow-list **at the gateway**;
- the assistant service holds **no** upstream model credential — a
  `BackendSecurityPolicy` injects it.

This directory is owned by the infra leg (task #13). It touches only
`e2e/gateway/`; `src/` seams are the assistant engineer's, the rest of `e2e/`
is QA's.

## Versions & compatibility

| Component | Version | Notes |
| --- | --- | --- |
| Envoy Gateway | **v1.8.1** | pinned by test-infra (Flux `OCIRepository`) |
| Envoy AI Gateway | **v1.0.0** | Helm `ai-gateway-{crds-,}helm` |
| Envoy Proxy | v1.38.x | via EG v1.8.1 |
| Gateway API | v1.5.1 (experimental) | installed by test-infra |
| Kubernetes | v1.32 (kind) | test-infra cluster |

Envoy AI Gateway **v1.0.0 is built on and pairs exactly with Envoy Gateway
v1.8.1** — the version test-infra pins (compatibility matrix: AI-GW v1.0.x ↔
EG v1.8.1+ / Envoy Proxy v1.38.x / K8s v1.32+ / Gateway API v1.5.x). No
version override was needed.

## Architecture

```
   host                              kind cluster (test-infra)
 ┌────────────┐   port-forward   ┌───────────────────────────────────────────┐
 │ assistant  │  :1975 ──────────┼─▶ Envoy (dedicated AI Gateway)             │
 │ service    │  /v1  (chat)     │     ├─ AIGatewayRoute  → AIServiceBackend ──┼─▶ stub-llm  (in-cluster)
 │ (host,SUT) │  /mcp (tools)    │     │    llmRequestCosts + BackendSecurity  │   OpenAI-compatible
 │            │                  │     └─ MCPRoute (toolSelector: 3 tools) ────┼─▶ streamco  (in-cluster)
 └────────────┘                  │                                            │   MCP provider
        │                        │  Envoy Gateway (test-infra) + AI Gateway   │
        └─ curl / patch CLI      │  controller (extension server :1063)       │
                                 └───────────────────────────────────────────┘
```

### Networking choice (CONTRACT §"Workload placement")

**Option (b): backends run in-cluster.** Docker on this machine is **colima**,
where `host.docker.internal` from inside kind pods is unreliable; running the
stub LLM and StreamCo in-cluster (ClusterIP services, resolved by CoreDNS)
makes the gateway→backend path deterministic. The assistant service stays
host-run (it is the system under test).

**Inbound: `kubectl port-forward`** (not a nodePort). The gateway's Envoy
service is `ClusterIP`; up.sh forwards host `:1975` to it. This is
independent of the kind host-port mappings — deliberately, because on this
shared machine those host ports (30080/30443) are already taken by other
clusters (see below). One forward serves both `/v1/...` (chat) and `/mcp`.

Backends image: one Node-22 image built from `../streamco` (StreamCo + its
`node_modules`) and kind-loaded. The stub LLM reuses it only for the Node
runtime and runs its own zero-dependency `stub-llm/stub-llm.mjs`, mounted from
a ConfigMap that up.sh generates from that single file.

## Environment note — the "test-infra" cluster here is shared

On this machine the kind cluster named `test-infra` is **not** an ephemeral
throwaway: it carries unrelated long-lived workloads (ipam, etcd, nats,
chainsaw). Consequences, baked into the scripts:

- up.sh **reuses** the existing cluster (idempotent `task cluster-up` only if
  absent) and installs Envoy Gateway via test-infra's own
  `task install-envoy-gateway-operator`.
- down.sh **never** deletes the kind cluster — that would destroy the shared
  work. Cluster deletion is strictly **user-initiated** (`task cluster-down`
  by hand). down.sh only removes objects it can attribute to this engagement,
  then **proves byte-restore** by diffing the cluster against a pre-install
  baseline (see below).
- A dedicated cluster (`CLUSTER_NAME=… task cluster-up`) is **not** used
  because test-infra's kind-config binds host ports 30000/30443/30080, all
  already held by the running test-infra + nso clusters, and the contract
  forbids hand-rolling a cluster.

test-infra is **used, never modified**: only its gitignored `./kubeconfig` is
(re)written; its working tree stays byte-clean.

### Guardrails (shared-cluster safety)

- Every resource carries `app.kubernetes.io/part-of=agent-framework-e2e` and
  lives in a dedicated namespace; teardown deletes by namespace/label only and
  never touches the ipam/etcd/nats/chainsaw namespaces.
- up.sh runs a **resource preflight** (aborts if node memory requests > 80%)
  and captures a **baseline snapshot** (`snapshot.sh baseline` →
  `out/cluster-snapshot.baseline.txt`) before installing anything.
- down.sh restores to the pre-engagement state — removing Envoy AI Gateway and
  Envoy Gateway **only when this engagement installed them** (baseline-gated:
  anything predating us is preserved) — and writes `out/cluster-restore.diff`
  proving the cluster matches the baseline.

### The one test-infra override

Envoy AI Gateway drives Envoy Gateway through EG's **extension server**, so
the EG controller must be told about the AI Gateway controller
(`extensionManager` → `:1063`) and have the Backend API + EnvoyPatchPolicy
enabled. test-infra installs EG via a Flux `HelmRelease` whose values only set
`gateway.controllerName`. up.sh applies a **JSON-merge patch to that live
HelmRelease** (`eg-helmrelease-extension.patch.yaml`) adding the extension
config, then restarts EG. down.sh reverts it. This is a live-object change,
not a repo edit — the documented override path.

## Usage

```bash
e2e/gateway/up.sh                 # bring the whole env up (idempotent)
e2e/gateway/up.sh --skip-build    # reuse the already-loaded backends image
e2e/gateway/up.sh --with-ratelimit# also enable the STRETCH token-budget 429

e2e/gateway/down.sh               # restore to pre-engagement state + prove byte-restore
e2e/gateway/down.sh --keep-aigw   # leave Envoy AI Gateway installed (only remove our namespace)
e2e/gateway/down.sh --keep-eg     # leave Envoy Gateway installed (just revert the extension patch)
```

Cluster deletion is never scripted — if you truly want the whole kind cluster
gone, run `task cluster-down` yourself (it destroys the shared workloads).

up.sh prints the connection details; they are also written to `.run/env`.

### Point the assistant at the gateway

```bash
MODEL_MODE=gateway
GATEWAY_URL=http://localhost:1975/v1     # NOTE the /v1 — the client appends /chat/completions
GATEWAY_MODEL=patch-stub-v1
# NO ANTHROPIC_API_KEY / model key — the gateway injects the upstream credential
```

AgentBinding fixtures (MCP) point at `http://localhost:1975/mcp`. **The AI
Gateway namespaces MCP tools as `streamco-backend__<tool>`**, so a gateway-mode
binding's `toolSelector.include` must list those names, e.g.
`["streamco-backend__streams_list","streamco-backend__streams_get","streamco-backend__pipeline_diagnose"]`.

### Inspect

```bash
export KUBECONFIG=/Users/scotwells/repos/datum-cloud/test-infra/kubeconfig

# JSON access log (LLM token counts + x-datum-* attribution)
kubectl -n envoy-gateway-system logs \
  -l gateway.envoyproxy.io/owning-gateway-name=patch-ai-gateway \
  | grep '"log.type":"llm"'

# tools through the gateway (exactly 3)
kubectl -n patch-ai-gateway get aigatewayroute,mcproute
kubectl -n patch-ai-gateway get pods
```

## Files

```
up.sh / down.sh                     idempotent bring-up / teardown (+ byte-restore proof)
snapshot.sh                         cluster baseline capture + restore diff
hack-ratelimit.sh                   STRETCH: EG rate-limit (Redis) addon on/off
eg-helmrelease-extension.patch.yaml the one test-infra override (EG extension)
stub-llm/stub-llm.mjs               zero-dep OpenAI-compatible stub upstream
streamco/Dockerfile                 backends image (StreamCo + Node runtime)
manifests/
  kustomization.yaml                applies 10–60 with the part-of label
  00-namespace.yaml
  10-gateway.yaml                   dedicated GatewayClass + EnvoyProxy (JSON
                                    access logs) + Gateway + ClientTrafficPolicy
  20-stub-llm.yaml                  stub LLM Deployment + Service
  30-streamco.yaml                  StreamCo Deployment + Service
  40-llm-backend.yaml               Backend + AIServiceBackend + Secret + BackendSecurityPolicy
  50-aigatewayroute.yaml            AIGatewayRoute (model patch-stub-v1) + llmRequestCosts
  60-mcp.yaml                       Backend + MCPRoute (toolSelector: 3 tools)
  70-backendtrafficpolicy.yaml      STRETCH: token-budget rate limit
```

## Model routing & metering details

- The AI Gateway extproc reads `model` from the request body and sets
  `x-ai-eg-model`, which `AIGatewayRoute` matches (`patch-stub-v1`).
- `llmRequestCosts` extracts `InputToken`/`OutputToken`/`TotalToken` from the
  upstream `usage` block into `io.envoy.ai_gateway` dynamic metadata; the
  EnvoyProxy access log emits them as `gen_ai.usage.*` JSON fields. The
  metadata keys (`llm_input_token`, …) must match between the route and the
  access-log format.
- The stub returns **real, deterministic** usage (a stable whitespace token
  count), so the gateway's counts equal what the service self-reports to the
  usage sink.
