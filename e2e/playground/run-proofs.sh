#!/usr/bin/env bash
# run-proofs.sh — QA proof driver for the real-environment playground (slice 6,
# CONTRACT-REAL-ENV.md). Drives proofs P1–P8 against the LIVE persistent
# playground on the shared test-infra kind cluster. Mirrors the proven
# e2e/run-e2e.sh gateway leg (build_go, access-log capture, record/finish).
#
# GUARDRAILS (non-negotiable, from the contract):
#   * NEVER runs `task cluster-down` and NEVER tears the playground down. The
#     env is persistent and the cluster hosts the user's unrelated workloads.
#   * P8 runs `playground-down.sh --dry-run` ONLY — it asserts the dry-run set
#     equals our labeled-resource set; it does not delete anything.
#   * test-infra repo is READ-ONLY. macOS has no `timeout` (we poll-loop).
#
# The playground bring-up scripts (playground-up.sh / playground-down.sh) are
# owned by pg-infra; this driver CALLS them and asserts on their effects. Every
# path/flag/URL below is env-overridable so drift in the builders' interfaces is
# a one-line fix, not a rewrite.
#
# Subcommands:
#   p1  bring-up idempotent + preflight-gated
#   p2  host patch CLI chat through the gateway (writes contextId for p4/p6)
#   p3  OVERLAY: live reconfiguration (v1 → v2 narrower → unpublish)
#   p4  gateway token attribution in the access log for our chat
#   p5  OVERLAY: entitlement isolation (unentitled project → no capabilities)
#   p6  sink shows service-emitted usage for the playground chat
#   p7  suites green — p7-assistant (go test) and/or p7-catalog (envtest)
#   p8  playground-down --dry-run == our labeled resources (NO teardown)
#   base     Phase 2 set: p1 p2 p4 p6 p7-assistant p8
#   overlay  Phase 2 set: p3 p5 p7-catalog   (requires --with-catalog up)
#   all      base then overlay
set -uo pipefail   # NOTE: not -e — proofs are non-fatal; we collect ALL results.

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${OUT_DIR:-${PG_DIR}/out}"
mkdir -p "${OUT}"

# ── Repos / cluster ─────────────────────────────────────────────────────────
ASSISTANT_REPO="${ASSISTANT_REPO:-/Users/scotwells/repos/milo-os/assistant}"
CATALOG_REPO="${CATALOG_REPO:-/Users/scotwells/repos/milo-os/service-catalog}"
TEST_INFRA="${TEST_INFRA:-/Users/scotwells/repos/datum-cloud/test-infra}"
KCFG="${KUBECONFIG:-${TEST_INFRA}/kubeconfig}"
GO_BIN_DIR="${GO_BIN_DIR:-${OUT}/bin}"
PATCH_CMD="${PATCH_CMD:-${GO_BIN_DIR}/patch}"

# ── Playground scripts (pg-infra owns these; confirm exact paths/flags) ─────
# Default location assumption: assistant repo deploy/ (contract §Team). Override
# with PLAYGROUND_UP_CMD / PLAYGROUND_DOWN_CMD once pg-infra confirms.
PLAYGROUND_UP_CMD="${PLAYGROUND_UP_CMD:-${ASSISTANT_REPO}/deploy/playground-up.sh}"
PLAYGROUND_DOWN_CMD="${PLAYGROUND_DOWN_CMD:-${ASSISTANT_REPO}/deploy/playground-down.sh}"
WITH_CATALOG_FLAG="${WITH_CATALOG_FLAG:---with-catalog}"
DRYRUN_FLAG="${DRYRUN_FLAG:---dry-run}"

# ── Runtime env (playground-up.sh writes .run/env; we source it if present) ──
PG_RUN_ENV="${PG_RUN_ENV:-${PG_DIR}/.run/env}"
[ -f "${PG_RUN_ENV}" ] && { set -a; . "${PG_RUN_ENV}"; set +a; }

