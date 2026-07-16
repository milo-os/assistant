#!/usr/bin/env bash
# playground-up.sh — bring up the PERSISTENT "real environment" playground on
# the shared test-infra kind cluster (CONTRACT-REAL-ENV.md, BASE tier).
# Idempotent: safe to re-run.
#
# BASE = in-cluster assistant (gateway mode, keyless) + Envoy AI Gateway +
# StreamCo (MCP) + stub-LLM + usage sink, fixture-mode capabilities, reached by
# the `patch` CLI from the host. Bring-up does NOT depend on anything
# catalog-related (the catalog is the optional --with-catalog OVERLAY, owned by
# the catalog engineer).
#
# What it does, in order:
#   1. Preflight: tools present, cluster reachable, node memory < 80%.
#   2. Label-scoped baseline snapshot (proof P8 reference).
#   3. Ensure shared gateway infra (idempotent singletons, NOT ours to remove):
#      Envoy Gateway (test-infra Taskfile) + Envoy AI Gateway v1.0.0 (Helm) +
#      the documented EG extension-server patch.
#   4. Build + kind-load the three images (assistant, streamco, sink).
#   5. Apply the playground namespace + gateway stack + workloads, resolving the
#      managed Envoy FQDN into the assistant's GATEWAY_URL and the capability
#      document's MCP endpoint.
#   6. Expose the gateway and the assistant to the host via self-healing
#      port-forwards; print the consumer quickstart.
#
# GUARDRAILS (shared cluster — hosts the user's unrelated live workloads):
#   * NEVER runs `task cluster-down`. Additive, labeled installs only.
#   * Aborts if node memory requests already exceed 80%.
#   * test-infra repo is READ-ONLY (only its gitignored ./kubeconfig is written).
#
# Flags / env:
#   --skip-build     reuse already-loaded images.
#   --with-catalog   (overlay) — owned by the catalog engineer; this BASE script
#                    only prints where the overlay lives. BASE never needs it.
#   REAL_MODEL=1     OPTIONAL real-model leg: expects a user-created Secret
#                    `anthropic-apikey` (key: apiKey) in the playground namespace
#                    and routes a real model through the gateway. NEVER stored in
#                    the repo. Default is the keyless stub model.
set -euo pipefail

# ── Config (override via env) ─────────────────────────────────
CLUSTER="${CLUSTER:-test-infra}"
TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
AIGW_VERSION="${AIGW_VERSION:-v1.0.0}"
NS="${NS:-patch-playground}"
GW="${GW:-patch-playground}"
GC="${GC:-patch-pg-gw}"
EG_NS="${EG_NS:-envoy-gateway-system}"
AIGW_NS="${AIGW_NS:-envoy-ai-gateway-system}"

ASSISTANT_IMAGE="${ASSISTANT_IMAGE:-patch-assistant:local}"
STREAMCO_IMAGE="${STREAMCO_IMAGE:-patch-streamco:local}"
SINK_IMAGE="${SINK_IMAGE:-patch-sink:local}"

GW_PF_PORT="${GW_PF_PORT:-1985}"          # gateway → host (inspect LLM/MCP directly)
ASSISTANT_PF_PORT="${ASSISTANT_PF_PORT:-1986}"  # assistant A2A → host (patch CLI)
DEV_TOKEN="${DEV_TOKEN:-pg-demo-token}"
PROJECT="${PROJECT:-demo-project}"

# --with-catalog OVERLAY (owned by the catalog engineer; BASE never needs it):
# the catalog bring-up script + the capability-provider adapter URL the assistant
# switches to (mutually exclusive with the fixture ConfigMap). The adapter lives
# in the catalog's own namespace (agent-framework-playground); the assistant
# reaches it cross-namespace by FQDN.
CATALOG_UP_CMD="${CATALOG_UP_CMD:-$HOME/repos/datum-cloud/service-catalog/hack/playground/catalog-up.sh}"
CAPABILITY_PROVIDER_URL="${CAPABILITY_PROVIDER_URL:-http://capability-provider.agent-framework-playground.svc.cluster.local:8080}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"     # assistant repo root
MANIFESTS="$SCRIPT_DIR/manifests"
RUN_DIR="$SCRIPT_DIR/.run"
KCFG="$TEST_INFRA_DIR/kubeconfig"
STUB_LLM_SRC="$ROOT/e2e/gateway/stub-llm/stub-llm.mjs"

