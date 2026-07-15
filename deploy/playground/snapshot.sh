#!/usr/bin/env bash
# snapshot.sh — label-scoped inventory of the playground's footprint on the
# shared cluster. Unlike the ephemeral e2e (which proves a full byte-restore),
# the playground is PERSISTENT, so its teardown proof (P8) is narrower: the set
# playground-down.sh removes must equal exactly the resources carrying
# app.kubernetes.io/part-of=agent-framework-playground (plus the namespace).
#
#   snapshot.sh inventory   print every resource carrying our label (+ ns + GatewayClass)
#
# playground-down.sh --dry-run prints the same set; QA diffs the two.
set -euo pipefail

TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
KCFG="${KUBECONFIG:-$TEST_INFRA_DIR/kubeconfig}"
NS="${NS:-patch-playground}"
LABEL="app.kubernetes.io/part-of=agent-framework-playground"
kc() { kubectl --kubeconfig "$KCFG" "$@"; }

# Namespaced kinds we create (core + Gateway API + AI Gateway CRDs). Listed
# explicitly so the inventory is deterministic and independent of `kubectl api-
# resources` ordering.
NS_KINDS="deployment,service,configmap,secret,gateway.gateway.networking.k8s.io,\
httproute.gateway.networking.k8s.io,envoyproxy.gateway.envoyproxy.io,\
clienttrafficpolicy.gateway.envoyproxy.io,backend.gateway.envoyproxy.io,\
aigatewayroute.aigateway.envoyproxy.io,aiservicebackend.aigateway.envoyproxy.io,\
backendsecuritypolicy.aigateway.envoyproxy.io,mcproute.aigateway.envoyproxy.io"

inventory() {
  echo "## namespace"
  kc get ns "$NS" -o name 2>/dev/null || true
  echo "## cluster-scoped (by label)"
  kc get gatewayclass -l "$LABEL" -o name 2>/dev/null | sort || true
  echo "## namespaced in $NS (by label)"
  # Some CRD kinds may not exist if the AI Gateway isn't installed yet; tolerate.
  kc -n "$NS" get "$NS_KINDS" -l "$LABEL" -o name 2>/dev/null | sort || true
}

case "${1:-inventory}" in
  inventory) inventory ;;
  *) echo "usage: $0 inventory" >&2; exit 2 ;;
esac
