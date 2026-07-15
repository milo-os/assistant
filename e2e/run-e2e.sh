#!/usr/bin/env bash
# Assistant service — end-to-end runner (QA-owned).
#
# GO PORT (slice 5): the harness now drives the BUILT Go binaries — build_go()
# compiles cmd/assistant + cmd/patch into ${OUT}/bin before each live leg, and
# the boot/CLI commands default to those binaries (no bun in the core/gateway
# legs). The A2A stream assertions in driver/a2a-checks.mjs are A2A v1.0-shaped
# (TASK_STATE_* enums, StreamResponse oneOf, no kind/final). The capability-doc
# fixture env was renamed AGENT_BINDINGS_FIXTURE → CAPABILITY_DOCS_FIXTURE.
# run_full additionally byte-diffs the sink CloudEvents against the pre-port TS
# golden (golden/sink-cloudevents.golden.jsonl) via golden/normalize-sink.mjs.
#
# Proves CONTRACT-ASSISTANT.md "Definition of done / QA" items 1-6 against the
# REAL running assistant service over HTTP with a dev bearer token. All model
# inference is MOCKED unless ANTHROPIC_API_KEY is set (contract: real-model
# parity is a documented risk; a real key triggers an additional real pass).
#
# Topology it stands up locally:
#   StreamCo demo provider   127.0.0.1:7810  (MCP /mcp + knowledge routes)
#   Usage capture sink       127.0.0.1:7811  (POST /cloudevents, GET /events)
#   Assistant service        127.0.0.1:7820  (A2A card + JSON-RPC /a2a)
#
# Subcommands:
#   selftests   StreamCo + sink standalone selftests (no assistant needed)
#   full        build Go binaries + boot all three + run the A2A assertion
#               driver (items 2-5) + sink golden byte-diff
#   repo-tests  `go vet ./...` + `go test ./...` in the assistant repo (item 6)
#   go-build    just build cmd/assistant + cmd/patch (Phase-2 smoke)
#   all         full + repo-tests   (default)
#   consumers   boot the stack + prove the consumers (task #12): the patch CLI,
#               the portal client-mode path, and no-double-metering. The portal
#               sub-leg auto-skips until feat/portal-assistant-client + its test
#               command are available.
#
# The assistant boot command/env is wired here; override via env when the
# engineer's exact command differs (ASSISTANT_START_CMD, ASSISTANT_REPO,
# AUTH_DEV_TOKENS). Set ASSISTANT_NO_BOOT=1 to test an already-running service.
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${E2E_DIR}/out"
mkdir -p "${OUT}"

# ── Config ───────────────────────────────────────────────────────────────────
STREAMCO_HOST="${STREAMCO_HOST:-127.0.0.1}"; STREAMCO_PORT="${STREAMCO_PORT:-7810}"
SINK_HOST="${SINK_HOST:-127.0.0.1}";         SINK_PORT="${SINK_PORT:-7811}"
ASSISTANT_HOST="${ASSISTANT_HOST:-127.0.0.1}"; ASSISTANT_PORT="${ASSISTANT_PORT:-7820}"

PROJECT="${PROJECT:-demo-project}"
OTHER_PROJECT="${OTHER_PROJECT:-other-project}"
# Token values match the service's AUTH_MODE=dev table below (confirmed with
# assistant-engineer): e2e-token grants demo-project (200 path), wrong-token
# grants only other-project (403 path).
GOOD_TOKEN="${GOOD_TOKEN:-e2e-token}"
WRONGPROJ_TOKEN="${WRONGPROJ_TOKEN:-wrong-token}"

# Go port: the capability-document fixture env was renamed
# AGENT_BINDINGS_FIXTURE → CAPABILITY_DOCS_FIXTURE (contract inversion). The QA
# fixture files keep their names (JSON shape is unchanged); accept either env
# override so an operator can point at a different doc set.
FIXTURE="${CAPABILITY_DOCS_FIXTURE:-${AGENT_BINDINGS_FIXTURE:-${E2E_DIR}/fixtures/agent-bindings.fixture.json}}"
STREAMCO_URL="http://${STREAMCO_HOST}:${STREAMCO_PORT}"
SINK_URL="http://${SINK_HOST}:${SINK_PORT}"
ASSISTANT_URL="http://${ASSISTANT_HOST}:${ASSISTANT_PORT}"
STREAMCO_LOG="${OUT}/streamco.log"