SKIP_BUILD=0
WITH_CATALOG=0
for arg in "$@"; do
  case "$arg" in
    --skip-build)   SKIP_BUILD=1 ;;
    --with-catalog) WITH_CATALOG=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

mkdir -p "$RUN_DIR"
say()  { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[1;33m!\033[0m %s\n' "$*"; }
kc()   { kubectl --kubeconfig "$KCFG" "$@"; }

# Re-export the kubeconfig and verify the API is reachable. kind reassigns the
# API host port when docker/colima restarts, stranding a stale address; call
# this before any API-heavy step.
ensure_api() {
  for _ in 1 2 3; do
    kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KCFG" >/dev/null 2>&1 || true
    kc cluster-info >/dev/null 2>&1 && return 0
    sleep 2
  done
  echo "cluster API not reachable via $KCFG (kind cluster '$CLUSTER')" >&2
  return 1
}

# ── 0. Preflight ──────────────────────────────────────────────
say "Preflight: tools + cluster"
for t in docker kind kubectl helm task; do
  command -v "$t" >/dev/null 2>&1 || { echo "missing required tool: $t" >&2; exit 1; }
done
ok "tools present"

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "ABORT: kind cluster '$CLUSTER' not found. The playground attaches to the" >&2
  echo "shared test-infra cluster; bring it up via test-infra first (task cluster-up)." >&2
  echo "This script never creates or destroys the shared cluster." >&2
  exit 1
fi
say "Refresh test-infra kubeconfig + verify API (gitignored artifact)"
ensure_api && ok "cluster reachable via $KCFG"

# Resource preflight (guardrail): never add load to a memory-pressured shared node.
say "Resource preflight"
mempct="$(kc describe node "${CLUSTER}-control-plane" 2>/dev/null \
  | awk '/Allocated resources:/{a=1} a&&/^[[:space:]]*memory/&&/%/{gsub(/[()%]/,"",$3); print $3; exit}')"
if [ -n "$mempct" ] && [ "$mempct" -eq "$mempct" ] 2>/dev/null; then
  ok "node memory requests committed: ${mempct}%"
  if [ "$mempct" -gt 80 ]; then
    echo "ABORT: node memory requests ${mempct}% > 80% — refusing to add load to the shared cluster." >&2
    exit 1
  fi
else
  warn "could not read node memory commit (continuing)"
fi

# ── 1. Baseline (label-scoped) ────────────────────────────────
say "Snapshot label-scoped baseline (our footprint before install)"
"$SCRIPT_DIR/snapshot.sh" inventory | tee "$RUN_DIR/inventory.baseline.txt" >/dev/null
ok "baseline → $RUN_DIR/inventory.baseline.txt"

# ── 2. Shared gateway infra (idempotent; NOT ours to remove) ──
# Only INSTALL when absent. Re-invoking the test-infra task / helm upgrade on an
# already-healthy install re-applies the Flux HelmRelease and rolls EG, which on
# a busy shared node churns leader election across two ReplicaSets and stalls.
# When EG is already Available we leave it strictly alone.
say "Ensure Envoy Gateway (test-infra: task install-envoy-gateway-operator)"
if kc -n "$EG_NS" get deploy envoy-gateway >/dev/null 2>&1; then
  if kc -n "$EG_NS" wait --for=condition=Available deploy/envoy-gateway --timeout=10s >/dev/null 2>&1; then
    ok "Envoy Gateway already installed + Available (leaving it untouched)"
  else
    # Present but not Ready: the shared operator pod can crashloop under the same
    # VM contention that flaps cm/scheduler. Do NOT re-run the test-infra install
    # — it blocks indefinitely on the crashlooping operator's rollout. An
    # already-programmed Envoy data plane keeps serving without the operator, so
    # on a re-run (our routes already programmed) it is safe to continue.
    warn "Envoy Gateway operator present but NOT Ready — likely shared-cluster CP contention."
    warn "NOT re-running the test-infra install (it would block on the crashlooping operator)."
    warn "An already-programmed data plane still serves; continuing."
  fi
else
  ( cd "$TEST_INFRA_DIR" && task install-envoy-gateway-operator )
  kc -n "$EG_NS" rollout status deploy/envoy-gateway --timeout=180s \
    || kc -n "$EG_NS" wait --for=condition=Available deploy/envoy-gateway --timeout=60s \
    || warn "Envoy Gateway not Available after install (shared CP may be flapping) — continuing"
  ok "Envoy Gateway install attempted"
fi

say "Ensure Envoy AI Gateway $AIGW_VERSION (Helm)"
ensure_api
if kc -n "$AIGW_NS" get deploy ai-gateway-controller >/dev/null 2>&1; then
  if kc -n "$AIGW_NS" wait --for=condition=Available deploy/ai-gateway-controller --timeout=10s >/dev/null 2>&1; then
    ok "Envoy AI Gateway already installed + Available (skipping helm upgrade)"
  else
    warn "AI Gateway controller present but NOT Ready — likely shared-cluster CP contention."
    warn "NOT re-running helm (--wait would block on the crashlooping controller)."
    warn "Already-programmed routes still serve; continuing."
  fi
else
  helm --kubeconfig "$KCFG" upgrade -i aieg-crd \
    oci://docker.io/envoyproxy/ai-gateway-crds-helm --version "$AIGW_VERSION" \
    --namespace "$AIGW_NS" --create-namespace --wait
  helm --kubeconfig "$KCFG" upgrade -i aieg \
    oci://docker.io/envoyproxy/ai-gateway-helm --version "$AIGW_VERSION" \
    --namespace "$AIGW_NS" --create-namespace --wait
  kc -n "$AIGW_NS" wait --for=condition=Available deploy --all --timeout=180s \
    || warn "AI Gateway not Available after install (shared CP may be flapping) — continuing"
  ok "AI Gateway install attempted"
fi

EXT_SVC="$(kc -n "$AIGW_NS" get svc -o jsonpath='{range .items[?(@.spec.ports[0].port==1063)]}{.metadata.name}{end}' 2>/dev/null || true)"
[ -z "$EXT_SVC" ] && EXT_SVC="ai-gateway-controller"
EXT_FQDN="${EXT_SVC}.${AIGW_NS}.svc.cluster.local"
ok "extension server: ${EXT_FQDN}:1063"

say "Patch Envoy Gateway HelmRelease → enable AI Gateway extension (idempotent)"
# Was the extension already live BEFORE we patched? If so (a re-run), the EG
# pod already carries it — restarting is not only unnecessary, it hangs
# `rollout status`: Flux owns this deployment via the HelmRelease and reverts
# kubectl's `restartedAt` annotation, so observedGeneration never settles.
EXT_ALREADY=0
if kc -n "$EG_NS" get cm envoy-gateway-config -o yaml 2>/dev/null | grep -q 'extensionManager'; then
  EXT_ALREADY=1
fi
PATCH_FILE="$RUN_DIR/eg-extension.patch.yaml"
sed "s#ai-gateway-controller.${AIGW_NS}.svc.cluster.local#${EXT_FQDN}#" \
  "$SCRIPT_DIR/eg-helmrelease-extension.patch.yaml" > "$PATCH_FILE"
kc -n flux-system patch helmrelease envoy-gateway --type merge --patch-file "$PATCH_FILE"
kc -n flux-system annotate helmrelease envoy-gateway \
  "reconcile.fluxcd.io/requestedAt=$(date +%s)" --overwrite >/dev/null
for i in $(seq 1 60); do
  if kc -n "$EG_NS" get cm envoy-gateway-config -o yaml 2>/dev/null | grep -q 'extensionManager'; then
    ok "envoy-gateway-config includes extensionManager"; break
  fi
  [ "$i" = 60 ] && { echo "timed out waiting for EG config" >&2; exit 1; }
  sleep 2
done
if [ "$EXT_ALREADY" = 1 ]; then
  ok "extension already active — EG restart not needed (skipping)"
else
  kc -n "$EG_NS" rollout restart deploy/envoy-gateway
  # Tolerant wait: Flux churn can stall `rollout status`; falling back to the
  # Available condition (which the healthy pod satisfies) avoids a false failure.
  kc -n "$EG_NS" rollout status deploy/envoy-gateway --timeout=120s \
    || kc -n "$EG_NS" wait --for=condition=Available deploy/envoy-gateway --timeout=120s
fi
kc -n "$EG_NS" wait --for=condition=Available deploy/envoy-gateway --timeout=60s >/dev/null 2>&1 \
  || warn "Envoy Gateway not Available (operator may be crashlooping under CP contention) — the already-programmed data plane still serves; continuing"
ok "Envoy Gateway carries the AI Gateway extension"

# ── 3. Build + kind-load images ───────────────────────────────
if [ "$SKIP_BUILD" = 0 ]; then
  say "Compile assistant on host (fast, native) + build/load images"
  command -v go >/dev/null 2>&1 || { echo "missing required tool: go (to compile the assistant)" >&2; exit 1; }
  # Match the cluster node's architecture; fall back to the host's. The kind
  # nodes share the Docker VM arch, which equals the host arch on kind.
  HOST_ARCH="$(uname -m)"
  case "$HOST_ARCH" in
    arm64|aarch64) GOARCH=arm64 ;;
    x86_64|amd64)  GOARCH=amd64 ;;
    *) echo "unsupported host arch: $HOST_ARCH" >&2; exit 1 ;;
  esac
  BUILD_DIR="$ROOT/deploy/.build"
  mkdir -p "$BUILD_DIR"
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
      go build -trimpath -ldflags="-s -w" -o "$BUILD_DIR/assistant" ./cmd/assistant )
  ok "assistant compiled (linux/$GOARCH)"
  docker build --provenance=false --sbom=false -t "$ASSISTANT_IMAGE" \
    -f "$ROOT/deploy/assistant.Dockerfile" "$BUILD_DIR"
  docker build --provenance=false --sbom=false -t "$STREAMCO_IMAGE" \
    -f "$ROOT/deploy/streamco.Dockerfile" "$ROOT/e2e/streamco"
  docker build --provenance=false --sbom=false -t "$SINK_IMAGE" \
    -f "$ROOT/deploy/sink.Dockerfile" "$ROOT/e2e/sink"
  kind load docker-image "$ASSISTANT_IMAGE" "$STREAMCO_IMAGE" "$SINK_IMAGE" --name "$CLUSTER"
  ok "images loaded into kind"
