#!/usr/bin/env bash
# snapshot.sh — cluster-state + labeled-resource capture for the PERSISTENT
# real-environment playground (slice 6, CONTRACT-REAL-ENV.md).
#
# Two jobs:
#   1. Guardrail baseline (like e2e/gateway/snapshot.sh): capture the cluster's
#      pre-install state so we can prove the playground never touched the
#      shared cluster's unrelated workloads (ipam/etcd/nats/chainsaw).
#   2. P8 evidence: enumerate EXACTLY the resources carrying our attribution
#      label (app.kubernetes.io/part-of=agent-framework-playground). P8 asserts
#      that `playground-down.sh --dry-run` lists this set and nothing else — so
#      this file is the ground-truth the dry-run is diffed against. We NEVER
#      tear the playground down here; the env is persistent.
#
# Subcommands:
#   baseline            capture the pre-install cluster baseline (won't overwrite)
#   current             capture current cluster state
#   diff                diff current vs baseline → out/cluster.diff (informational)
#   labeled             list every resource carrying our label, one `kind/ns/name`
#                       per line, sorted → out/labeled-resources.txt (P8 truth set)
#   in-baseline <ns|crd|helmrelease> <name> [ns]
#                       exit 0 if the object is ABSENT from the baseline (ours to
#                       remove), 1 if it predates us (must be preserved)
set -euo pipefail

TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
KCFG="${KUBECONFIG:-$TEST_INFRA_DIR/kubeconfig}"
LABEL="${PLAYGROUND_LABEL:-app.kubernetes.io/part-of=agent-framework-playground}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${OUT_DIR:-$SCRIPT_DIR/out}"
BASELINE="$OUT/cluster-snapshot.baseline.txt"
mkdir -p "$OUT"
kc() { kubectl --kubeconfig "$KCFG" "$@"; }

capture() {
  echo "## namespaces"
  kc get ns -o name | sort
  echo "## crds (gateway/aigateway/mcp/catalog)"
  kc get crd -o name 2>/dev/null \
    | grep -Ei 'gateway|aigateway|mcp|miloapis|servicecatalog|services\.milo' | sort || true
  echo "## flux helmreleases (ns/name)"
  kc get helmrelease -A --no-headers -o custom-columns=X:.metadata.namespace,Y:.metadata.name 2>/dev/null \
    | awk '{print $1"/"$2}' | sort || true
  echo "## helm releases (ns/name)"
  helm --kubeconfig "$KCFG" ls -A 2>/dev/null | awk 'NR>1{print $2"/"$1}' | sort || true
}

