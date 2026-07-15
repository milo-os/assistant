#!/usr/bin/env bash
# playground-down.sh — remove EXACTLY the playground's labeled footprint from the
# shared test-infra kind cluster (CONTRACT-REAL-ENV.md, proof P8). Idempotent.
#
# What it removes (everything carries app.kubernetes.io/part-of=agent-framework-
# playground): the `patch-playground` namespace (and all workloads, routes,
# secrets, configmaps within it) + the cluster-scoped `patch-pg-gw`
# GatewayClass. Nothing else.
#
# What it deliberately does NOT touch (shared infra the ephemeral e2e env and the
# user's workloads rely on; NONE of it carries our label):
#   * the kind cluster itself (NEVER `task cluster-down`),
#   * Envoy Gateway / Envoy AI Gateway operators + CRDs,
#   * the EG extension-server patch,
#   * any other namespace.
#
#   --dry-run   print exactly what would be deleted (the label-scoped inventory)
#               and exit without changing anything. This IS the P8 proof.
set -euo pipefail

CLUSTER="${CLUSTER:-test-infra}"
TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
NS="${NS:-patch-playground}"
GC="${GC:-patch-pg-gw}"
GW_PF_PORT="${GW_PF_PORT:-1985}"
ASSISTANT_PF_PORT="${ASSISTANT_PF_PORT:-1986}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN_DIR="$SCRIPT_DIR/.run"
KCFG="$TEST_INFRA_DIR/kubeconfig"
LABEL="app.kubernetes.io/part-of=agent-framework-playground"

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
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

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  ok "cluster '$CLUSTER' not present — nothing to do"
  exit 0
fi
ensure_api || { echo "cluster API not reachable" >&2; exit 1; }

# ── Dry-run: show exactly our labeled footprint, delete nothing ──
if [ "$DRY_RUN" = 1 ]; then
  say "DRY-RUN — the following (and ONLY the following) would be deleted:"
  "$SCRIPT_DIR/snapshot.sh" inventory
  echo
  ok "dry-run only — no changes made"
  echo "  (every line above carries $LABEL, or is the namespace/GatewayClass"
  echo "   that scopes them; shared EG/AI-Gateway infra is untouched.)"
  exit 0
fi

# ── Stop port-forwards ────────────────────────────────────────
say "Stop port-forwards"
for tag in gateway assistant; do
  pidfile="$RUN_DIR/pf-$tag.pid"
  [ -f "$pidfile" ] && { kill "$(cat "$pidfile")" 2>/dev/null || true; rm -f "$pidfile"; }
done
pkill -f "port-forward.*${GW_PF_PORT}:80" 2>/dev/null || true
pkill -f "port-forward.*${ASSISTANT_PF_PORT}:7820" 2>/dev/null || true
ok "port-forwards stopped"

# ── Delete our namespace WHILE the AI Gateway controller still runs ──
# so it clears the aigateway.envoyproxy.io finalizers on our CRs (deleting the
# controller first would strand them). We never uninstall that controller.
say "Delete namespace $NS + GatewayClass $GC (label-scoped; other namespaces untouched)"
kc delete namespace "$NS" --ignore-not-found --wait=false
kc delete gatewayclass "$GC" --ignore-not-found --wait=false 2>/dev/null || true

# If the namespace lingers on a CRD finalizer, clear ours explicitly.
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
# The GatewayClass carries an EG finalizer only its controller can clear; force
# it so neither the GatewayClass delete nor anything downstream hangs.
kc patch gatewayclass "$GC" --type merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
ok "delete issued"

# ── Verify our footprint is gone ──────────────────────────────
say "Verify label-scoped footprint is empty"
for i in $(seq 1 60); do
  kc get ns "$NS" >/dev/null 2>&1 || break
  sleep 2
done
remaining="$("$SCRIPT_DIR/snapshot.sh" inventory | grep -vE '^##' | grep -v '^$' || true)"
if [ -z "$remaining" ]; then
  ok "clean — no agent-framework-playground resources remain"
else
  echo "  ! still present (may be mid-termination — re-run to confirm):"
  echo "$remaining" | sed 's/^/    /'
fi

say "Teardown complete (shared EG / AI Gateway infra left intact)"