else
  warn "--skip-build: reusing $ASSISTANT_IMAGE, $STREAMCO_IMAGE, $SINK_IMAGE"
fi

# ── 4. Namespace + generated config ───────────────────────────
say "Apply namespace + generated config"
kc apply -f "$MANIFESTS/00-namespace.yaml"

[ -f "$STUB_LLM_SRC" ] || { echo "stub-llm source not found: $STUB_LLM_SRC" >&2; exit 1; }
kc -n "$NS" create configmap stub-llm-src \
  --from-file=stub-llm.mjs="$STUB_LLM_SRC" \
  --dry-run=client -o yaml | kc apply -f -
kc -n "$NS" label configmap stub-llm-src \
  app.kubernetes.io/part-of=agent-framework-playground --overwrite >/dev/null

kc -n "$NS" create secret generic assistant-dev-token \
  --from-literal=tokens="${DEV_TOKEN}:demo-user:${PROJECT}" \
  --dry-run=client -o yaml | kc apply -f -
kc -n "$NS" label secret assistant-dev-token \
  app.kubernetes.io/part-of=agent-framework-playground --overwrite >/dev/null
ok "namespace, stub-llm-src, assistant-dev-token ready"

# ── 5. Gateway stack + resolve managed Envoy FQDN ─────────────
say "Apply gateway stack (10–60) + wait Programmed"
for f in 10-gateway 20-stub-llm 30-streamco 40-llm-backend 50-aigatewayroute 60-mcp; do
  kc apply -f "$MANIFESTS/$f.yaml"
