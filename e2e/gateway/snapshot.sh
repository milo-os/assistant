#!/usr/bin/env bash
# snapshot.sh — cluster-state snapshot + byte-restore proof for the AI Gateway
# e2e (team-lead guardrail #2/#5). Because this env installs onto a SHARED
# cluster, we capture the pre-install baseline and, after teardown, diff the
# cluster against it to prove nothing of ours remains.
#
#   snapshot.sh baseline   capture the pre-install baseline (once; won't overwrite)
#   snapshot.sh current    capture current cluster state
#   snapshot.sh diff       diff current vs baseline → out/cluster-restore.diff (PASS/FAIL)
#   snapshot.sh installed-not-in-baseline <ns|crd|helmrelease> <name> [namespace]
#                          exit 0 if the object is absent from the baseline (i.e. WE
#                          installed it and may remove it), 1 if it predates us (keep)
set -euo pipefail

TEST_INFRA_DIR="${TEST_INFRA_DIR:-/Users/scotwells/repos/datum-cloud/test-infra}"
KCFG="$TEST_INFRA_DIR/kubeconfig"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$SCRIPT_DIR/out"
BASELINE="$OUT/cluster-snapshot.baseline.txt"
mkdir -p "$OUT"
kc() { kubectl --kubeconfig "$KCFG" "$@"; }

capture() {
  echo "## namespaces"
  kc get ns -o name | sort
  echo "## crds (gateway/aigateway/mcp)"
  kc get crd -o name 2>/dev/null | grep -Ei 'gateway|aigateway|mcp' | sort || true
  echo "## flux helmreleases (ns/name)"
  kc get helmrelease -A --no-headers -o custom-columns=X:.metadata.namespace,Y:.metadata.name 2>/dev/null \
    | awk '{print $1"/"$2}' | sort || true
  echo "## helm releases (ns/name)"
  helm --kubeconfig "$KCFG" ls -A 2>/dev/null | awk 'NR>1{print $2"/"$1}' | sort || true
}

case "${1:-}" in
  baseline)
    if [ -f "$BASELINE" ]; then
      echo "baseline already captured: $BASELINE"
    else
      capture > "$BASELINE"
      echo "baseline captured → $BASELINE"
    fi
    ;;
  current)
    capture > "$OUT/cluster-snapshot.current.txt"
    echo "current captured → $OUT/cluster-snapshot.current.txt"
    ;;
  diff)
    [ -f "$BASELINE" ] || { echo "no baseline to diff against ($BASELINE)"; exit 2; }
    capture > "$OUT/cluster-snapshot.current.txt"
    if diff -u "$BASELINE" "$OUT/cluster-snapshot.current.txt" > "$OUT/cluster-restore.diff"; then
      echo "RESTORE PASS — cluster matches pre-install baseline (see out/cluster-restore.diff, empty)"
    else
      echo "RESTORE DIFF (current vs baseline) — see out/cluster-restore.diff:"
      cat "$OUT/cluster-restore.diff"
      echo "NOTE: '+' lines are objects still present that were NOT in the baseline (ours to clean); '-' lines predate us."
    fi
    ;;
  installed-not-in-baseline)
    # $2=kind (ns|crd|helmrelease), $3=name, $4=namespace(optional, for helmrelease)
    [ -f "$BASELINE" ] || exit 0  # no baseline → treat as ours
    case "$2" in
      ns)          key="$3$" ; grep -qE "namespace/$3\$" "$BASELINE" && exit 1 || exit 0 ;;
      crd)         grep -qE "customresourcedefinition.*$3" "$BASELINE" && exit 1 || exit 0 ;;
      helmrelease) grep -qE "${4:-}/$3\$" "$BASELINE" && exit 1 || exit 0 ;;
      *) echo "unknown kind $2" >&2; exit 2 ;;
    esac
    ;;
  *)
    echo "usage: $0 baseline|current|diff|installed-not-in-baseline <kind> <name> [ns]" >&2
    exit 2 ;;
esac