ASSISTANT_REPO="${ASSISTANT_REPO:-/Users/scotwells/repos/milo-os/assistant}"
# Go port: the harness drives the BUILT Go binaries (cmd/assistant + cmd/patch),
# not bun. build_go() compiles them into GO_BIN_DIR before each live leg; the
# boot/CLI commands default to those binaries. Override to test another build.
GO_BIN_DIR="${GO_BIN_DIR:-${OUT}/bin}"
ASSISTANT_START_CMD="${ASSISTANT_START_CMD:-${GO_BIN_DIR}/assistant}"

# Dev-token env for the assistant. Format (confirmed, src/auth/dev.ts):
# "token:subject:projA,projB;..." — ';'-separated entries, 3 ':'-fields,
# comma-separated projects, '*' grants all.
AUTH_DEV_TOKENS="${AUTH_DEV_TOKENS:-${GOOD_TOKEN}:e2e-user:${PROJECT};${WRONGPROJ_TOKEN}:other:${OTHER_PROJECT}}"
# The service builds the agent card url from PUBLIC_BASE_URL; keep it on the
# same host the driver hits so card.url is reachable (avoid localhost/IPv6).
PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-http://${ASSISTANT_HOST}:${ASSISTANT_PORT}}"

# ── Consumers config (task #12 / CONTRACT-CONSUMERS.md Workstream C) ──────────
# CLI (Workstream C): the patch CLI is now the built Go binary (cmd/patch),
# honors PATCH_URL/PATCH_TOKEN, answer on stdout.
PATCH_CMD="${PATCH_CMD:-${GO_BIN_DIR}/patch}"
PATCH_TOKEN="${PATCH_TOKEN:-${GOOD_TOKEN}}"
BAD_TOKEN="${BAD_TOKEN:-not-a-real-token}"
# Portal (Workstream A): live-service integration test on the client-mode branch.
CP_REPO="${CP_REPO:-/Users/scotwells/repos/datum-cloud/cloud-portal}"
PORTAL_BRANCH="${PORTAL_BRANCH:-feat/portal-assistant-client}"
PORTAL_WORKTREE="${PORTAL_WORKTREE:-${OUT}/portal-worktree}"
ASSISTANT_SERVICE_E2E_URL="${ASSISTANT_SERVICE_E2E_URL:-${ASSISTANT_URL}}"
ASSISTANT_SERVICE_E2E_TOKEN="${ASSISTANT_SERVICE_E2E_TOKEN:-${GOOD_TOKEN}}"
# Confirmed with portal-engineer (feat/portal-assistant-client): the route
# e2e uses bun:test and auto-skips when ASSISTANT_SERVICE_E2E_URL is unset.
PORTAL_E2E_TEST_CMD="${PORTAL_E2E_TEST_CMD:-bun test app/modules/assistant/client/route.e2e.test.ts}"
PORTAL_SCOPED_TEST_CMD="${PORTAL_SCOPED_TEST_CMD:-bun test app/modules/assistant app/modules/usage}"
PORTAL_TYPECHECK_CMD="${PORTAL_TYPECHECK_CMD:-bun run typecheck}"
PORTAL_PROJECT="${PORTAL_PROJECT:-${PROJECT}}"
# Pin the portal conversation's contextId so the no-double-metering assertion
# can bind sink events to exactly this conversation (the portal client sends
# contextId = conversation id; the service meters under resource.name = it).
PORTAL_CONVERSATION="${PORTAL_CONVERSATION:-e2e-portal-conv-1}"