done
kc -n "$NS" rollout status deploy/stub-llm --timeout=120s
kc -n "$NS" rollout status deploy/streamco --timeout=120s
kc -n "$NS" wait --for=condition=Programmed gateway/"$GW" --timeout=300s
ok "stub-llm + streamco + gateway ready"

say "Resolve managed Envoy service FQDN (in $EG_NS)"
ENVOY_SVC=""
for i in $(seq 1 30); do
  ENVOY_SVC="$(kc -n "$EG_NS" get svc \
    -l gateway.envoyproxy.io/owning-gateway-name="$GW",gateway.envoyproxy.io/owning-gateway-namespace="$NS" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [ -n "$ENVOY_SVC" ] && break
  sleep 2
done
[ -z "$ENVOY_SVC" ] && { echo "could not resolve Envoy service for gateway $GW" >&2; exit 1; }
ENVOY_FQDN="${ENVOY_SVC}.${EG_NS}.svc.cluster.local"
GATEWAY_URL="http://${ENVOY_FQDN}/v1"
GATEWAY_MCP_URL="http://${ENVOY_FQDN}/mcp"
ok "Envoy: $ENVOY_FQDN  (LLM $GATEWAY_URL, MCP $GATEWAY_MCP_URL)"

# ── 6. Capability doc + assistant + sink ──────────────────────
say "Render capability document (MCP → gateway) + assistant (GATEWAY_URL)"
CAP_FILE="$RUN_DIR/capability-documents.json"
sed "s#__GATEWAY_MCP_URL__#${GATEWAY_MCP_URL}#" \
  "$SCRIPT_DIR/capability-documents.tmpl.json" > "$CAP_FILE"