PF_PORT="${PF_PORT:-1975}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:${PF_PORT}/v1}"
PROJECT="${PROJECT:-demo-project}"
UNENTITLED_PROJECT="${UNENTITLED_PROJECT:-unentitled-project}"
# Host workloads: the assistant service (gateway mode) + sink. If pg-infra runs
# the assistant IN-CLUSTER (contract component 1), set ASSISTANT_URL to its
# NodePort/forward and PATCH_URL to it, and SKIP the host boot (HOST_ASSISTANT=0).
HOST_ASSISTANT="${HOST_ASSISTANT:-1}"
ASSISTANT_HOST="${ASSISTANT_HOST:-127.0.0.1}"; ASSISTANT_PORT="${ASSISTANT_PORT:-7820}"
ASSISTANT_URL="${ASSISTANT_URL:-http://${ASSISTANT_HOST}:${ASSISTANT_PORT}}"
PATCH_URL="${PATCH_URL:-${ASSISTANT_URL}}"
PATCH_TOKEN="${PATCH_TOKEN:-e2e-token}"
SINK_HOST="${SINK_HOST:-127.0.0.1}"; SINK_PORT="${SINK_PORT:-7811}"
SINK_URL="${SINK_URL:-http://${SINK_HOST}:${SINK_PORT}}"
CAPABILITY_PROVIDER_URL="${CAPABILITY_PROVIDER_URL:-http://127.0.0.1:8085}"
GW="${GW:-patch-ai-gateway}"          # owning-gateway-name label value
EG_NS="${EG_NS:-envoy-gateway-system}"
CHECKS="${PG_DIR}/playground-checks.mjs"
SNAP="${PG_DIR}/snapshot.sh"

