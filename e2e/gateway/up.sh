#!/usr/bin/env bash
# up.sh — bring up the Envoy AI Gateway environment in front of the assistant
# stack (CONTRACT-GATEWAY.md, task #13). Idempotent: safe to re-run.
#
# What it does, in order:
#   1. Ensure the test-infra kind cluster exists + Envoy Gateway is installed,
#      using test-infra's OWN Taskfile (task cluster-up / install-envoy-gateway-operator).
#   2. Install Envoy AI Gateway v1.0.0 (Helm) — the version that pairs with the
#      Envoy Gateway v1.8.1 test-infra pins (compatibility verified; see README).
#   3. Apply the ONE required test-infra override: patch the EG HelmRelease
#      values to enable the AI Gateway extension server, then restart EG.
#   4. Build + kind-load the in-cluster backends image (stub LLM + StreamCo).
#   5. Apply our gateway manifests (dedicated Gateway w/ JSON access logs,
#      AIGatewayRoute + llmRequestCosts, BackendSecurityPolicy, MCPRoute).
#   6. Expose the gateway to the host via `kubectl port-forward`.
#
# test-infra is USED, never modified: only its gitignored ./kubeconfig is
# (re)written. Nothing here runs `task cluster-down` (the cluster on this
# machine is shared — see README "Environment note"). down.sh tears our layer
# back down.
#
# Flags:
#   --with-ratelimit   also enable the EG rate-limit (Redis) addon + apply the
#                      STRETCH token-budget BackendTrafficPolicy.
#   --skip-build       reuse the already-loaded backends image.
set -euo pipefail

# ── Config (override via env) ─────────────────────────────────
CLUSTER="${CLUSTER:-test-infra}"
TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
AIGW_VERSION="${AIGW_VERSION:-v1.0.0}"
NS="${NS:-patch-ai-gateway}"
GW="${GW:-patch-ai-gateway}"
EG_NS="${EG_NS:-envoy-gateway-system}"
AIGW_NS="${AIGW_NS:-envoy-ai-gateway-system}"
IMAGE="${IMAGE:-patch-e2e-backends:local}"
PF_PORT="${PF_PORT:-1975}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS="$SCRIPT_DIR/manifests"
RUN_DIR="$SCRIPT_DIR/.run"
KCFG="$TEST_INFRA_DIR/kubeconfig"

WITH_RATELIMIT=0
SKIP_BUILD=0
for arg in "$@"; do
  case "$arg" in
    --with-ratelimit) WITH_RATELIMIT=1 ;;
    --skip-build)     SKIP_BUILD=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