kc -n "$NS" create configmap assistant-capabilities \
  --from-file=capability-documents.json="$CAP_FILE" \
  --dry-run=client -o yaml | kc apply -f -
kc -n "$NS" label configmap assistant-capabilities \
  app.kubernetes.io/part-of=agent-framework-playground --overwrite >/dev/null

# Mode guard: a plain BASE run over a live env that's in CATALOG/adapter mode
# must NOT silently revert it to the fixture — that would tear down live overlay
# wiring (e.g. a running P3). Refuse with a clear message unless FORCE_BASE=1.
# (--with-catalog intends the switch; a fresh install has no live Deployment.)
if [ "$WITH_CATALOG" = 0 ] && [ "${FORCE_BASE:-0}" != "1" ]; then
  live_provider="$(kc -n "$NS" get deploy assistant \
    -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="CAPABILITY_PROVIDER_URL")]}{.value}{end}' 2>/dev/null || true)"
  if [ -n "$live_provider" ]; then
    echo "REFUSING: the live assistant is in CATALOG/adapter mode (CAPABILITY_PROVIDER_URL is set)." >&2
    echo "A plain BASE run would revert it to the fixture source and break the overlay wiring." >&2
    echo "  • keep overlay mode:  re-run with --with-catalog  (add CATALOG_UP_CMD=skip if the overlay is already up)" >&2
    echo "  • force BASE (revert to fixture):  FORCE_BASE=1 $0 ${*:-}" >&2
    exit 1
  fi
fi

# ── Conversation store (durable history) before the assistant: the assistant
# fails fast when CONVERSATION_STORE_URL is unreachable, so bring Postgres up
# first. CloudNativePG owns provisioning: the vendored operator manifest is
# installed into cnpg-system (idempotent, server-side apply — the CRDs are too
# big for client-side), then the Cluster CR in 65-postgres.yaml. Images are
# pulled on the HOST and kind-loaded so the cluster needs no registry egress.
# NOTE: the operator is ADDITIVE shared infra (like the Envoy Gateway stack):
# playground-down.sh removes the Cluster with the namespace but leaves
# cnpg-system in place — other tenants of the shared cluster may adopt it.
say "Conversation store (CloudNativePG)"
CNPG_OPERATOR_MANIFEST="$SCRIPT_DIR/cnpg/cnpg-operator-1.30.0.yaml"
CNPG_OPERATOR_IMAGE="${CNPG_OPERATOR_IMAGE:-ghcr.io/cloudnative-pg/cloudnative-pg:1.30.0}"
CNPG_POSTGRES_IMAGE="${CNPG_POSTGRES_IMAGE:-ghcr.io/cloudnative-pg/postgresql:17.8}"
for img in "$CNPG_OPERATOR_IMAGE" "$CNPG_POSTGRES_IMAGE"; do
  docker image inspect "$img" >/dev/null 2>&1 || docker pull "$img"
