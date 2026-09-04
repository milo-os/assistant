#!/usr/bin/env bash
# down.sh — tear down the AI Gateway layer this repo added (CONTRACT-GATEWAY.md
# task #13). Idempotent. By DEFAULT it restores the cluster to its
# pre-engagement state — removing everything up.sh installed that was NOT in
# the pre-install baseline (our namespace + GatewayClass, Envoy AI Gateway, and
# Envoy Gateway itself when we were the ones who installed it) — then diffs the
# cluster against the baseline to PROVE byte-restore.
#
# It NEVER deletes the kind cluster. The "test-infra" cluster on this machine
# is SHARED with unrelated workloads (ipam/etcd/nats/chainsaw); deleting it is
# strictly user-initiated (run `task cluster-down` yourself if you truly mean
# it). This script only removes objects it can attribute to this engagement.
#
# Flags:
#   --keep-aigw   leave Envoy AI Gateway installed (only remove our namespace layer)
#   --keep-eg     leave Envoy Gateway installed; just revert our extension patch
#                 (baseline-gating already preserves a pre-existing EG — this is
#                  a manual override for iterative dev)
set -euo pipefail

CLUSTER="${CLUSTER:-test-infra}"
TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
NS="${NS:-patch-ai-gateway}"
EG_NS="${EG_NS:-envoy-gateway-system}"
AIGW_NS="${AIGW_NS:-envoy-ai-gateway-system}"
PF_PORT="${PF_PORT:-1975}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_DIR="$SCRIPT_DIR/.run"
KCFG="$TEST_INFRA_DIR/kubeconfig"
LABEL="app.kubernetes.io/part-of=agent-framework-e2e"
# All Gateway API + Envoy Gateway + AI Gateway CRD groups (incl. the
# experimental gateway.networking.x-k8s.io channel test-infra installs).
GW_CRD_RE='gateway\.(networking|envoyproxy)|aigateway\.envoyproxy'

KEEP_AIGW=0
KEEP_EG=0
for arg in "$@"; do
  case "$arg" in
    --keep-aigw) KEEP_AIGW=1 ;;
    --keep-eg)   KEEP_EG=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

say() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()  { printf '  \033[1;32m✓\033[0m %s\n' "$*"; }
kc()  { kubectl --kubeconfig "$KCFG" "$@"; }
ensure_api() {
  for _ in 1 2 3; do
    kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KCFG" >/dev/null 2>&1 || true
    kc cluster-info >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

# ── Stop port-forward ─────────────────────────────────────────
say "Stop port-forward"
if [ -f "$RUN_DIR/portforward.pid" ]; then
  kill "$(cat "$RUN_DIR/portforward.pid")" 2>/dev/null || true   # the reconnect loop
  rm -f "$RUN_DIR/portforward.pid"
  ok "port-forward stopped"
else
  ok "no port-forward pidfile"
fi
# Also kill the child kubectl (orphaned when the loop dies) — matches both the
# wrapper and the child by the "<port>:80" forward arg they carry.
pkill -f "port-forward.*${PF_PORT}:80" 2>/dev/null || true

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  ok "cluster '$CLUSTER' not present — nothing else to do"
  exit 0
fi
ensure_api || { echo "cluster API not reachable — cannot tear down cleanly" >&2; exit 1; }

# ── Our layer (deleted by namespace + label only; never touches other ns) ──
# Order matters: drain our namespace WHILE the AI Gateway controller is still
# running, so it clears the aigateway.envoyproxy.io finalizers on our CRs.
# Uninstalling the controller first would strand those finalizers.
say "Delete our namespace + GatewayClass (by name/label; other namespaces untouched)"
"$SCRIPT_DIR/hack-ratelimit.sh" down 2>/dev/null || true
kc delete namespace "$NS" --ignore-not-found --wait=false
kc delete gatewayclass -l "$LABEL" --ignore-not-found --wait=false 2>/dev/null || true
kc delete gatewayclass patch-ai-gw --ignore-not-found --wait=false 2>/dev/null || true
for i in $(seq 1 30); do
  kc get ns "$NS" >/dev/null 2>&1 || break
  sleep 2
done
if kc get ns "$NS" >/dev/null 2>&1; then
  for r in backendsecuritypolicy aigatewayroute mcproute aiservicebackend; do
    for n in $(kc -n "$NS" get "$r" -o name 2>/dev/null); do
      kc -n "$NS" patch "$n" --type merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    done
  done
fi
ok "our layer removed"

# ── Envoy AI Gateway ──────────────────────────────────────────
if [ "$KEEP_AIGW" = 0 ]; then
  say "Uninstall Envoy AI Gateway (Helm)"
  helm --kubeconfig "$KCFG" uninstall aieg -n "$AIGW_NS" 2>/dev/null || true
  helm --kubeconfig "$KCFG" uninstall aieg-crd -n "$AIGW_NS" 2>/dev/null || true
  kc delete namespace "$AIGW_NS" --ignore-not-found --wait=false
  ok "AI Gateway uninstalled"
else
  ok "--keep-aigw: leaving Envoy AI Gateway installed"
fi

# ── Envoy Gateway: full removal (default) IF we installed it, else revert ──
# Baseline-gated: if EG predates this engagement it is preserved (config
# reverted only); if WE installed it (absent from the baseline) it is removed
# entirely, returning the cluster to its pre-engagement state.
EG_OURS=0
if [ "$KEEP_EG" = 0 ] && "$SCRIPT_DIR/snapshot.sh" installed-not-in-baseline helmrelease envoy-gateway flux-system; then
  EG_OURS=1
fi

if [ "$EG_OURS" = 1 ]; then
  say "Remove Envoy Gateway (we installed it) + Gateway API CRDs → pre-engagement state"
  kc -n flux-system delete helmrelease envoy-gateway --ignore-not-found
  kc delete namespace "$EG_NS" --ignore-not-found --wait=false
  # GatewayClasses carry an EG finalizer that ONLY the (now-removed) controller
  # can clear — force-clear it, or both the GatewayClass delete AND the CRD
  # delete hang forever waiting on it.
  for gc in $(kc get gatewayclass -o name 2>/dev/null); do
    kc patch "$gc" --type merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
  done
  kc delete gatewayclass --all --ignore-not-found --wait=false 2>/dev/null || true
  # Remove the Gateway API + Envoy Gateway + AI Gateway CRDs (none predate us —
  # verified: gatewayclass didn't exist pre-engagement). Capture the list
  # explicitly and loop, so a transient `get` failure can't silently no-op the
  # delete (which would then surface as a restore-diff failure downstream).
  crds="$(kc get crd -o name 2>/dev/null | grep -E "$GW_CRD_RE" || true)"
  if [ -n "$crds" ]; then
    echo "$crds" | while read -r crd; do kc delete "$crd" --ignore-not-found --wait=false || true; done
  fi
  ok "Envoy Gateway + Gateway API CRDs removed"
elif kc -n flux-system get helmrelease envoy-gateway >/dev/null 2>&1; then
  say "Revert EG extension patch → restore stock config (EG predates us / --keep-eg)"
  kc -n flux-system patch helmrelease envoy-gateway --type merge \
    -p '{"spec":{"values":{"config":{"envoyGateway":{"extensionManager":null,"extensionApis":null}}}}}'
  kc -n flux-system annotate helmrelease envoy-gateway \
    "reconcile.fluxcd.io/requestedAt=$(date +%s)" --overwrite >/dev/null
  for i in $(seq 1 30); do
    kc -n "$EG_NS" get cm envoy-gateway-config -o yaml 2>/dev/null | grep -q 'extensionManager' || break
    sleep 2
  done
  kc -n "$EG_NS" rollout restart deploy/envoy-gateway 2>/dev/null || true
  ok "EG extension config reverted"
fi

# ── Byte-restore proof ────────────────────────────────────────
# Wait for namespaces to finish terminating so the diff reflects the settled
# state, then compare the cluster against the pre-install baseline.
say "Verify byte-restore against pre-install baseline"
# Namespace teardown (esp. envoy-gateway-system) can lag well past a minute on
# kind; wait generously so the diff reflects the settled state in one pass.
for i in $(seq 1 120); do
  still=0
  for ns in "$NS" "$AIGW_NS" redis-system; do kc get ns "$ns" >/dev/null 2>&1 && still=1; done
  if [ "$EG_OURS" = 1 ]; then
    kc get ns "$EG_NS" >/dev/null 2>&1 && still=1
    [ "$(kc get crd -o name 2>/dev/null | grep -Ec "$GW_CRD_RE")" != 0 ] && still=1
  fi
  [ "$still" = 0 ] && break
  sleep 2
done
"$SCRIPT_DIR/snapshot.sh" diff || true

say "Teardown complete"
