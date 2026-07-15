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
# Catalog OVERLAY lives in a WORKTREE on feat/agent-framework-playground.
CATALOG_REPO="${CATALOG_REPO:-/Users/scotwells/repos/milo-os/service-catalog-playground}"
CATALOG_NS="${CATALOG_NS:-agent-framework-playground}"   # adapter + CRs live here
TEST_INFRA="${TEST_INFRA:-/Users/scotwells/repos/datum-cloud/test-infra}"
KCFG="${KUBECONFIG:-${TEST_INFRA}/kubeconfig}"
GO_BIN_DIR="${GO_BIN_DIR:-${OUT}/bin}"
PATCH_CMD="${PATCH_CMD:-${GO_BIN_DIR}/patch}"

# ── Playground scripts (pg-infra: deploy/playground/, confirmed 7f87ace) ────
PLAYGROUND_UP_CMD="${PLAYGROUND_UP_CMD:-${ASSISTANT_REPO}/deploy/playground/playground-up.sh}"
PLAYGROUND_DOWN_CMD="${PLAYGROUND_DOWN_CMD:-${ASSISTANT_REPO}/deploy/playground/playground-down.sh}"
WITH_CATALOG_FLAG="${WITH_CATALOG_FLAG:---with-catalog}"
DRYRUN_FLAG="${DRYRUN_FLAG:---dry-run}"

# ── Runtime env — playground-up.sh writes deploy/playground/.run/env with the
# authoritative PATCH_URL/PATCH_TOKEN/GW/NS/PROJECT; source it if present. ─────
PG_RUN_ENV="${PG_RUN_ENV:-${ASSISTANT_REPO}/deploy/playground/.run/env}"
[ -f "${PG_RUN_ENV}" ] && { set -a; . "${PG_RUN_ENV}"; set +a; }

# Assistant + sink run IN-CLUSTER (manifests 70/80). up.sh port-forwards the
# assistant to :1986; the SINK is a ClusterIP Service NOT forwarded by up.sh, so
# this driver forwards svc/sink itself (sink_port_forward). No host assistant.
PROJECT="${PROJECT:-demo-project}"
UNENTITLED_PROJECT="${UNENTITLED_PROJECT:-unentitled-project}"
ASSISTANT_PF_PORT="${ASSISTANT_PF_PORT:-1986}"
PATCH_URL="${PATCH_URL:-http://localhost:${ASSISTANT_PF_PORT}}"
PATCH_TOKEN="${PATCH_TOKEN:-pg-demo-token}"
SINK_HOST="${SINK_HOST:-127.0.0.1}"; SINK_PORT="${SINK_PORT:-7811}"
SINK_URL="${SINK_URL:-http://${SINK_HOST}:${SINK_PORT}}"
CAPABILITY_PROVIDER_URL="${CAPABILITY_PROVIDER_URL:-http://127.0.0.1:8085}"
GW="${GW:-patch-playground}"          # owning-gateway-name label value (README P4)
NS="${NS:-patch-playground}"
EG_NS="${EG_NS:-envoy-gateway-system}"
CHECKS="${PG_DIR}/playground-checks.mjs"
SNAP="${PG_DIR}/snapshot.sh"