log()  { printf '\n==> %s\n' "$*"; }
note() { printf 'NOTE: %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
kc()   { kubectl --kubeconfig "${KCFG}" "$@"; }
have() { command -v "$1" >/dev/null 2>&1; }

# One-time: build the patch CLI the host consumer uses (P2/P3/P5).
build_patch() {
  [ -x "${PATCH_CMD}" ] && return 0
  have go || { warn "go not found — cannot build patch CLI"; return 1; }
  [ -f "${ASSISTANT_REPO}/go.mod" ] || { warn "no go.mod in ${ASSISTANT_REPO}"; return 1; }
  mkdir -p "${GO_BIN_DIR}"
  log "Building patch CLI → ${PATCH_CMD}"
  ( cd "${ASSISTANT_REPO}" && go build -o "${GO_BIN_DIR}/patch" ./cmd/patch ) 2>&1 | tee "${OUT}/go-build.log"
}

# Capture the gateway access log for a conversation into out/playground-access.log.
# Recipe proven in the gateway leg: resolve the RUNNING data-plane Envoy pod by
# owning-gateway-name, `kubectl logs <pod> --all-containers --tail=-1`, then wait
# (poll, no `timeout`) for the conversation's llm lines to flush (~up to 45s).
capture_access_log() {
  local ctxid="$1" want="${2:-2}"
  local pod; pod=$(kc -n "${EG_NS}" get pods \
      -l gateway.envoyproxy.io/owning-gateway-name="${GW}" \
      --field-selector=status.phase=Running \
      -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null) || pod=""
  local n=0
  for _ in $(seq 1 15); do
    kc -n "${EG_NS}" logs "${pod}" --all-containers --tail=-1 > "${OUT}/playground-access.log" 2>/dev/null || true
    n=$(grep '"log.type":"llm"' "${OUT}/playground-access.log" 2>/dev/null | grep -c "${ctxid}" 2>/dev/null) || n=0
    [ "${n:-0}" -ge "${want}" ] && break
    sleep 3
  done
  echo "access-log: pod=${pod} llm-lines-for-conversation=${n:-0}"
}

# ── P1: idempotent + preflight-gated bring-up ───────────────────────────────
p1() {
  log "P1: idempotent + preflight-gated bring-up"
  [ -x "${PLAYGROUND_UP_CMD%% *}" ] || { note "playground-up.sh not found at '${PLAYGROUND_UP_CMD}' — NOT PROVEN (pg-infra pending)"; return 1; }
  local up1="${OUT}/p1-up-run1.log" up2="${OUT}/p1-up-run2.log"
  bash -c "${PLAYGROUND_UP_CMD}" 2>&1 | tee "${up1}"; local rc1=${PIPESTATUS[0]}
  bash -c "${PLAYGROUND_UP_CMD}" 2>&1 | tee "${up2}"; local rc2=${PIPESTATUS[0]}
  # Preflight gate: the memory-% preflight line must be present (proves the
  # >80% abort guardrail ran). Idempotency: run 2 exits 0 and reports UP.
  local preflight=1; grep -qiE 'preflight|memory.*%' "${up1}" || preflight=0
  local up_ok=1; { [ "${rc2}" -eq 0 ] && grep -qiE 'is UP|environment is up|ready' "${up2}"; } || up_ok=0
  echo "P1: run1 rc=${rc1} run2 rc=${rc2} preflight=${preflight} idempotent-up=${up_ok}"
  [ "${rc1}" -eq 0 ] && [ "${up_ok}" -eq 1 ] && [ "${preflight}" -eq 1 ]
}

# ── P2: host patch CLI chat through the gateway ─────────────────────────────
p2() {
  log "P2: host patch CLI chat through the gateway"
  build_patch || true
  curl -fsS -X DELETE "${SINK_URL}/events" >/dev/null 2>&1 || true
  OUT_DIR="${OUT}" SINK_URL="${SINK_URL}" PATCH_URL="${PATCH_URL}" PATCH_TOKEN="${PATCH_TOKEN}" \
  PATCH_CMD="${PATCH_CMD}" PROJECT="${PROJECT}" \
    node "${CHECKS}" chat | tee "${OUT}/p2-chat-console.log"
  local rc=${PIPESTATUS[0]}
  # Flush the access log now so p4/p6 can read it for the same contextId.
  local ctxid; ctxid=$(cat "${OUT}/playground-contextid.txt" 2>/dev/null || echo "")
  [ -n "${ctxid}" ] && capture_access_log "${ctxid}" 2 | tee -a "${OUT}/p2-chat-console.log"
  return "${rc}"
}

# ── P3: OVERLAY live reconfiguration ────────────────────────────────────────
# Applies the catalog CRs through their stages; between stages the checks driver
# captures the capability-doc + chat-turn tool set. CR apply/unpublish commands
# come from catalog-engineer (env-overridable). Requires --with-catalog up.
p3() {
  log "P3: OVERLAY live reconfiguration (v1 → v2 narrower → unpublish)"
  local run="env OUT_DIR=${OUT} SINK_URL=${SINK_URL} PATCH_URL=${PATCH_URL} PATCH_TOKEN=${PATCH_TOKEN} PATCH_CMD=${PATCH_CMD} PROJECT=${PROJECT} CAPABILITY_PROVIDER_URL=${CAPABILITY_PROVIDER_URL}"
  # Stage v1 (default published config assumed already applied by --with-catalog up).
  [ -n "${SAC_APPLY_V1:-}" ] && bash -c "${SAC_APPLY_V1}" 2>&1 | tee "${OUT}/p3-apply-v1.log"
  sleep "${RECONCILE_WAIT:-8}"
  ${run} STAGE=v1 node "${CHECKS}" reconfig | tee "${OUT}/p3-v1.log"
  # Stage v2 (narrower toolSelector).
  [ -n "${SAC_APPLY_V2:-}" ] && bash -c "${SAC_APPLY_V2}" 2>&1 | tee "${OUT}/p3-apply-v2.log" \
    || note "SAC_APPLY_V2 unset — provide the kubectl apply for the v2 ServiceAgentConfiguration (catalog-engineer)"
  sleep "${RECONCILE_WAIT:-8}"
  ${run} STAGE=v2 node "${CHECKS}" reconfig | tee "${OUT}/p3-v2.log"
  # Stage unpublish.
  [ -n "${SAC_UNPUBLISH:-}" ] && bash -c "${SAC_UNPUBLISH}" 2>&1 | tee "${OUT}/p3-unpublish.log" \
    || note "SAC_UNPUBLISH unset — provide the unpublish command (delete/withdraw the ServiceAgentConfiguration)"
  sleep "${RECONCILE_WAIT:-8}"
  ${run} STAGE=unpublished node "${CHECKS}" reconfig | tee "${OUT}/p3-unpublished.log"
  # Compare all three stages → verdict.
  ${run} STAGE=compare node "${CHECKS}" reconfig | tee "${OUT}/p3-compare-console.log"
  return "${PIPESTATUS[0]}"
}

# ── P4: gateway token attribution ───────────────────────────────────────────
p4() {
  log "P4: gateway token attribution for our chat"
  local ctxid; ctxid=$(cat "${OUT}/playground-contextid.txt" 2>/dev/null || echo "")
  [ -z "${ctxid}" ] && { note "no contextId captured — run p2 first"; return 1; }
  [ -s "${OUT}/playground-access.log" ] || capture_access_log "${ctxid}" 2
  OUT_DIR="${OUT}" ACCESS_LOG_FILE="${OUT}/playground-access.log" SINK_URL="${SINK_URL}" \
  PROJECT="${PROJECT}" EXPECT_CONTEXT_ID="${ctxid}" \
    node "${CHECKS}" tokens | tee "${OUT}/p4-tokens-console.log"
  return "${PIPESTATUS[0]}"
}

# ── P5: OVERLAY entitlement isolation ───────────────────────────────────────
p5() {
  log "P5: OVERLAY entitlement isolation (unentitled project → no capabilities)"
  OUT_DIR="${OUT}" PATCH_URL="${PATCH_URL}" PATCH_TOKEN="${PATCH_TOKEN}" PATCH_CMD="${PATCH_CMD}" \
  PROJECT="${PROJECT}" UNENTITLED_PROJECT="${UNENTITLED_PROJECT}" \
  ${UNENTITLED_TOKEN:+UNENTITLED_TOKEN=${UNENTITLED_TOKEN}} \
  CAPABILITY_PROVIDER_URL="${CAPABILITY_PROVIDER_URL}" \
    node "${CHECKS}" entitlement | tee "${OUT}/p5-entitlement-console.log"
  return "${PIPESTATUS[0]}"
}

# ── P6: service-emitted usage at the sink ───────────────────────────────────
p6() {
  log "P6: sink shows service-emitted usage for the playground chat"
  local ctxid; ctxid=$(cat "${OUT}/playground-contextid.txt" 2>/dev/null || echo "")
  OUT_DIR="${OUT}" SINK_URL="${SINK_URL}" PROJECT="${PROJECT}" EXPECT_CONTEXT_ID="${ctxid}" \
    node "${CHECKS}" sink | tee "${OUT}/p6-sink-console.log"
  return "${PIPESTATUS[0]}"
}

# ── P7: suites green ────────────────────────────────────────────────────────
p7_assistant() {
  log "P7 (assistant): go vet ./... + go test ./..."
  have go || { note "go not found"; return 1; }
  [ -f "${ASSISTANT_REPO}/go.mod" ] || { note "no go.mod in ${ASSISTANT_REPO}"; return 1; }
  ( cd "${ASSISTANT_REPO}" && go vet ./... ) 2>&1 | tee "${OUT}/p7-assistant-govet.log"; local v=${PIPESTATUS[0]}
  ( cd "${ASSISTANT_REPO}" && go test ./... ) 2>&1 | tee "${OUT}/p7-assistant-gotest.log"; local t=${PIPESTATUS[0]}
  [ "${v}" -eq 0 ] && [ "${t}" -eq 0 ]
}
p7_catalog() {
  log "P7 (catalog): envtest suite on the playground branch"
  [ -d "${CATALOG_REPO}" ] || { note "catalog repo not found at ${CATALOG_REPO}"; return 1; }
  have go || { note "go not found"; return 1; }
  # service-catalog uses envtest via `make test` (confirm exact target with pg-catalog).
  local cmd="${CATALOG_TEST_CMD:-make test}"
  ( cd "${CATALOG_REPO}" && bash -c "${cmd}" ) 2>&1 | tee "${OUT}/p7-catalog-test.log"
  return "${PIPESTATUS[0]}"
}

# ── P8: playground-down --dry-run == our labeled resources (NO teardown) ─────
p8() {
  log "P8: playground-down --dry-run lists EXACTLY our labeled resources (NO teardown)"
  # Ground truth: everything carrying our attribution label.
  "${SNAP}" labeled | tee "${OUT}/p8-labeled-console.log"
  local truth="${OUT}/labeled-resources.txt"
  [ -x "${PLAYGROUND_DOWN_CMD%% *}" ] || { note "playground-down.sh not found at '${PLAYGROUND_DOWN_CMD}' — NOT PROVEN (pg-infra pending)"; return 1; }
  bash -c "${PLAYGROUND_DOWN_CMD} ${DRYRUN_FLAG}" 2>&1 | tee "${OUT}/p8-dryrun.log"; local rc=${PIPESTATUS[0]}
  # Extract kind/ns/name tuples the dry-run says it WOULD delete. The down script
  # should print them in a parseable form; we normalize to the snapshot's
  # `Kind/ns/name` shape. If it prints a different shape, DRYRUN_PARSE overrides.
  local parsed="${OUT}/p8-dryrun-parsed.txt"
  if [ -n "${DRYRUN_PARSE:-}" ]; then
    bash -c "${DRYRUN_PARSE} < '${OUT}/p8-dryrun.log'" > "${parsed}" 2>/dev/null || true
  else
    grep -oiE '[a-z][a-z0-9.-]+/[a-z0-9-]+/[a-z0-9.-]+' "${OUT}/p8-dryrun.log" 2>/dev/null | sort -u > "${parsed}" || true
  fi
  # Compare sets. Absolute equality is the bar; report both directions.
  local only_truth only_dry
  only_truth=$(comm -23 <(sort -u "${truth}" 2>/dev/null) <(sort -u "${parsed}" 2>/dev/null) | wc -l | tr -d ' ')
  only_dry=$(comm -13 <(sort -u "${truth}" 2>/dev/null) <(sort -u "${parsed}" 2>/dev/null) | wc -l | tr -d ' ')
  echo "P8: dry-run rc=${rc} labeled=$(wc -l <"${truth}"|tr -d ' ') dryrun=$(wc -l <"${parsed}"|tr -d ' ') only-in-labeled=${only_truth} only-in-dryrun=${only_dry}"
  diff -u <(sort -u "${truth}") <(sort -u "${parsed}") > "${OUT}/p8-set.diff" 2>/dev/null || true
  [ "${rc}" -eq 0 ] && [ "${only_truth}" -eq 0 ] && [ "${only_dry}" -eq 0 ]
}

# ── Phase groupings ─────────────────────────────────────────────────────────
declare -A VERDICT
mark() { VERDICT["$1"]=$([ "$2" -eq 0 ] && echo PROVEN || echo "NOT PROVEN"); }
runp() { "$1"; mark "$2" "$?"; }

base() {
  runp p1 P1; runp p2 P2; runp p4 P4; runp p6 P6
  p7_assistant; mark P7-assistant $?
  runp p8 P8
  summary
}
overlay() {
  runp p3 P3; runp p5 P5
  p7_catalog; mark P7-catalog $?
  summary
}
summary() {
  log "PROOF SUMMARY"
  for k in $(printf '%s\n' "${!VERDICT[@]}" | sort); do printf '  %-14s %s\n' "$k" "${VERDICT[$k]}"; done
}

main() {
  have node || { echo "node required" >&2; exit 2; }
  have kubectl || { echo "kubectl required" >&2; exit 2; }
  case "${1:-base}" in
    p1) p1 ;; p2) p2 ;; p3) p3 ;; p4) p4 ;; p5) p5 ;; p6) p6 ;;
    p7-assistant) p7_assistant ;; p7-catalog) p7_catalog ;; p8) p8 ;;
    base) base ;; overlay) overlay ;; all) base; overlay ;;
    labeled) "${SNAP}" labeled ;;
    *) echo "usage: $0 p1|p2|p3|p4|p5|p6|p7-assistant|p7-catalog|p8|base|overlay|all|labeled" >&2; exit 2 ;;
  esac
}
main "$@"