# ── Gateway config (task #15 / CONTRACT-GATEWAY.md) ──────────────────────────
# infra-engineer owns e2e/gateway/**; this leg CALLS their up.sh/down.sh and
# derives the in-cluster service addresses from their manifests. Values from
# e2e/gateway/.run/env + README + manifests (Envoy Gateway v1.8.1 + AI Gateway v1.0.0).
TEST_INFRA="${TEST_INFRA:-/Users/scotwells/repos/datum-cloud/test-infra}"
GW_DIR="${GW_DIR:-${E2E_DIR}/gateway}"
GW_NS="${GW_NS:-patch-ai-gateway}"
GW_KUBECONFIG="${KUBECONFIG:-${TEST_INFRA}/kubeconfig}"
GW_UP_CMD="${GW_UP_CMD:-${GW_DIR}/up.sh}"
GW_DOWN_CMD="${GW_DOWN_CMD:-${GW_DIR}/down.sh}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:1975/v1}"       # LLM base (client appends /chat/completions)
GATEWAY_MODEL="${GATEWAY_MODEL:-patch-stub-v1}"
GATEWAY_MCP_URL="${GATEWAY_MCP_URL:-http://localhost:1975/mcp}"
GW_FIXTURE="${GW_FIXTURE:-${E2E_DIR}/fixtures/agent-bindings.gateway.json}"
GW_PROJECT="${GW_PROJECT:-${PROJECT}}"
# Direct-access controls: port-forward the in-cluster services to the host.
GW_STREAMCO_PF_PORT="${GW_STREAMCO_PF_PORT:-7810}"
GW_STUB_PF_PORT="${GW_STUB_PF_PORT:-8080}"
STREAMCO_DIRECT_URL="http://127.0.0.1:${GW_STREAMCO_PF_PORT}/mcp"
STUB_DIRECT_URL="http://127.0.0.1:${GW_STUB_PF_PORT}/v1/chat/completions"
# Access log (proof 3): the gateway's Envoy data-plane pod(s), JSON lines. Capture
# ALL lines (--all-containers, all pods matching the owning-gateway label); the
# driver parses JSON + filters by contextId, so we don't rely on a grep pattern.
GW_ACCESSLOG_CMD="${GW_ACCESSLOG_CMD:-kubectl --kubeconfig ${GW_KUBECONFIG} -n envoy-gateway-system logs -l gateway.envoyproxy.io/owning-gateway-name=patch-ai-gateway --all-containers=true --tail=-1}"
GW_STARTED=0
GW_WITH_RATELIMIT="${GW_WITH_RATELIMIT:-0}"

