# Patch Assistant — Real Environment Playground (BASE)

A persistent, consumer-facing environment on the shared **test-infra** kind
cluster that proves the whole assistant data plane end to end: the in-cluster Go
assistant (gateway mode, **no model credential**) answers a `patch` chat turn
by calling a provider's MCP tool **through the Envoy AI Gateway**, with every
model token metered and project-attributed at the gate and every usage event
captured by an in-cluster sink.

This is the **BASE tier**. The optional **catalog overlay** (`--with-catalog`)
adds the service-catalog CRDs + capability-provider adapter for the live
`kubectl apply` → capabilities-change demo; it is owned separately and BASE
never depends on it.

```
 host                     │ test-infra kind cluster (shared — additive installs only)
 ─────────────────────────┼──────────────────────────────────────────────────────────
  patch CLI ──:1986──▶ assistant (gateway mode, keyless)
                          │     │  model calls (OpenAI wire)      MCP tool calls
                          │     ▼                                  │
                          │  Envoy AI Gateway  ◀────── /mcp ───────┘
                          │     │  llmRequestCosts (token metering)
                          │     │  MCPRoute allow-list (streams_delete blocked)
                          │     │  BackendSecurityPolicy (injects the model key)
                          │     ▼
                          │  stub-LLM        StreamCo (MCP)      sink (usage capture)
  :1985 ─────────────────▶ (gateway, for inspecting LLM/MCP access logs directly)
```

## Prerequisites

- The shared **test-infra** kind cluster is already up (this playground never
  creates or destroys it). Its kubeconfig lives at
  `~/repos/datum-cloud/test-infra/kubeconfig`.
- `docker`, `kind`, `kubectl`, `helm`, `task` on PATH. macOS or Linux.
- Go toolchain (only to build the `patch` CLI on the host).

## Bring it up

```bash
cd ~/repos/milo-os/assistant
./deploy/playground/playground-up.sh
```

Idempotent and preflight-gated: it refuses to proceed if the shared node's
memory requests already exceed 80%, and it only ever adds resources labeled
`app.kubernetes.io/part-of=agent-framework-playground`. On a cluster without
Envoy Gateway yet, the first run also installs the shared Envoy Gateway + Envoy
AI Gateway v1.0.0 (idempotent singletons — **not** removed by teardown, since
the ephemeral e2e env relies on them too). Re-run with `--skip-build` to reuse
already-loaded images.

When it finishes it prints the connection details and a ready-to-run chat
command.

## Try it as a consumer

Build the `patch` CLI from this repo and talk to the in-cluster assistant over
A2A (the assistant Service is port-forwarded to `localhost:1986`):

```bash
go build -o /tmp/patch ./cmd/patch

# The demo chat: the assistant runs StreamCo's pipeline_diagnose tool through
# the gateway and explains the result.
PATCH_URL=http://localhost:1986 PATCH_TOKEN=pg-demo-token \
  /tmp/patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project

# The agent card (advertises the A2A surface + skills):
PATCH_URL=http://localhost:1986 /tmp/patch card
```

`pg-demo-token` grants `demo-project`; a token for another project (or no token)
is rejected — the auth boundary is real, not decorative.

## TLS posture

The playground is **dev-mode**: `AUTH_MODE=dev` (a static bearer token from a
Kubernetes Secret) and **plaintext HTTP** reached over `kubectl port-forward` on
`127.0.0.1`. There is no ingress TLS and no NodePort — nothing is exposed off
the host loopback. This is deliberate: the shared kind cluster has no
LoadBalancer and we do not modify its (pre-existing) node port mappings.
Production posture instead uses OIDC access tokens (RFC 8693 on-behalf-of),
SubjectAccessReview authorization, and TLS terminated at the gateway; the
assistant is keyless in both.

## Where the proofs live

| Proof | How to see it |
|---|---|
| **Token metering + attribution (P4)** | `kubectl -n envoy-gateway-system logs -l gateway.envoyproxy.io/owning-gateway-name=patch-playground -f` — one JSON line per model call with `gen_ai.usage.*_tokens` and `x_datum_project/conversation/agent`. |
| **MCP allow-list enforcement** | The same log shows `log.type: mcp` lines for `pipeline_diagnose`; `streams_delete` is absent from the gateway's tool list and blocked, while still reachable on a direct-to-StreamCo connection. |
| **Usage capture (P6)** | `kubectl -n patch-playground port-forward svc/sink 7811:7811 &` then `curl -s http://localhost:7811/events \| jq .` — the CloudEvents the assistant emitted for your chats. (The sink image has no shell/wget, so read it over a port-forward, or `kubectl -n patch-playground logs deploy/sink` for the accepted-batch lines.) |
| **Keyless assistant** | `kubectl -n patch-playground exec deploy/assistant -- printenv \| grep -i anthropic` prints nothing; the model key lives only in the gateway's `stub-llm-apikey` Secret. |

## Optional: a real model

By default the model is a keyless in-cluster stub. To route a real Anthropic
model through the gateway (the assistant still holds no key), create the Secret
yourself — it is **never** stored in this repo — and set the flag:

```bash
kubectl --kubeconfig ~/repos/datum-cloud/test-infra/kubeconfig \
  -n patch-playground create secret generic anthropic-apikey \
  --from-literal=apiKey="$ANTHROPIC_API_KEY"

REAL_MODEL=1 ./deploy/playground/playground-up.sh --skip-build
```

## Optional: the catalog overlay

`./deploy/playground/playground-up.sh --with-catalog` brings up the catalog
control plane + capability-provider adapter (the catalog engineer's
`CATALOG_UP_CMD`, default
`~/repos/datum-cloud/service-catalog/hack/playground/catalog-up.sh`) and flips
the assistant from the fixture ConfigMap to the adapter's HTTP API by setting
`CAPABILITY_PROVIDER_URL` and unsetting `CAPABILITY_DOCS_FIXTURE` (the two
sources are mutually exclusive). That one env flip rolls the assistant once.

The adapter lives in its own namespace (`agent-framework-playground`); the
assistant reaches it cross-namespace at
`http://capability-provider.agent-framework-playground.svc.cluster.local:8080`.
The capability documents the adapter serves must point their MCP endpoint at
**this** gateway so tool calls stay metered and allow-listed:

- MCP endpoint: `${gateway}/mcp` — a **single `/mcp` path**, no per-server URL
  suffix. The gateway federates backends by tool-name prefix
  (`streamco-backend__<tool>`), not by path.
- The managed Envoy service name is generated per-gateway (currently
  `envoy-patch-playground-patch-playground-<hash>` in `envoy-gateway-system`);
  resolve it by the `gateway.envoyproxy.io/owning-gateway-name=patch-playground`
  label rather than hardcoding.
- StreamCo's in-cluster Service (bypass/enforcement reference only) is
  `streamco.patch-playground.svc.cluster.local:7810`.

`playground-down.sh` removes only the BASE footprint (namespace
`patch-playground` + GatewayClass `patch-pg-gw`); the catalog overlay in
`agent-framework-playground` is torn down by the catalog engineer's own script.

## Tear down (exactly our layer)

```bash
# Show precisely what would be removed — this IS the teardown proof (P8):
./deploy/playground/playground-down.sh --dry-run

# Remove it (the patch-playground namespace + the patch-pg-gw GatewayClass —
# everything carrying our label; nothing else):
./deploy/playground/playground-down.sh
```

Teardown never runs `task cluster-down`, never removes the Envoy Gateway / AI
Gateway operators, and never touches another namespace. The shared cluster's
unrelated workloads (ipam/etcd/nats/chainsaw) are out of scope by construction.