done
kind load docker-image "$CNPG_OPERATOR_IMAGE" "$CNPG_POSTGRES_IMAGE" --name "$CLUSTER" 2>/dev/null \
  || warn "kind load reported errors (attestation manifests) — verify below"
docker exec "${CLUSTER}-control-plane" crictl images | grep -q cloudnative-pg/postgresql \
  || { echo "ABORT: CNPG postgres image not on the node" >&2; exit 1; }
kc apply --server-side --force-conflicts -f "$CNPG_OPERATOR_MANIFEST"
kc -n cnpg-system rollout status deploy/cnpg-controller-manager --timeout=180s \
  || kc -n cnpg-system wait --for=condition=Available deploy/cnpg-controller-manager --timeout=180s
# The Cluster apply needs the operator's admission webhook answering; retry
# through its readiness window.
for i in $(seq 1 18); do
  kc apply -f "$MANIFESTS/65-postgres.yaml" 2>/dev/null && break
  [ "$i" = 18 ] && { echo "ABORT: CNPG webhook never admitted the Cluster" >&2; exit 1; }
  sleep 10
done
kc -n "$NS" wait --for=condition=Ready cluster/conversation-store --timeout=600s
ok "conversation store ready (CNPG cluster; history survives restarts)"

ASSISTANT_FILE="$RUN_DIR/70-assistant.yaml"
sed "s#__GATEWAY_URL__#${GATEWAY_URL}#" \
  "$MANIFESTS/70-assistant.tmpl.yaml" > "$ASSISTANT_FILE"
kc apply -f "$ASSISTANT_FILE"
# Assert exactly ONE capability source (they're mutually exclusive in config.go).
# The manifest deliberately omits both so `kubectl apply` never merges a stray
# value from a prior cross-mode run; we set the authoritative one here. This is
# idempotent (a re-run in the same mode is a no-op → no roll) and converges
# cleanly on an intentional mode switch (no both-set crash pod). --with-catalog
# re-asserts the provider URL in its own block below; setting it here too keeps
# the intermediate rollout healthy.
if [ "$WITH_CATALOG" = 1 ]; then
  kc -n "$NS" set env deploy/assistant \
    CAPABILITY_DOCS_FIXTURE- CAPABILITY_PROVIDER_URL="$CAPABILITY_PROVIDER_URL" >/dev/null
else
  kc -n "$NS" set env deploy/assistant \
    CAPABILITY_PROVIDER_URL- CAPABILITY_DOCS_FIXTURE=/config/capability-documents.json >/dev/null
fi
kc apply -f "$MANIFESTS/80-sink.yaml"

# Optional real-model leg (never stores a key in the repo).
if [ "${REAL_MODEL:-0}" = "1" ]; then
  say "REAL_MODEL=1 — wiring the real-model gateway leg"
  if ! kc -n "$NS" get secret anthropic-apikey >/dev/null 2>&1; then
    echo "ABORT: REAL_MODEL=1 requires a user-created Secret in $NS. Create it yourself:" >&2
    echo "  kubectl -n $NS create secret generic anthropic-apikey --from-literal=apiKey=\$ANTHROPIC_API_KEY" >&2
    exit 1
  fi
  if [ -f "$MANIFESTS/overlay-real-model.tmpl.yaml" ]; then
    sed "s#__ENVOY_FQDN__#${ENVOY_FQDN}#" "$MANIFESTS/overlay-real-model.tmpl.yaml" \
      | kc apply -f -
    # Point the assistant at the real model name; the gateway holds the key.
    kc -n "$NS" set env deploy/assistant GATEWAY_MODEL="${REAL_MODEL_NAME:-claude-sonnet-4-6}"
    ok "real-model leg applied (assistant stays keyless; key lives only at the gateway)"
  else
    warn "overlay-real-model.tmpl.yaml absent — skipping real-model leg"
  fi