mkdir -p "$RUN_DIR"
say()  { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()   { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[1;33m!\033[0m %s\n' "$*"; }
kc()   { kubectl --kubeconfig "$KCFG" "$@"; }

# Re-export the kubeconfig and verify the API is actually reachable. kind
# reassigns the API host port when docker/colima restarts, which strands a
# stale address in the kubeconfig and makes helm/kubectl time out. Call this
# before any API-heavy step (surfaced by QA: a re-up once hit a helm timeout
# against a stale API address).
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
for t in docker kind kubectl helm task kustomize; do
  command -v "$t" >/dev/null 2>&1 || { echo "missing required tool: $t" >&2; exit 1; }
done
ok "tools present"

# ── 1. Cluster + Envoy Gateway via test-infra's Taskfile ──────
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  say "kind cluster '$CLUSTER' absent — bringing it up via test-infra (task cluster-up)"
  ( cd "$TEST_INFRA_DIR" && CLUSTER_NAME="$CLUSTER" task cluster-up )
else
  ok "kind cluster '$CLUSTER' already exists (reusing)"
fi

say "Refresh test-infra kubeconfig + verify API (gitignored artifact)"
ensure_api && ok "cluster reachable via $KCFG"

# Resource preflight (guardrail): don't add load to the shared cluster if the
# node's memory requests are already pressured. Our workloads are tiny, but
# must never risk evicting the user's unrelated work.
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

# Baseline snapshot for down.sh's byte-restore proof (captured once, before
# we install anything — reflects the cluster's pre-engagement state).
say "Snapshot pre-install baseline"
"$SCRIPT_DIR/snapshot.sh" baseline

say "Ensure Envoy Gateway (test-infra: task install-envoy-gateway-operator)"
( cd "$TEST_INFRA_DIR" && task install-envoy-gateway-operator )
kc -n "$EG_NS" rollout status deploy/envoy-gateway --timeout=180s
ok "Envoy Gateway ready"

# ── 2. Envoy AI Gateway (Helm, version-pinned) ────────────────
say "Install Envoy AI Gateway $AIGW_VERSION (Helm)"
ensure_api   # re-verify the API address before helm (kind port can shift mid-run)
helm --kubeconfig "$KCFG" upgrade -i aieg-crd \
  oci://docker.io/envoyproxy/ai-gateway-crds-helm --version "$AIGW_VERSION" \
  --namespace "$AIGW_NS" --create-namespace --wait
helm --kubeconfig "$KCFG" upgrade -i aieg \
  oci://docker.io/envoyproxy/ai-gateway-helm --version "$AIGW_VERSION" \
  --namespace "$AIGW_NS" --create-namespace --wait
# Controller deployment name is ai-gateway-controller; wait generically to be safe.
kc -n "$AIGW_NS" wait --for=condition=Available deploy --all --timeout=180s
ok "AI Gateway controller ready"

# Resolve the extension-server service FQDN (the EG patch points EG at it).
EXT_SVC="$(kc -n "$AIGW_NS" get svc -o jsonpath='{range .items[?(@.spec.ports[0].port==1063)]}{.metadata.name}{end}' 2>/dev/null || true)"
[ -z "$EXT_SVC" ] && EXT_SVC="ai-gateway-controller"
EXT_FQDN="${EXT_SVC}.${AIGW_NS}.svc.cluster.local"
ok "extension server: ${EXT_FQDN}:1063"

# ── 3. Configure Envoy Gateway for AI Gateway (documented override) ──
say "Patch Envoy Gateway HelmRelease → enable AI Gateway extension"
# Rewrite the patch file's hostname to the resolved service FQDN.
PATCH_FILE="$RUN_DIR/eg-extension.patch.yaml"
sed "s#ai-gateway-controller.${AIGW_NS}.svc.cluster.local#${EXT_FQDN}#" \
  "$SCRIPT_DIR/eg-helmrelease-extension.patch.yaml" > "$PATCH_FILE"
kc -n flux-system patch helmrelease envoy-gateway --type merge --patch-file "$PATCH_FILE"
# Nudge Flux to reconcile the HelmRelease now.
kc -n flux-system annotate helmrelease envoy-gateway \
  "reconcile.fluxcd.io/requestedAt=$(date +%s)" --overwrite >/dev/null

say "Wait for EG config to carry the extension, then restart EG"
for i in $(seq 1 60); do
  if kc -n "$EG_NS" get cm envoy-gateway-config -o yaml 2>/dev/null | grep -q 'extensionManager'; then
    ok "envoy-gateway-config now includes extensionManager"; break
  fi
  [ "$i" = 60 ] && { echo "timed out waiting for EG config" >&2; exit 1; }
  sleep 2
done
kc -n "$EG_NS" rollout restart deploy/envoy-gateway
kc -n "$EG_NS" rollout status deploy/envoy-gateway --timeout=180s
ok "Envoy Gateway restarted with AI Gateway extension enabled"

# ── 4. Backends image (stub LLM + StreamCo), loaded into kind ──
if [ "$SKIP_BUILD" = 0 ]; then
  say "Build + load backends image ($IMAGE)"
  docker build --provenance=false --sbom=false -t "$IMAGE" \
    -f "$SCRIPT_DIR/streamco/Dockerfile" "$SCRIPT_DIR/../streamco"
  kind load docker-image "$IMAGE" --name "$CLUSTER"
  ok "image loaded into kind"
else
  warn "--skip-build: reusing existing $IMAGE"
fi

# ── 5. Apply gateway manifests ────────────────────────────────
say "Apply gateway manifests → namespace $NS"
kc apply -f "$MANIFESTS/00-namespace.yaml"
# stub-llm source ConfigMap generated from the single .mjs (one source of truth).
kc -n "$NS" create configmap stub-llm-src \
  --from-file=stub-llm.mjs="$SCRIPT_DIR/../../config/components/llm-stub/stub-llm.mjs" \
  --dry-run=client -o yaml | kc apply -f -
kc -n "$NS" label configmap stub-llm-src app.kubernetes.io/part-of=agent-framework-e2e --overwrite >/dev/null
# Apply 10–60 via kustomize so every resource carries the attribution label
# (manifests/kustomization.yaml; selectors untouched). 70 is stretch-only.
kustomize build "$MANIFESTS" | kc apply -f -
ok "manifests applied (labeled app.kubernetes.io/part-of=agent-framework-e2e)"

say "Wait for workloads + gateway to be ready"
kc -n "$NS" rollout status deploy/stub-llm --timeout=120s
kc -n "$NS" rollout status deploy/streamco --timeout=120s
kc -n "$NS" wait --for=condition=Programmed gateway/"$GW" --timeout=300s
ok "stub-llm + streamco + gateway ready"
# Best-effort: surface route acceptance (non-fatal if the printer differs).
kc -n "$NS" get aigatewayroute,mcproute 2>/dev/null || true

if [ "$WITH_RATELIMIT" = 1 ]; then
  say "STRETCH: enable EG rate-limit addon + apply token-budget policy"
  "$SCRIPT_DIR/hack-ratelimit.sh" up || warn "rate-limit addon setup reported an issue (stretch)"
  kc apply -f "$MANIFESTS/70-backendtrafficpolicy.yaml" || warn "BackendTrafficPolicy apply failed (stretch)"
fi

# ── 6. Expose to host via port-forward ────────────────────────
say "Port-forward gateway → host :$PF_PORT"
# Stop any prior port-forward we started.
if [ -f "$RUN_DIR/portforward.pid" ]; then
  kill "$(cat "$RUN_DIR/portforward.pid")" 2>/dev/null || true
  rm -f "$RUN_DIR/portforward.pid"
fi
ENVOY_SVC="$(kc -n "$EG_NS" get svc \
  -l gateway.envoyproxy.io/owning-gateway-name="$GW",gateway.envoyproxy.io/owning-gateway-namespace="$NS" \
  -o jsonpath='{.items[0].metadata.name}')"
[ -z "$ENVOY_SVC" ] && { echo "could not find Envoy service for gateway $GW" >&2; exit 1; }
# Let the gateway's managed Envoy deployment settle first, so the forward
# doesn't attach to a pod that's about to be rolled as the config finalizes.
ENVOY_DEPLOY="$(kc -n "$EG_NS" get deploy \
  -l gateway.envoyproxy.io/owning-gateway-name="$GW" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[ -n "$ENVOY_DEPLOY" ] && kc -n "$EG_NS" rollout status "deploy/$ENVOY_DEPLOY" --timeout=120s || true
# Self-healing port-forward: a reconnect loop so a later Envoy pod roll doesn't
# leave a dead forward. down.sh kills the loop (pidfile) and its child.
nohup bash -c "while true; do kubectl --kubeconfig '$KCFG' -n '$EG_NS' port-forward 'svc/$ENVOY_SVC' '$PF_PORT:80' --address 127.0.0.1; echo '[port-forward] reconnecting in 2s'; sleep 2; done" \
  >"$RUN_DIR/portforward.log" 2>&1 &
echo $! > "$RUN_DIR/portforward.pid"
disown || true
# Wait for the forward to accept connections (Envoy 404 on / == up).
for i in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PF_PORT/" || true)"
  [ -n "$code" ] && [ "$code" != "000" ] && { ok "port-forward live (svc/$ENVOY_SVC, HTTP $code on /)"; break; }
  [ "$i" = 30 ] && { echo "port-forward not reachable" >&2; cat "$RUN_DIR/portforward.log" >&2; exit 1; }
  sleep 1
done

# Record connection details for down.sh + humans.
cat > "$RUN_DIR/env" <<EOF
KUBECONFIG=$KCFG
CLUSTER=$CLUSTER
NS=$NS
PF_PORT=$PF_PORT
GATEWAY_URL=http://localhost:$PF_PORT/v1
GATEWAY_MODEL=patch-stub-v1
MCP_URL=http://localhost:$PF_PORT/mcp
EOF

say "AI Gateway environment is UP"
cat <<EOF

  Assistant service (gateway mode) env:
    MODEL_MODE=gateway
    GATEWAY_URL=http://localhost:$PF_PORT/v1     # openai-compatible appends /chat/completions
    GATEWAY_MODEL=patch-stub-v1
    (NO model API key — the gateway injects it)

  MCP endpoint for AgentBinding fixtures:
    http://localhost:$PF_PORT/mcp

  Inspect:
    export KUBECONFIG=$KCFG
    kubectl -n $EG_NS logs -l gateway.envoyproxy.io/owning-gateway-name=$GW -f   # JSON access logs
    kubectl -n $NS get pods

  Tear down our layer:  $SCRIPT_DIR/down.sh
EOF