# Enumerate labeled resources by broad api-resources sweep — independent of the
# down-script's fixed kind list, so it catches a stray the script would miss.
#
# SCOPE (P8 is the BASE playground-down.sh proof): playground-down.sh removes the
# BASE footprint = the `patch-playground` namespace (cascading its contents) +
# the `patch-pg-gw` GatewayClass. So the BASE truth set is:
#   * namespaced labeled objects IN namespace $NS (default patch-playground),
#     EXCLUDING cascade-derived kinds (pods/replicasets/endpointslices/...) which
#     the down-script deliberately doesn't enumerate (namespace deletion removes
#     them), and
#   * the labeled GatewayClass (the one BASE cluster-scoped object).
# The CATALOG OVERLAY shares the same label but lives in a DIFFERENT namespace
# (agent-framework-playground) with its own cluster-scoped objects (webhooks,
# issuers) and is torn down separately (catalog-engineer) — so BASE P8 must NOT
# count it. Use `labeled-all` to see the full cross-namespace labeled footprint.
NS="${NS:-patch-playground}"
DERIVED_RE='^(pods|replicasets|endpointslices|controllerrevisions|events|endpoints|podmetrics)($|\.)'
labeled() { _labeled scoped; }
labeled_all() { _labeled all; }
_labeled() {
  local mode="$1" out="$OUT/labeled-resources.txt"
  [ "$mode" = all ] && out="$OUT/labeled-resources.all.txt"
  : > "$out.tmp"
  local nk; nk="$(kc api-resources --verbs=list --namespaced -o name 2>/dev/null | sort -u)"
  for r in $nk; do
    [ "$mode" = scoped ] && echo "$r" | grep -qE "$DERIVED_RE" && continue
    if [ "$mode" = scoped ]; then
      kc get "$r" -n "$NS" -l "$LABEL" \
        -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers 2>/dev/null \
        | awk 'NF{print $1"/"$2"/"$3}' >> "$out.tmp" || true
    else
      kc get "$r" -A -l "$LABEL" \
        -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers 2>/dev/null \
        | awk 'NF{print $1"/"$2"/"$3}' >> "$out.tmp" || true
    fi
  done
  # Cluster-scoped: BASE owns only the GatewayClass. In `all` mode, sweep every
  # cluster-scoped kind (surfaces overlay webhooks/issuers too).
  if [ "$mode" = scoped ]; then
    kc get gatewayclass -l "$LABEL" -o custom-columns=K:.kind,N:.metadata.name --no-headers 2>/dev/null \
      | awk 'NF{print $1"/<cluster>/"$2}' >> "$out.tmp" || true
  else
    local ck; ck="$(kc api-resources --verbs=list --namespaced=false -o name 2>/dev/null | sort -u)"
    for r in $ck; do
      kc get "$r" -l "$LABEL" -o custom-columns=K:.kind,N:.metadata.name --no-headers 2>/dev/null \
        | awk 'NF{print $1"/<cluster>/"$2}' >> "$out.tmp" || true
    done
  fi
  grep -v '^/' "$out.tmp" 2>/dev/null | sort -u > "$out" || true
  rm -f "$out.tmp"
  echo "labeled resources [$mode] ($LABEL, ns=$([ "$mode" = scoped ] && echo "$NS + GatewayClass" || echo "ALL")) → $out"
  wc -l < "$out" | awk '{print $1" object(s)"}'
}

case "${1:-}" in
  baseline)
    if [ -f "$BASELINE" ]; then echo "baseline already captured: $BASELINE"
    else capture > "$BASELINE"; echo "baseline captured → $BASELINE"; fi ;;
  current) capture > "$OUT/cluster-snapshot.current.txt"; echo "current → $OUT/cluster-snapshot.current.txt" ;;
  diff)
    [ -f "$BASELINE" ] || { echo "no baseline ($BASELINE)"; exit 2; }
    capture > "$OUT/cluster-snapshot.current.txt"
    if diff -u "$BASELINE" "$OUT/cluster-snapshot.current.txt" > "$OUT/cluster.diff"; then
      echo "SNAPSHOT MATCH — cluster matches pre-install baseline (out/cluster.diff empty)"
    else
      echo "SNAPSHOT DIFF (current vs baseline) — see out/cluster.diff:"; cat "$OUT/cluster.diff"
      echo "NOTE: '+' = objects present now, absent at baseline (playground/other new work); '-' = predated us and now gone (investigate)."
    fi ;;
  labeled) labeled ;;
  labeled-all) labeled_all ;;
  in-baseline)
    [ -f "$BASELINE" ] || exit 0
    case "$2" in
      ns)          grep -qE "namespace/$3\$" "$BASELINE" && exit 1 || exit 0 ;;
      crd)         grep -qE "customresourcedefinition.*$3" "$BASELINE" && exit 1 || exit 0 ;;
      helmrelease) grep -qE "${4:-}/$3\$" "$BASELINE" && exit 1 || exit 0 ;;
      *) echo "unknown kind $2" >&2; exit 2 ;;
    esac ;;
  *) echo "usage: $0 baseline|current|diff|labeled|in-baseline <kind> <name> [ns]" >&2; exit 2 ;;
esac