fi

say "Wait for assistant + sink ready"
kc -n "$NS" rollout status deploy/sink --timeout=120s \
  || kc -n "$NS" wait --for=condition=Available deploy/sink --timeout=120s
# Tolerant: a flapping kube-controller-manager on the shared node stalls
# `rollout status` on observedGeneration lag even when the pod is healthy; fall
# back to the Available condition so a CP flap doesn't fail an otherwise-fine run.
kc -n "$NS" rollout status deploy/assistant --timeout=120s \
  || kc -n "$NS" wait --for=condition=Available deploy/assistant --timeout=120s
ok "assistant + sink ready"
kc -n "$NS" get aigatewayroute,mcproute 2>/dev/null || true

# ── 6b. CATALOG OVERLAY (optional; flips the capability source) ──
# BASE composes capabilities from the fixture ConfigMap. --with-catalog brings up
# the catalog control plane + capability-provider adapter (catalog engineer's
# script) and switches the assistant to the adapter's HTTP API. The two capability
# sources are MUTUALLY EXCLUSIVE (config.go rejects both), so we UNSET the fixture
# env as we set the provider URL. This flip rolls the assistant once — expected in
# overlay mode. The MCP endpoints the adapter emits should point at THIS gateway;
# see the adapter contract note printed below.
if [ "$WITH_CATALOG" = 1 ]; then
  say "--with-catalog: bring up the catalog overlay + switch capability source"
  # CATALOG_UP_CMD=skip|none|"" ⇒ the overlay is already up (owned by the catalog
  # engineer); do NOT re-run their bring-up (it would churn the SAC/entitlement
  # CRs a live P3 depends on). Just re-assert the assistant's capability source —
  # this keeps a `--with-catalog --skip-build` re-run idempotent in adapter mode.
  case "${CATALOG_UP_CMD:-skip}" in
    skip|none|"") warn "CATALOG_UP_CMD=skip — assuming the overlay is already up; only flipping the assistant" ;;
    *)
      if [ -x "$CATALOG_UP_CMD" ]; then
        ( "$CATALOG_UP_CMD" ) || warn "catalog bring-up ($CATALOG_UP_CMD) reported an issue"
      else
        warn "catalog bring-up script not found/executable at: $CATALOG_UP_CMD"
        warn "set CATALOG_UP_CMD (or =skip if the overlay is already up), or run the"
        warn "catalog engineer's overlay script yourself. Flipping the assistant to"
        warn "CAPABILITY_PROVIDER_URL regardless — it 503s until the adapter is reachable."
      fi ;;
  esac
  echo "  adapter must serve capability docs whose MCP endpoint is THIS gateway:"
  echo "    MCP base + path : $GATEWAY_MCP_URL   (single /mcp path — NO per-server suffix)"
  echo "    tool identity   : gateway federates by tool-name prefix 'streamco-backend__<tool>'"
  echo "    StreamCo direct : streamco.${NS}.svc.cluster.local:7810  (bypass ref only)"
  # Flip: provider URL in, fixture out (mutually exclusive). Redundant with the
  # apply-time assertion above when already in overlay mode (a no-op re-run), but
  # kept so a BASE→overlay switch converges here too.
  kc -n "$NS" set env deploy/assistant \
    CAPABILITY_DOCS_FIXTURE- \
    CAPABILITY_PROVIDER_URL="$CAPABILITY_PROVIDER_URL"
  kc -n "$NS" rollout status deploy/assistant --timeout=120s \
    || kc -n "$NS" wait --for=condition=Available deploy/assistant --timeout=120s
  ok "assistant now sources capabilities from $CAPABILITY_PROVIDER_URL (fixture unset)"
fi