log()  { printf '\n==> %s\n' "$*"; }
note() { printf 'NOTE: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

PIDS=()
BOOTED_PORTS=()
cleanup() {
  local code=$?
  for pid in "${PIDS[@]:-}"; do
    [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null && kill "${pid}" 2>/dev/null || true
  done
  # Backstop: the tracked PID is the launching subshell, but the actual
  # listener can be a grandchild (e.g. `bun run start` forks a child bun, or
  # a Node child of the subshell). Kill whatever still listens on the ports we
  # booted so repeated runs never collide.
  for port in "${BOOTED_PORTS[@]:-}"; do
    [[ -z "${port}" ]] && continue
    local lp; lp=$(lsof -ti "tcp:${port}" -sTCP:LISTEN 2>/dev/null || true)
    [[ -n "${lp}" ]] && kill ${lp} 2>/dev/null || true
  done
  # Remove the portal worktree if the consumers leg created one.
  if [[ -n "${PORTAL_WORKTREE:-}" && -d "${PORTAL_WORKTREE}" ]]; then
    git -C "${CP_REPO}" worktree remove --force "${PORTAL_WORKTREE}" 2>/dev/null \
      || rm -rf "${PORTAL_WORKTREE}" 2>/dev/null || true
  fi
  # Tear down the gateway layer if this run brought it up (down.sh removes only
  # OUR namespace/manifests; it never runs test-infra cluster-down).
  if [[ "${GW_STARTED:-0}" == "1" ]]; then
    bash -c "${GW_DOWN_CMD}" >>"${OUT}/gateway-down.log" 2>&1 || true
  fi
  sleep 0.3 || true
  exit "${code}"
}
trap cleanup EXIT

health() { curl -fsS --max-time 1 "$1/healthz" >/dev/null 2>&1; }
wait_for_health() {
  local name="$1" url="$2" deadline=$((SECONDS + ${3:-20}))
  until health "${url}"; do
    (( SECONDS >= deadline )) && fail "${name} did not become healthy at ${url} within ${3:-20}s (see ${OUT})"
    sleep 0.3
  done
  echo "ok: ${name} healthy at ${url}"
}

ensure_tools() {
  for t in node curl jq; do command -v "${t}" >/dev/null || fail "required tool not found: ${t}"; done
}

# Go port: build the assistant service + patch CLI binaries the harness drives.
# Run before every live leg. Fails loudly (not silently) if the Go sources are
# not yet present — during the port that means the engineers haven't landed
# cmd/assistant or cmd/patch on feat/go-port yet.
build_go() {
  command -v go >/dev/null || fail "go not found (required to build cmd/assistant + cmd/patch)"
  [[ -d "${ASSISTANT_REPO}" ]] || fail "assistant repo not found at ${ASSISTANT_REPO}"
  [[ -f "${ASSISTANT_REPO}/go.mod" ]] || fail "no go.mod in ${ASSISTANT_REPO} — Go port not landed yet (run against feat/go-port once cmd/assistant + cmd/patch exist)"
  mkdir -p "${GO_BIN_DIR}"
  log "Building Go binaries (go build ./cmd/assistant ./cmd/patch → ${GO_BIN_DIR})"
  ( cd "${ASSISTANT_REPO}" && go build -o "${GO_BIN_DIR}/assistant" ./cmd/assistant ) 2>&1 | tee "${OUT}/go-build.log" || fail "go build ./cmd/assistant failed (see ${OUT}/go-build.log)"
  ( cd "${ASSISTANT_REPO}" && go build -o "${GO_BIN_DIR}/patch" ./cmd/patch ) 2>&1 | tee -a "${OUT}/go-build.log" || fail "go build ./cmd/patch failed (see ${OUT}/go-build.log)"
  echo "ok: built ${GO_BIN_DIR}/assistant and ${GO_BIN_DIR}/patch"
}

# Byte-diff the sink's received CloudEvents against the golden recorded from the
# TS emitter BEFORE the port (contract: usage wire must be byte-compatible). The
# normalizer masks only volatile fields (ULID id, time, contextId); everything
# else — type, source, subject, value, dimensions, resource — is the contract.
check_sink_golden() {
  local golden="${E2E_DIR}/golden/sink-cloudevents.golden.jsonl"
  local cap="${OUT}/captured-events.jsonl"
  [[ -f "${golden}" ]] || { note "no sink golden at ${golden} — skipping byte-diff"; return 0; }
  [[ -f "${cap}"    ]] || { note "no captured events at ${cap} — cannot byte-diff sink wire"; return 1; }
  log "Byte-diffing sink CloudEvents vs the TS golden (${golden})"
  node "${E2E_DIR}/golden/normalize-sink.mjs" --check "${cap}" "${golden}" 2>&1 | tee "${OUT}/sink-golden-check.log"
}

boot_streamco() {
  log "Booting StreamCo demo provider on ${STREAMCO_URL}"
  ( cd "${E2E_DIR}/streamco" && STREAMCO_HOST="${STREAMCO_HOST}" STREAMCO_PORT="${STREAMCO_PORT}" \
      node src/server.ts ) >"${STREAMCO_LOG}" 2>&1 &
  PIDS+=("$!"); BOOTED_PORTS+=("${STREAMCO_PORT}"); disown
  wait_for_health "streamco" "${STREAMCO_URL}"
}

boot_sink() {
  log "Booting usage capture sink on ${SINK_URL}"
  ( SINK_HOST="${SINK_HOST}" SINK_PORT="${SINK_PORT}" CAPTURE_FILE="${OUT}/captured-events.jsonl" \
      node "${E2E_DIR}/sink/sink.mjs" ) >"${OUT}/sink.log" 2>&1 &
  PIDS+=("$!"); BOOTED_PORTS+=("${SINK_PORT}"); disown
  wait_for_health "sink" "${SINK_URL}"
  curl -fsS -X DELETE "${SINK_URL}/events" >/dev/null 2>&1 || true
}

boot_assistant() {
  if [[ "${ASSISTANT_NO_BOOT:-0}" == "1" ]]; then
    log "ASSISTANT_NO_BOOT=1 — expecting an already-running service at ${ASSISTANT_URL}"
    wait_for_health "assistant" "${ASSISTANT_URL}"
    return
  fi
  [[ -d "${ASSISTANT_REPO}" ]] || fail "assistant repo not found at ${ASSISTANT_REPO} (set ASSISTANT_REPO or ASSISTANT_NO_BOOT=1)"
  log "Booting assistant service on ${ASSISTANT_URL} (cmd: ${ASSISTANT_START_CMD})"
  note "model mode: ${MODEL_MODE:-$([[ -n "${ANTHROPIC_API_KEY:-}" ]] && echo anthropic || echo mock)}"
  ( cd "${ASSISTANT_REPO}" \
      && PORT="${ASSISTANT_PORT}" \
         AUTH_MODE="${AUTH_MODE:-dev}" \
         AUTH_DEV_TOKENS="${AUTH_DEV_TOKENS}" \
         CAPABILITY_DOCS_FIXTURE="${FIXTURE}" \
         MODEL_MODE="${MODEL_MODE:-mock}" \
         USAGE_GATEWAY_URL="${SINK_URL}" \
         PUBLIC_BASE_URL="${PUBLIC_BASE_URL}" \
         ${ANTHROPIC_API_KEY:+ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY}"} \
         ${ANTHROPIC_MODEL:+ANTHROPIC_MODEL="${ANTHROPIC_MODEL}"} \
         ${USAGE_GATEWAY_API_KEY:+USAGE_GATEWAY_API_KEY="${USAGE_GATEWAY_API_KEY}"} \
         bash -c "${ASSISTANT_START_CMD}" ) >"${OUT}/assistant.log" 2>&1 &
  PIDS+=("$!"); BOOTED_PORTS+=("${ASSISTANT_PORT}"); disown
  wait_for_health "assistant" "${ASSISTANT_URL}" 30
}

run_selftests() {
  ensure_tools
  log "StreamCo selftest"
  ( cd "${E2E_DIR}/streamco" && node src/selftest.ts ) | tee "${OUT}/streamco-selftest.log"
  log "Sink selftest"
  node "${E2E_DIR}/sink/selftest.mjs" | tee "${OUT}/sink-selftest.log"
}

run_driver() {
  log "Running A2A assertion driver against ${ASSISTANT_URL}"
  OUT_DIR="${OUT}" \
  ASSISTANT_URL="${ASSISTANT_URL}" \
  SINK_URL="${SINK_URL}" \
  STREAMCO_LOG="${STREAMCO_LOG}" \
  PROJECT="${PROJECT}" \
  GOOD_TOKEN="${GOOD_TOKEN}" \
  WRONGPROJ_TOKEN="${WRONGPROJ_TOKEN}" \
    node "${E2E_DIR}/driver/a2a-checks.mjs" | tee "${OUT}/driver-console.log"
}

run_full() {
  ensure_tools
  build_go
  boot_streamco
  boot_sink
  boot_assistant
  run_driver
  # Additional proof: the Go emitter's sink wire is byte-compatible with the TS
  # golden. Non-fatal (loudly logged) — I read the result for the report.
  check_sink_golden || note "sink CloudEvent golden DRIFT (see ${OUT}/sink-golden-check.log)"
}

run_repo_tests() {
  [[ -d "${ASSISTANT_REPO}" ]] || fail "assistant repo not found at ${ASSISTANT_REPO}"
  command -v go >/dev/null || fail "go not found (required for repo tests)"
  [[ -f "${ASSISTANT_REPO}/go.mod" ]] || fail "no go.mod in ${ASSISTANT_REPO} — Go port not landed yet"
  log "Assistant repo: go vet ./..."
  ( cd "${ASSISTANT_REPO}" && go vet ./... ) 2>&1 | tee "${OUT}/repo-govet.log"
  log "Assistant repo: go test ./..."
  ( cd "${ASSISTANT_REPO}" && go test ./... ) 2>&1 | tee "${OUT}/repo-gotest.log"
}

# ── Consumers leg (task #12 / CONTRACT-CONSUMERS.md Workstream C) ─────────────
run_consumer_cli() {
  log "Consumer leg 1/2: CLI (patch)"
  note "CLI invocation: ${PATCH_CMD}"
  OUT_DIR="${OUT}" SINK_URL="${SINK_URL}" PATCH_URL="${ASSISTANT_URL}" \
  PATCH_TOKEN="${PATCH_TOKEN}" BAD_TOKEN="${BAD_TOKEN}" PROJECT="${PROJECT}" \
  PATCH_CMD="${PATCH_CMD}" \
    node "${E2E_DIR}/driver/consumers-checks.mjs" cli | tee "${OUT}/consumers-cli-console.log"
}

run_consumer_portal() {
  log "Consumer leg 2/2: portal client-mode against the live service"
  # Read-only worktree of the portal branch — never touches cloud-portal's tree.
  if [[ ! -d "${PORTAL_WORKTREE}" ]]; then
    git -C "${CP_REPO}" worktree add --detach "${PORTAL_WORKTREE}" "${PORTAL_BRANCH}" \
      || fail "could not create portal worktree for ${PORTAL_BRANCH}"
  fi
  ( cd "${PORTAL_WORKTREE}" && bun install --frozen-lockfile ) >"${OUT}/portal-bun-install.log" 2>&1 \
    || note "bun install in portal worktree had warnings (see ${OUT}/portal-bun-install.log)"
  # Wipe the sink so the no-double-metering assertion sees only this conversation.
  curl -fsS -X DELETE "${SINK_URL}/events" >/dev/null 2>&1 || true
  log "Portal live-service integration test (A4): ${PORTAL_E2E_TEST_CMD}"
  ( cd "${PORTAL_WORKTREE}" \
      && ASSISTANT_SERVICE_E2E_URL="${ASSISTANT_SERVICE_E2E_URL}" \
         ASSISTANT_SERVICE_E2E_TOKEN="${ASSISTANT_SERVICE_E2E_TOKEN}" \
         ASSISTANT_SERVICE_E2E_PROJECT="${PORTAL_PROJECT}" \
         ASSISTANT_SERVICE_E2E_CONVERSATION="${PORTAL_CONVERSATION}" \
         bash -c "${PORTAL_E2E_TEST_CMD}" ) 2>&1 | tee "${OUT}/portal-e2e-test.log"
  log "No-double-metering assertion (sink = service-only, pinned contextId ${PORTAL_CONVERSATION})"
  OUT_DIR="${OUT}" SINK_URL="${SINK_URL}" \
  SERVICE_SOURCE_MARKER=":${ASSISTANT_PORT}/a2a" \
  EXPECT_CONTEXT_ID="${PORTAL_CONVERSATION}" \
    node "${E2E_DIR}/driver/consumers-checks.mjs" nodemeter | tee "${OUT}/consumers-nodemeter-console.log"
  if [[ -n "${PORTAL_SCOPED_TEST_CMD}" ]]; then
    log "Portal scoped suites on ${PORTAL_BRANCH}: ${PORTAL_SCOPED_TEST_CMD}"
    ( cd "${PORTAL_WORKTREE}" && bash -c "${PORTAL_SCOPED_TEST_CMD}" ) 2>&1 | tee "${OUT}/portal-scoped-tests.log" || note "scoped portal tests reported failures (see log)"
  fi
  log "Portal typecheck on ${PORTAL_BRANCH}: ${PORTAL_TYPECHECK_CMD}"
  ( cd "${PORTAL_WORKTREE}" && bash -c "${PORTAL_TYPECHECK_CMD}" ) 2>&1 | tee "${OUT}/portal-typecheck.log" || note "portal typecheck reported errors (see log)"
}

run_consumers() {
  ensure_tools
  build_go   # the patch CLI consumer is now the built Go binary
  boot_streamco
  boot_sink
  boot_assistant
  run_consumer_cli
  # The portal client-mode sub-leg still runs on bun. NOTE (contract): the portal
  # translator (cloud-portal feat/portal-assistant-client) is v0.3-shaped and
  # needs a v1.0 update before it can drive the Go service — a documented
  # follow-up. Only run it if bun + the branch are present.
  if command -v bun >/dev/null && git -C "${CP_REPO}" rev-parse --verify "refs/heads/${PORTAL_BRANCH}" >/dev/null 2>&1 && [[ -n "${PORTAL_E2E_TEST_CMD}" ]]; then
    run_consumer_portal
  else
    note "Portal client-mode leg SKIPPED — bun/branch ${PORTAL_BRANCH} absent, or the portal translator still needs its A2A v1.0 update (follow-up). Ran the CLI leg only."
  fi
}

# ── Gateway leg (task #15 / CONTRACT-GATEWAY.md) ─────────────────────────────
gw_kubectl() { kubectl --kubeconfig "${GW_KUBECONFIG}" "$@"; }
gw_port_forward() {
  gw_kubectl -n "${GW_NS}" port-forward "svc/$1" "$2:$3" >>"${OUT}/gateway-portforward.log" 2>&1 &
  # Track both the PID and the local port — kubectl port-forward doesn't always
  # die on the subshell kill, so the port-based backstop in cleanup() reaps it.
  PIDS+=("$!"); BOOTED_PORTS+=("$2"); disown
}

run_gateway() {
  ensure_tools
  command -v kubectl >/dev/null || fail "kubectl required for gateway leg"
  command -v docker >/dev/null || fail "docker required for gateway leg"
  [[ -d "${TEST_INFRA}" ]] || fail "test-infra not found at ${TEST_INFRA}"
  [[ -f "${GW_UP_CMD%% *}" ]] || fail "gateway up.sh not found (${GW_UP_CMD}); infra-engineer owns e2e/gateway/"
  build_go   # service (gateway mode) + patch CLI are the built Go binaries

  # G1: bring up the gateway env (idempotent). Track test-infra cleanliness.
  local ti_before; ti_before=$(git -C "${TEST_INFRA}" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
  log "G1: bring up Envoy AI Gateway env"
  GW_STARTED=1
  local up_flags=""; [[ "${GW_WITH_RATELIMIT}" == "1" ]] && up_flags="--with-ratelimit"
  bash -c "${GW_UP_CMD} ${up_flags}" 2>&1 | tee "${OUT}/gateway-up.log"
  [[ "${PIPESTATUS[0]}" -eq 0 ]] || fail "gateway bring-up failed (see ${OUT}/gateway-up.log)"

  # Host workloads: sink + assistant in gateway mode (StreamCo + stub are in-cluster).
  boot_sink
  gw_port_forward streamco "${GW_STREAMCO_PF_PORT}" 7810
  gw_port_forward stub-llm "${GW_STUB_PF_PORT}" 8080
  sleep 2  # let port-forwards establish + Envoy access-log flush headroom

  log "Booting assistant in MODEL_MODE=gateway (NO model API key — credential isolation)"
  note "GATEWAY_URL=${GATEWAY_URL} model=${GATEWAY_MODEL} fixture=${GW_FIXTURE}"
  ( cd "${ASSISTANT_REPO}" \
      && PORT="${ASSISTANT_PORT}" AUTH_MODE=dev AUTH_DEV_TOKENS="${AUTH_DEV_TOKENS}" \
         CAPABILITY_DOCS_FIXTURE="${GW_FIXTURE}" USAGE_GATEWAY_URL="${SINK_URL}" \
         PUBLIC_BASE_URL="${PUBLIC_BASE_URL}" \
         MODEL_MODE=gateway GATEWAY_URL="${GATEWAY_URL}" GATEWAY_MODEL="${GATEWAY_MODEL}" \
         bash -c "${ASSISTANT_START_CMD}" ) >"${OUT}/assistant-gateway.log" 2>&1 &
  PIDS+=("$!"); BOOTED_PORTS+=("${ASSISTANT_PORT}"); disown
  wait_for_health "assistant(gateway)" "${ASSISTANT_URL}" 30

  # G2: CLI chat end-to-end through the gateway (writes contextId → out/).
  log "G2: patch chat through the gateway"
  curl -fsS -X DELETE "${SINK_URL}/events" >/dev/null 2>&1 || true
  # Each proof is NON-FATAL to the leg: one run collects ALL of G2-G7 and the
  # per-proof summaries drive the report (honesty over green — I read results).
  OUT_DIR="${OUT}" SINK_URL="${SINK_URL}" PATCH_URL="${ASSISTANT_URL}" PATCH_TOKEN="${PATCH_TOKEN}" \
  PROJECT="${GW_PROJECT}" PATCH_CMD="${PATCH_CMD}" \
    node "${E2E_DIR}/driver/gateway-checks.mjs" chat | tee "${OUT}/gateway-chat-console.log" || true

  # The diagnose conversation makes TWO model calls, so the gateway writes two
  # log.type:llm access-log lines carrying x_datum_conversation=<contextId>.
  # They flush shortly after the streamed responses end — retry until BOTH are
  # present (or timeout). Capture ALL lines (clean JSON, no --prefix); the driver
  # parses + filters by contextId.
  local ctxid; ctxid=$(cat "${OUT}/gateway-contextid.txt" 2>/dev/null || echo "")
  log "Collecting gateway access logs (waiting for the 2 llm lines of conversation ${ctxid})"
  # Resolve the specific RUNNING data-plane Envoy pod — a bare label selector is
  # ambiguous right after the EG restart (old terminating + new pod), and the
  # llm records live only on the pod that served this chat.
  local pod; pod=$(gw_kubectl -n envoy-gateway-system get pods \
      -l gateway.envoyproxy.io/owning-gateway-name=patch-ai-gateway \
      --field-selector=status.phase=Running \
      -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null) || pod=""
  local llm_ctx=0
  for _ in $(seq 1 15); do  # up to ~45s for the streamed-response access-log flush
    gw_kubectl -n envoy-gateway-system logs "${pod}" --all-containers --tail=-1 \
      > "${OUT}/gateway-access.log" 2>/dev/null || true
    llm_ctx=$(grep '"log.type":"llm"' "${OUT}/gateway-access.log" 2>/dev/null | grep -c "${ctxid}" 2>/dev/null) || llm_ctx=0
    [[ "${llm_ctx:-0}" -ge 2 ]] && break
    sleep 3
  done
  echo "access-log: pod=${pod} llm-lines-for-conversation=${llm_ctx:-0}"

  # G5: MCPRoute allow-list (MCP SDK resolves via e2e/node_modules — ESM ignores NODE_PATH).
  log "G5: MCPRoute allow-list (gateway vs direct)"
  OUT_DIR="${OUT}" GATEWAY_MCP_URL="${GATEWAY_MCP_URL}" STREAMCO_DIRECT_URL="${STREAMCO_DIRECT_URL}" \
    node "${E2E_DIR}/driver/gateway-checks.mjs" allowlist | tee "${OUT}/gateway-allowlist-console.log" || true

  # G3: gateway-counted tokens vs the sink.
  log "G3: gateway-counted tokens + attribution"
  OUT_DIR="${OUT}" ACCESS_LOG_FILE="${OUT}/gateway-access.log" SINK_URL="${SINK_URL}" PROJECT="${GW_PROJECT}" \
    node "${E2E_DIR}/driver/gateway-checks.mjs" tokens | tee "${OUT}/gateway-tokens-console.log" || true

  # G4: credential isolation.
  log "G4: credential isolation (direct stub → 401)"
  if grep -iqE '(ANTHROPIC|OPENAI)_API_KEY' "${OUT}/assistant-gateway.log"; then
    note "a model API key may be present in gateway-mode boot — inspect ${OUT}/assistant-gateway.log"
  else
    echo "ok: no model API key in the gateway-mode boot env"
  fi
  OUT_DIR="${OUT}" STUB_DIRECT_URL="${STUB_DIRECT_URL}" \
    node "${E2E_DIR}/driver/gateway-checks.mjs" credisolation | tee "${OUT}/gateway-cred-console.log" || true

  # G7: repo suites with the new seams.
  run_repo_tests || note "repo suites reported failures (see logs)"

  local ti_after; ti_after=$(git -C "${TEST_INFRA}" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
  if [[ "${ti_before}" == "${ti_after}" ]]; then echo "ok: test-infra working tree byte-clean"; else note "test-infra working tree changed (${ti_before}->${ti_after}) — investigate (must stay clean)"; fi
  log "Gateway teardown runs on exit (down.sh). Evidence in ${OUT}/"
}

main() {
  local cmd="${1:-all}"
  case "${cmd}" in
    selftests)  run_selftests ;;
    full)       run_full ;;
    repo-tests) run_repo_tests ;;
    consumers)  run_consumers ;;
    gateway)    run_gateway ;;
    go-build)   build_go ;;
    all)        run_full; run_repo_tests ;;
    *) fail "unknown subcommand '${cmd}' (use: selftests | full | repo-tests | consumers | gateway | go-build | all)" ;;
  esac
  log "Done (${cmd}). Evidence in ${OUT}/"
}

main "$@"