log()  { printf '\n==> %s\n' "$*"; }
note() { printf 'NOTE: %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
kc()   { kubectl --kubeconfig "${KCFG}" "$@"; }
have() { command -v "$1" >/dev/null 2>&1; }

# Control-plane health gate. The shared cluster's controller-manager + scheduler
# chronically crashloop under VM contention ("leaderelection lost"); when they
# are down, nothing schedules and a proof would measure the CLUSTER, not the
# playground. The AUTHORITATIVE signal is the container's CURRENT state: a
# healthy CP component is state=running AND ready=true; an unhealthy one is
# waiting (CrashLoopBackOff) / not-ready — a timing-independent snapshot, so we
# don't have to guess where we sit in the ~minutes-long restart cadence. We also
# HOLD if it restarted within CP_STABLE_WINDOW (may be briefly running+ready
# right after a restart, about to flip again). Override: SKIP_CLUSTER_HEALTH=1.
# pg-infra's "BASE up" is the definitive go — this gate is a conservative backstop.
CP_STABLE_WINDOW="${CP_STABLE_WINDOW:-120}"
# Emit "<ready>|<runningStartedAt>|<waitingReason>|<restartCount>" for a CP
# component (empty fields where unreadable).
cp_status() {
  kc -n kube-system get pods -l "$1" -o jsonpath='{range .items[0].status.containerStatuses[0]}{.ready}|{.state.running.startedAt}|{.state.waiting.reason}|{.restartCount}{end}' 2>/dev/null
}
# Assess one component; append to the global `reasons` if unhealthy.
assess_cp() {
  local name="$1" sel="$2" st ready started waiting
  st=$(cp_status "${sel}")
  [ -z "${st}" ] && { reasons="${reasons} ${name} status unreadable;"; return; }
  ready="${st%%|*}"; st="${st#*|}"; started="${st%%|*}"; st="${st#*|}"; waiting="${st%%|*}"
  if [ "${ready}" != "true" ] || [ -n "${waiting}" ]; then
    reasons="${reasons} ${name} not ready (ready=${ready:-?} state=${waiting:-non-running});"
    return
  fi
  # Ready+running: also HOLD if it just restarted (may flip again shortly).
  if [ -n "${started}" ]; then
    local s now; s=$(date -j -u -f "%Y-%m-%dT%H:%M:%SZ" "${started}" +%s 2>/dev/null || echo "")
    if [ -n "${s}" ]; then now=$(date -u +%s); local age=$(( now - s ))
      [ "${age}" -lt "${CP_STABLE_WINDOW}" ] && reasons="${reasons} ${name} restarted ${age}s ago (< ${CP_STABLE_WINDOW}s);"
    fi
  fi
}
cluster_health() {
  [ "${SKIP_CLUSTER_HEALTH:-0}" = 1 ] && { echo "ok: cluster health gate SKIPPED (SKIP_CLUSTER_HEALTH=1)"; return 0; }
  kc cluster-info >/dev/null 2>&1 || { note "cluster API unreachable via ${KCFG}"; return 1; }
  reasons=""
  assess_cp controller-manager 'component=kube-controller-manager'
  assess_cp scheduler          'component=kube-scheduler'
  if [ -n "${reasons}" ]; then
    note "CLUSTER CONTROL PLANE UNSTABLE:${reasons} — proofs would measure the cluster, not the playground. HOLD."
    return 1
  fi
  echo "ok: control plane healthy (controller-manager + scheduler running, ready, stable > ${CP_STABLE_WINDOW}s)"
  return 0
}
# Guard for a proof that SCHEDULES new pods (P1 bring-up): needs a healthy CP.
require_healthy_cluster() {
  cluster_health && return 0
  note "$1: held on unstable control plane (bring-up schedules new pods; re-run when the CP is stable)."
  return 2
}

# Workload health — the gate for proofs that EXERCISE already-running playground
# pods (P2 chat, P4 log read, P6 sink read, P8 dry-run). These don't schedule
# anything, so a control-plane flap doesn't affect them; what matters is that the
# playground's own pods are Running+Ready. This is why a chat succeeds even while
# kube-controller-manager crashloops (only NEW scheduling is blocked then).
workloads_healthy() {
  [ "${SKIP_CLUSTER_HEALTH:-0}" = 1 ] && { echo "ok: workload health gate SKIPPED"; return 0; }
  kc cluster-info >/dev/null 2>&1 || { note "cluster API unreachable via ${KCFG}"; return 1; }
  local d unhealthy=""
  for d in assistant sink streamco stub-llm; do
    local ready; ready=$(kc -n "${NS}" get deploy "${d}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
    [ "${ready:-0}" -ge 1 ] 2>/dev/null || unhealthy="${unhealthy} ${d}(ready=${ready:-0})"
  done
  if [ -n "${unhealthy}" ]; then
    note "PLAYGROUND WORKLOADS NOT READY in ns ${NS}:${unhealthy} — is BASE up? HOLD."
    return 1
  fi
  echo "ok: playground workloads Ready (assistant, sink, streamco, stub-llm)"
  return 0
}
require_healthy_workloads() {
  workloads_healthy && return 0
  note "$1: held — playground workloads not Ready (need BASE up)."
  return 2
}

# One-time: build the patch CLI the host consumer uses (P2/P3/P5).
build_patch() {
  [ -x "${PATCH_CMD}" ] && return 0
  have go || { warn "go not found — cannot build patch CLI"; return 1; }
  [ -f "${ASSISTANT_REPO}/go.mod" ] || { warn "no go.mod in ${ASSISTANT_REPO}"; return 1; }
  mkdir -p "${GO_BIN_DIR}"
  log "Building patch CLI → ${PATCH_CMD}"
  ( cd "${ASSISTANT_REPO}" && go build -o "${GO_BIN_DIR}/patch" ./cmd/patch ) 2>&1 | tee "${OUT}/go-build.log"
}

# Port-forward the in-cluster sink Service to the host so the driver can GET
# /events over HTTP (up.sh forwards the assistant + gateway, but NOT the sink).
# Self-reaped via pidfile; idempotent. Returns once /healthz answers.
SINK_PF_PID=""
sink_port_forward() {
  curl -fsS --max-time 1 "${SINK_URL}/healthz" >/dev/null 2>&1 && return 0
  local logf="${OUT}/pf-sink.log"
  kc -n "${NS}" port-forward svc/sink "${SINK_PORT}:7811" --address 127.0.0.1 >"${logf}" 2>&1 &
  SINK_PF_PID=$!; disown 2>/dev/null || true
  for _ in $(seq 1 30); do
    curl -fsS --max-time 1 "${SINK_URL}/healthz" >/dev/null 2>&1 && { echo "ok: sink forwarded on ${SINK_URL}"; return 0; }
    sleep 0.5
  done
  note "sink port-forward did not become healthy (see ${logf})"; return 1
}
stop_sink_pf() { [ -n "${SINK_PF_PID}" ] && kill "${SINK_PF_PID}" 2>/dev/null || true; SINK_PF_PID=""; }

# Port-forward the capability-provider adapter (overlay, ns agent-framework-
# playground) so P3/P5 can GET /projects/{name}/capability-documents.
ADAPTER_PF_PID=""
ADAPTER_PORT="${ADAPTER_PORT:-8085}"
adapter_port_forward() {
  curl -fsS --max-time 1 "${CAPABILITY_PROVIDER_URL}/projects/${PROJECT}/capability-documents" >/dev/null 2>&1 && return 0
  kc -n "${CATALOG_NS}" port-forward svc/capability-provider "${ADAPTER_PORT}:8080" --address 127.0.0.1 >"${OUT}/pf-adapter.log" 2>&1 &
  ADAPTER_PF_PID=$!; disown 2>/dev/null || true
  for _ in $(seq 1 30); do
    curl -fsS --max-time 1 "${CAPABILITY_PROVIDER_URL}/projects/${PROJECT}/capability-documents" >/dev/null 2>&1 && { echo "ok: adapter forwarded on ${CAPABILITY_PROVIDER_URL}"; return 0; }
    sleep 0.5
  done
  note "adapter port-forward not reachable (see ${OUT}/pf-adapter.log)"; return 1
}
stop_adapter_pf() { [ -n "${ADAPTER_PF_PID}" ] && kill "${ADAPTER_PF_PID}" 2>/dev/null || true; ADAPTER_PF_PID=""; }
require_healthy_adapter() {
  adapter_port_forward && return 0
  note "$1: held — capability-provider adapter not reachable (is the overlay up?)."
  return 2
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
  require_healthy_cluster P1 || return 2
  [ -x "${PLAYGROUND_UP_CMD%% *}" ] || { note "playground-up.sh not found at '${PLAYGROUND_UP_CMD}' — NOT PROVEN (pg-infra pending)"; return 1; }
  local up1="${OUT}/p1-up-run1.log" up2="${OUT}/p1-up-run2.log"
  bash -c "${PLAYGROUND_UP_CMD}" 2>&1 | tee "${up1}"; local rc1=${PIPESTATUS[0]}
  # Second run reuses already-loaded images (--skip-build) — proves the cluster-
  # mutation path is idempotent and re-reports UP, without rebuilding images.
  bash -c "${PLAYGROUND_UP_CMD} --skip-build" 2>&1 | tee "${up2}"; local rc2=${PIPESTATUS[0]}
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
  require_healthy_workloads P2 || return 2
  build_patch || true
  sink_port_forward || true
  # Do NOT wipe the sink: the user shares this env. P4/P6 filter by our contextId.
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
# pg-catalog interface: CRs are CLUSTER-SCOPED (no -n). The AgentBinding
# reconciler only WATCHES ServiceEntitlement — SAC changes need a POKE
# (annotate the entitlement) or they wait out the 5-min resync. Samples live in
# ${CATALOG_REPO}/hack/playground/samples/. All commands env-overridable.
SAMPLES="${SAMPLES:-${CATALOG_REPO}/hack/playground/samples}"
POKE_CMD="${POKE_CMD:-kubectl --kubeconfig ${KCFG} annotate serviceentitlement streaming-streamco-example playground.internal/poke=\$(date +%s.%N) --overwrite}"
SAC_APPLY_V1="${SAC_APPLY_V1:-kubectl --kubeconfig ${KCFG} apply -f ${SAMPLES}/20-serviceagentconfiguration-v1.yaml}"
SAC_APPLY_V2="${SAC_APPLY_V2:-kubectl --kubeconfig ${KCFG} apply -f ${SAMPLES}/21-serviceagentconfiguration-v2.yaml}"
SAC_UNPUBLISH="${SAC_UNPUBLISH:-kubectl --kubeconfig ${KCFG} delete -f ${SAMPLES}/30-serviceentitlement.yaml}"
# Restore the v1 baseline after the destructive unpublish so the shared overlay
# is left as we found it (re-create the entitlement + v1 SAC, then poke).
SAC_RESTORE="${SAC_RESTORE:-kubectl --kubeconfig ${KCFG} apply -f ${SAMPLES}/30-serviceentitlement.yaml && kubectl --kubeconfig ${KCFG} apply -f ${SAMPLES}/20-serviceagentconfiguration-v1.yaml}"
p3() {
  log "P3: OVERLAY live reconfiguration (v1 → v2 narrower → unpublish)"
  require_healthy_adapter P3 || return 2
  # P3's "next chat turn reflects it" needs the IN-CLUSTER assistant reading from
  # the adapter (CAPABILITY_PROVIDER_URL), not a static fixture. Detect + warn.
  local capsrc; capsrc=$(kc -n "${NS}" get deploy assistant -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value};{end}' 2>/dev/null)
  case "${capsrc}" in
    *CAPABILITY_PROVIDER_URL=*) echo "ok: assistant wired to the adapter (CAPABILITY_PROVIDER_URL present)" ;;
    *) note "assistant is FIXTURE-mode (no CAPABILITY_PROVIDER_URL) — the ADAPTER-LEVEL reconfig proof runs, but the chat-turn reflection sub-check cannot pass until pg-infra wires --with-catalog into the live assistant." ;;
  esac
  local run="env OUT_DIR=${OUT} SINK_URL=${SINK_URL} PATCH_URL=${PATCH_URL} PATCH_TOKEN=${PATCH_TOKEN} PATCH_CMD=${PATCH_CMD} PROJECT=${PROJECT} CAPABILITY_PROVIDER_URL=${CAPABILITY_PROVIDER_URL}"
  poke() { bash -c "${POKE_CMD}" 2>&1 | tee -a "${OUT}/p3-poke.log"; }
  # Stage v1.
  bash -c "${SAC_APPLY_V1}" 2>&1 | tee "${OUT}/p3-apply-v1.log"; poke
  sleep "${RECONCILE_WAIT:-5}"
  ${run} STAGE=v1 node "${CHECKS}" reconfig | tee "${OUT}/p3-v1.log"
  # Stage v2 (narrower toolSelector).
  bash -c "${SAC_APPLY_V2}" 2>&1 | tee "${OUT}/p3-apply-v2.log"; poke
  sleep "${RECONCILE_WAIT:-5}"
  ${run} STAGE=v2 node "${CHECKS}" reconfig | tee "${OUT}/p3-v2.log"
  # Stage unpublish (delete the entitlement — the watch fires, binding pruned).
  bash -c "${SAC_UNPUBLISH}" 2>&1 | tee "${OUT}/p3-unpublish.log"
  sleep "${RECONCILE_WAIT:-5}"
  ${run} STAGE=unpublished node "${CHECKS}" reconfig | tee "${OUT}/p3-unpublished.log"
  # Compare all three stages → verdict.
  ${run} STAGE=compare node "${CHECKS}" reconfig | tee "${OUT}/p3-compare-console.log"; local rc=${PIPESTATUS[0]}
  # RESTORE the v1 baseline so the shared overlay is left as we found it.
  log "P3: restoring v1 baseline (re-publish entitlement + v1 SAC)"
  bash -c "${SAC_RESTORE}" 2>&1 | tee "${OUT}/p3-restore.log"; poke
  sleep "${RECONCILE_WAIT:-5}"
  ${run} STAGE=v1 node "${CHECKS}" reconfig >/dev/null 2>&1 || true
  local restored; restored=$(node -e 'const s=require("'"${OUT}"'/playground-reconfig-v1.json");process.stdout.write(String((s.tools||[]).length))' 2>/dev/null || echo "?")
  echo "P3: baseline restored — v1 tool count now=${restored}"
  return "${rc}"
}

# ── P4: gateway token attribution ───────────────────────────────────────────
p4() {
  log "P4: gateway token attribution for our chat"
  require_healthy_workloads P4 || return 2
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
  require_healthy_adapter P5 || return 2
  local extra=""; [ -n "${UNENTITLED_TOKEN:-}" ] && extra="UNENTITLED_TOKEN=${UNENTITLED_TOKEN}"
  env OUT_DIR="${OUT}" PATCH_URL="${PATCH_URL}" PATCH_TOKEN="${PATCH_TOKEN}" PATCH_CMD="${PATCH_CMD}" \
    PROJECT="${PROJECT}" UNENTITLED_PROJECT="${UNENTITLED_PROJECT}" \
    CAPABILITY_PROVIDER_URL="${CAPABILITY_PROVIDER_URL}" ${extra} \
    node "${CHECKS}" entitlement | tee "${OUT}/p5-entitlement-console.log"
  return "${PIPESTATUS[0]}"
}

# ── P6: service-emitted usage at the sink ───────────────────────────────────
p6() {
  log "P6: sink shows service-emitted usage for the playground chat"
  # P6 reads the sink, but its DATA comes from a chat that needs a healthy CP —
  # hold it too so a held P2 doesn't surface as a false P6 failure.
  require_healthy_workloads P6 || return 2
  sink_port_forward || true
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
  log "P7 (catalog): envtest suite on feat/agent-framework-playground"
  [ -d "${CATALOG_REPO}" ] || { note "catalog repo not found at ${CATALOG_REPO}"; return 1; }
  have go || { note "go not found"; return 1; }
  # pg-catalog: NO Makefile; envtest assets via setup-envtest, then go test.
  # (install once: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.21)
  local cmd="${CATALOG_TEST_CMD:-export KUBEBUILDER_ASSETS=\"\$(setup-envtest use 1.31.0 -p path)\" && go test -timeout 8m ./... -count=1}"
  ( cd "${CATALOG_REPO}" && bash -c "${cmd}" ) 2>&1 | tee "${OUT}/p7-catalog-test.log"
  return "${PIPESTATUS[0]}"
}

# ── P8: playground-down --dry-run == our labeled resources (NO teardown) ─────
# Canonicalize a k8s object reference to `kind/name`, tolerant of both shapes:
#   my snapshot.sh `labeled`:      Kind/ns/name        (3 fields, CamelCase kind)
#   down.sh --dry-run (`-o name`): resource.group/name (kind before first '.')
# kind = lowercase text before the first '.' or '/'; name = last '/'-segment.
# Drops '##' headers, blanks, and the namespace container line (compared apart).
p8_canon() {
  awk -F'/' '
    /^##/ || /^[[:space:]]*$/ { next }
    { k=$1; sub(/[.].*$/,"",k); k=tolower(k); n=$NF; if (k!="" && n!="") print k"/"n }
  ' | sort -u
}
p8() {
  log "P8: playground-down --dry-run lists EXACTLY our labeled resources (NO teardown)"
  require_healthy_workloads P8 || return 2
  # Independent ground truth: my BROAD label sweep across ALL api-resources
  # (catches a stray the down-script's fixed kind list might miss), canonicalized.
  "${SNAP}" labeled | tee "${OUT}/p8-labeled-console.log"
  local truth="${OUT}/p8-truth-canon.txt"
  { p8_canon < "${OUT}/labeled-resources.txt"; echo "namespace/${NS}"; } | sort -u > "${truth}"
  [ -x "${PLAYGROUND_DOWN_CMD%% *}" ] || { note "playground-down.sh not found at '${PLAYGROUND_DOWN_CMD}' — NOT PROVEN (pg-infra pending)"; return 1; }
  bash -c "${PLAYGROUND_DOWN_CMD} ${DRYRUN_FLAG}" 2>&1 | tee "${OUT}/p8-dryrun.log"; local rc=${PIPESTATUS[0]}
  # Parse the objects the dry-run says it WOULD delete. Default: keep only lines
  # that look like an object ref (contain a '/', not our decorative prose), then
  # canonicalize. DRYRUN_PARSE overrides the extraction if the format changes.
  local parsed="${OUT}/p8-dryrun-canon.txt"
  if [ -n "${DRYRUN_PARSE:-}" ]; then
    bash -c "${DRYRUN_PARSE} < '${OUT}/p8-dryrun.log'" | p8_canon > "${parsed}" 2>/dev/null || true
  else
    grep -E '^(namespace/|[a-z][a-z0-9.-]*/)' "${OUT}/p8-dryrun.log" 2>/dev/null | p8_canon > "${parsed}" || true
  fi
  local only_truth only_dry
  only_truth=$(comm -23 "${truth}" "${parsed}" | tee "${OUT}/p8-only-labeled.txt" | grep -c . || true)
  only_dry=$(comm -13 "${truth}" "${parsed}" | tee "${OUT}/p8-only-dryrun.txt" | grep -c . || true)
  echo "P8: dry-run rc=${rc} labeled=$(grep -c . "${truth}") dryrun=$(grep -c . "${parsed}") only-in-labeled=${only_truth} only-in-dryrun=${only_dry}"
  [ "${only_truth}" != 0 ] && { echo "  only in labeled truth (down.sh would MISS):"; sed 's/^/    /' "${OUT}/p8-only-labeled.txt"; }
  [ "${only_dry}" != 0 ] && { echo "  only in dry-run (claims to delete, but unlabeled?):"; sed 's/^/    /' "${OUT}/p8-only-dryrun.txt"; }
  diff -u "${truth}" "${parsed}" > "${OUT}/p8-set.diff" 2>/dev/null || true
  [ "${rc}" -eq 0 ] && [ "${only_truth}" -eq 0 ] && [ "${only_dry}" -eq 0 ]
}

# ── Phase groupings ─────────────────────────────────────────────────────────
# Verdict accumulator — plain newline-delimited "proof<TAB>verdict" lines so this
# stays compatible with macOS's default bash 3.2 (no associative arrays).
VERDICTS=""
mark() {
  local v
  case "$2" in
    0) v=PROVEN ;;
    2) v="HELD (CP unstable)" ;;
    *) v="NOT PROVEN" ;;
  esac
  VERDICTS="${VERDICTS}$(printf '%s\t%s' "$1" "$v")
"
}
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
  printf '%s' "${VERDICTS}" | grep -v '^$' | sort | while IFS="$(printf '\t')" read -r k v; do
    printf '  %-14s %s\n' "$k" "$v"
  done
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