# ── 7. Expose to host (self-healing port-forwards) ────────────
start_pf() {  # $1=svc  $2=ns  $3=localport  $4=targetport  $5=pidfile-tag
  local svc="$1" pfns="$2" lport="$3" tport="$4" tag="$5"
  local pidfile="$RUN_DIR/pf-$tag.pid" logf="$RUN_DIR/pf-$tag.log"
  if [ -f "$pidfile" ]; then kill "$(cat "$pidfile")" 2>/dev/null || true; rm -f "$pidfile"; fi
  nohup bash -c "while true; do kubectl --kubeconfig '$KCFG' -n '$pfns' port-forward 'svc/$svc' '$lport:$tport' --address 127.0.0.1; echo '[pf-$tag] reconnecting in 2s'; sleep 2; done" \
    >"$logf" 2>&1 &
  echo $! > "$pidfile"
  disown || true
}

say "Port-forward gateway → :$GW_PF_PORT and assistant → :$ASSISTANT_PF_PORT"
ENVOY_DEPLOY="$(kc -n "$EG_NS" get deploy \
  -l gateway.envoyproxy.io/owning-gateway-name="$GW" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[ -n "$ENVOY_DEPLOY" ] && kc -n "$EG_NS" rollout status "deploy/$ENVOY_DEPLOY" --timeout=120s || true
start_pf "$ENVOY_SVC" "$EG_NS" "$GW_PF_PORT" 80 gateway
start_pf assistant "$NS" "$ASSISTANT_PF_PORT" 7820 assistant

# Wait for the assistant forward to serve /healthz.
for i in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$ASSISTANT_PF_PORT/healthz" || true)"
  [ "$code" = "200" ] && { ok "assistant reachable on :$ASSISTANT_PF_PORT (HTTP 200 /healthz)"; break; }
  [ "$i" = 30 ] && { echo "assistant port-forward not healthy" >&2; cat "$RUN_DIR/pf-assistant.log" >&2; exit 1; }
  sleep 1
done
for i in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$GW_PF_PORT/" || true)"
  [ -n "$code" ] && [ "$code" != "000" ] && { ok "gateway reachable on :$GW_PF_PORT (HTTP $code /)"; break; }
  [ "$i" = 30 ] && warn "gateway port-forward not confirmed (non-fatal)"
  sleep 1
done

# ── 8. Record connection details + quickstart ─────────────────
cat > "$RUN_DIR/env" <<EOF
KUBECONFIG=$KCFG
CLUSTER=$CLUSTER
NS=$NS
GW=$GW
PATCH_URL=http://localhost:$ASSISTANT_PF_PORT
PATCH_TOKEN=$DEV_TOKEN
PROJECT=$PROJECT
GATEWAY_HOST_URL=http://localhost:$GW_PF_PORT
GATEWAY_URL_IN_CLUSTER=$GATEWAY_URL
GATEWAY_MCP_URL_IN_CLUSTER=$GATEWAY_MCP_URL
ENVOY_SVC=$ENVOY_SVC
EOF

TIER="BASE"; [ "$WITH_CATALOG" = 1 ] && TIER="BASE + CATALOG overlay"
say "Playground is UP ($TIER tier)"
cat <<EOF

  Try it as a consumer (patch CLI, built from this repo):

    go build -o /tmp/patch ./cmd/patch
    PATCH_URL=http://localhost:$ASSISTANT_PF_PORT PATCH_TOKEN=$DEV_TOKEN \\
      /tmp/patch chat "Diagnose pipeline p-1 for StreamCo" --project $PROJECT

    PATCH_URL=http://localhost:$ASSISTANT_PF_PORT /tmp/patch card

  Watch gateway token + MCP access logs (proof P4):
    export KUBECONFIG=$KCFG
    kubectl -n $EG_NS logs -l gateway.envoyproxy.io/owning-gateway-name=$GW -f

  See usage CloudEvents captured by the sink (proof P6):
    kubectl -n $NS port-forward svc/sink 7811:7811 &   # then:
    curl -s http://localhost:7811/events | jq .        # (sink image has no wget)

  Inspect workloads:
    kubectl -n $NS get pods

  Tear down exactly our layer:  $SCRIPT_DIR/playground-down.sh
  (Dry-run first:               $SCRIPT_DIR/playground-down.sh --dry-run)
EOF
