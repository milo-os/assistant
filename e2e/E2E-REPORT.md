# Assistant Service — E2E Acceptance Report (Go port)

Owner: go-qa (Workstream D). Contract: scratchpad `CONTRACT-GO-PORT.md`,
section "Workstreams → D — qa".

Rule: every item below is either **PROVEN** with commands + trimmed output, or
marked **NOT PROVEN** / **NOT RUN** with a concrete reason. No item is green
without captured evidence from an actual run against the built Go binaries over
HTTP with a dev bearer token. **Honesty over green** — a NOT PROVEN with
evidence beats a hollow PROVEN.

> This report covers the **Go port** (TypeScript → Go). The acceptance harness
> is the same one that proved the TS service; it was adapted to build and drive
> the Go binaries, to assert the **A2A v1.0** wire, and to byte-diff the Go
> usage emitter against a golden recorded from the TS emitter **before** the TS
> tree was deleted. The prior TS-slice report is preserved in git history.

## Run metadata

| Field | Value |
| --- | --- |
| Date | 2026-07-15 |
| Host | Darwin arm64 (macOS), go 1.26.1 (module targets go 1.25), bun 1.3.10, node v22.22.0 |
| Assistant repo | `/Users/scotwells/repos/milo-os/assistant` @ `d5f65b7`; real agent runner wired at `8a17852`, mock totals pinned at `3bb8b5c` |
| Harness commits | `5998855` (build/launcher/golden), `14ec17a` (contextId/gitignore), `b3d4996` (v1.0 card), `5a25893` (v1.0 methods+message), `d5f65b7` (TS retirement) |
| Model mode | **MOCK** (core/consumers); **STUB** upstream via Envoy AI Gateway (gateway leg) |
| Wire | **A2A v1.0** (a2a-go v2.3.1): PascalCase methods, `TASK_STATE_*` enums, `StreamResponse` oneOf, no `kind`/`final`, `supportedInterfaces[]` card |

### A2A v1.0 method-name erratum

`CONTRACT-GO-PORT.md` originally stated the JSON-RPC method strings were
"unchanged from our TS (`message/send`, `message/stream`, `tasks/get`,
`tasks/cancel`)". That is **incorrect** for a2a-go v2.3.1 / A2A v1.0 — the real
methods are PascalCase (`SendMessage`, `SendStreamingMessage`, `GetTask`,
`CancelTask`; verified against `internal/jsonrpc/jsonrpc.go` and the service's
green httptest suite). The raw-POST driver was updated accordingly (`5a25893`);
the contract carries the erratum. The old method names now return `-32601`
MethodNotFound — this is what breaks the not-yet-updated portal client (C2).

## How to reproduce

```
e2e/run-e2e.sh go-build     # build cmd/assistant + cmd/patch
e2e/run-e2e.sh full         # core leg: boot StreamCo + sink + Go service, run the A2A driver + sink golden byte-diff
e2e/run-e2e.sh consumers    # consumers leg: patch CLI (portal sub-leg is a v1.0 follow-up)
e2e/run-e2e.sh gateway      # gateway leg: Envoy AI Gateway env + 6 proofs on the shared kind cluster
e2e/run-e2e.sh repo-tests   # go vet ./... + go test ./...
```

---

## Core leg — **PROVEN** (driver 24/24 + sink golden byte-match)

Command: `e2e/run-e2e.sh full` (Go binary, `MODEL_MODE=mock`,
`CAPABILITY_DOCS_FIXTURE=e2e/fixtures/agent-bindings.fixture.json`).

### Item 2 — agent card (A2A v1.0 shape)

```
PASS  card.name = "Patch"
PASS  card advertises A2A v1.0 (supportedInterfaces[].protocolVersion = "1.0") — interfaces=1
PASS  card advertises a JSONRPC interface binding — protocolBinding=JSONRPC
PASS  card.capabilities.streaming = true
PASS  card advertises HTTP bearer security scheme — securitySchemes.bearer.httpAuthSecurityScheme.scheme=bearer
PASS  card.skills includes project-assistant
PASS  card JSONRPC interface url drives the a2a calls — http://127.0.0.1:7820/a2a
```

### Item 3 — auth matrix (HTTP status)

```
PASS  no token -> 401
PASS  valid token, unauthorized project -> 403
PASS  good token, granted project -> 200
```

### Item 4 — SendStreamingMessage lifecycle + GetTask + CancelTask

```
PASS  message/stream responds text/event-stream (status 200), events=19
PASS  stream shows a working (non-terminal) state (TASK_STATE_WORKING)
PASS  stream reaches terminal state (TASK_STATE_COMPLETED)
PASS  stream is A2A v1.0-shaped (oneOf StreamResponse, no kind/final) — oneOf=true noKind=true noFinal=true
PASS  artifact/message text surfaces canned findings — [CONSUMER_LAG, vod-transcode, p-1, runbooks/lag.md]
PASS  a taskId is observable in the stream
PASS  GetTask retrieves the task record — status=200 sameId=true completed=true
PASS  CancelTask behaves sanely — JSON-RPC -32002 "task in non-cancelable state TASK_STATE_COMPLETED"
```

(Mock inference completes fast, so a fresh task is already terminal at cancel
time; the service correctly returns the A2A not-cancelable error, not a 5xx.)

### Item 5 — real MCP round-trip + usage at the sink

```
PASS  provider tool call went over real MCP (StreamCo log: tools/call pipeline_diagnose id=p-1)
PASS  sink captured tool-invocations meter (service=streaming.streamco.example, subject=projects/demo-project) — 3/3
PASS  sink captured token meters (input/output-tokens, resource.kind=Conversation) — 6/6
```

### Sink CloudEvent golden — **BYTE-IDENTICAL**

The golden was recorded from the **TS** emitter (mock model) before deletion and
volatile fields (ULID `id`, `time`, `contextId`) normalized. The Go emitter's
sink events byte-match it:

```
GOLDEN MATCH — 4 canonical event(s) byte-identical to golden/sink-cloudevents.golden.jsonl
```

The 4 canonical per-turn events: `input-tokens`="84", `output-tokens`="46"
(multi-step total — the mock makes 2 model calls at 42/23, aggregated per loop
rule 4), `messages`="1", `tool-invocations`="1" (dimension `service`), all with
`source=<PUBLIC_BASE_URL>/a2a`, `subject=projects/demo-project`, int64-string
values, `resource.kind=Conversation`.

### Item 6 — repo suites

```
go vet ./...    → clean
go test ./...   → ok (14 packages; internal/logger has no test files)
```

**Driver total: `DRIVER PASS — 24/24 checks passed`** (was 23/23 in TS; +1 for
the added "wire is v1.0-shaped" assertion).

---

## Consumers leg

Command: `e2e/run-e2e.sh consumers`.

### C1 — CLI consumer (`patch`) — **PROVEN (9/9)**

```
PASS  [cli.card]            patch card exits 0 and shows the Patch card
PASS  [cli.card.json]       patch card --json is a valid A2A v1.0 AgentCard (supportedInterfaces/JSONRPC/nested bearer)
PASS  [cli.chat.exit]       patch chat exits 0
PASS  [cli.chat.findings]   streams StreamCo findings — [CONSUMER_LAG, vod-transcode, p-1, runbooks/lag.md]
PASS  [cli.sink.onecontext] sink events for the turn share exactly one contextId
PASS  [cli.sink.toolinvocation] sink has tool-invocation (service=streaming.streamco.example, subject=projects/demo-project)
PASS  [cli.sink.tokens]     sink has input/output-token meters for the turn
PASS  [cli.json]            patch chat --json prints parseable A2A events with a contextId
PASS  [cli.badtoken]        bad token → exit 1 "patch: unexpected HTTP status: 401 Unauthorized"
```

The CLI is the built `cmd/patch` (on `a2aclient`). C1 independently demonstrates
the "portal is just a client, the SERVICE meters" architecture: the sink events
for the CLI turn are all service-sourced.

### C2 — portal client-mode — **NOT PROVEN (documented v1.0 follow-up)**

The cloud-portal thin client (`feat/portal-assistant-client`) is still
**v0.3-shaped**: it sends an old JSON-RPC method name and the v1.0 Go service
returns `-32601 METHOD_NOT_FOUND`, so the portal shows "Patch hit a snag
reaching the assistant service" and the integration test fails.

```
WARN  assistant.client.service_error {"status":200,"error":{"code":-32601,"message":"method not found",...,"reason":"METHOD_NOT_FOUND"}}
FAIL  streams a StreamCo pipeline diagnosis end to end — Received: "Patch hit a snag reaching the assistant service…"
```

This is the expected **portal v1.0 translator follow-up** (update method names to
`SendMessage`/`SendStreamingMessage` + v1.0 SSE parsing), not a Go-service
regression. The Go service's own httptest suite and the C1 CLI prove the wire is
correct.

### C3 — no double metering — **NOT RUN this port**

C3 asserts that during a portal conversation the portal emits **zero** usage and
only the service meters. It requires a working v1.0 portal conversation (C2), so
it cannot be soundly run until the portal translator is updated. (Note: an
earlier run appeared to "pass 5/5", but that was **stale git-tracked
`e2e/out/` evidence** dated 3 days prior — the portal test failure aborts the
leg under `set -e` before C3 runs. Caught via timestamps; `e2e/out/` has since
been untracked. See "Harness notes".) C1's sink correlation independently
confirms service-only metering.

---

## Gateway leg — **PROVEN (G1–G5, G7; G6 NOT RUN)**

Command: `e2e/run-e2e.sh gateway`. Shared **kind cluster** (`test-infra`),
Envoy Gateway v1.8.1 + **Envoy AI Gateway v1.0.0**, **STUB** upstream
(`patch-stub-v1`, OpenAI-compatible, deterministic usage). Service in
`MODEL_MODE=gateway`, driven via the `patch` CLI. Guardrails honored: no
`cluster-down`, additive install only, resource preflight, baseline snapshot +
byte-restore diff.

### G1 — cluster bootstrap + clean teardown — **PROVEN**

```
✓ Resource preflight: node memory requests committed = 32%   (< 80% abort threshold)
✓ Envoy Gateway + merged gateway ready; Envoy AI Gateway v1.0.0 installed
✓ test-infra working tree byte-clean
RESTORE PASS — cluster matches pre-install baseline (out/cluster-restore.diff empty)
```

### G2 — CLI chat end-to-end through the gateway — **PROVEN (3/3)**

```
PASS  [gw.chat.exit]     patch chat (gateway mode) exits 0
PASS  [gw.chat.findings] streams StreamCo findings (service→gateway→stub + MCP via gateway) — [CONSUMER_LAG, vod-transcode, p-1]
PASS  [gw.chat.context]  contextId=019f67d0-d55d-7b65-93b3-7553bace012d
```

### G3 — gateway-counted tokens + attribution — **PROVEN (4/4), EXACT match**

```
PASS  [gw.tokens.record]      gateway access log has 2 llm records for the conversation
PASS  [gw.tokens.attribution] x_datum_project=demo-project, x_datum_agent=patch on every record
PASS  [gw.tokens.present]     gateway counted input=464 output=96 total=560
PASS  [gw.tokens.equal_sink]  gateway 464/96 == sink 464/96  (delta in=0 out=0)
```

Gateway-counted (tamper-independent) and service self-reported token totals match
**exactly**. This is the proof that caught the multi-step under-billing bug in
the TS slice; the fix carried over to the Go loop (usage aggregated per step —
the core-leg golden also pins it).

### G4 — credential isolation — **PROVEN (1/1)**

```
ok: no model API key in the gateway-mode boot env
PASS  [gw.cred.direct401] direct-to-stub WITHOUT the gateway-injected key → 401
      body: "missing or invalid api key — the gateway BackendSecurityPolicy must inject it"
```

### G5 — gateway-enforced allow-list (MCPRoute toolSelector) — **PROVEN (4/4)**

```
PASS  [gw.allowlist.control]      direct StreamCo = 4 tools [pipeline_diagnose, streams_delete, streams_get, streams_list]
PASS  [gw.allowlist.included]     gateway tools/list = [streamco-backend__pipeline_diagnose, streamco-backend__streams_get, streamco-backend__streams_list]
PASS  [gw.allowlist.excluded]     gateway reachable AND does NOT expose streamco-backend__streams_delete
PASS  [gw.allowlist.call_blocked] calling it through the gateway → rejected "invalid tool name: streams_delete"
```

### G6 — token-budget 429 (stretch) — **NOT RUN**

Not run this port: the run did not pass `--with-ratelimit` (default off). The
proof was PROVEN in slice 4 (`df6d33b`: a 40-token/hour `BackendTrafficPolicy`
keyed on `x-datum-project`, 429 on exhaustion, per-consumer isolation). Re-run
with `GW_WITH_RATELIMIT=1 e2e/run-e2e.sh gateway` to reconfirm against the Go
gateway path.

### G7 — repo suites with the new seams — **PROVEN**

```
go vet ./...  → clean;  go test ./...  → ok (all packages)
```

---

## Summary verdict

| Leg / proof | Status |
| --- | --- |
| Core — driver (items 2–5) | **PROVEN** (24/24) |
| Core — sink CloudEvent golden | **PROVEN** (byte-identical to the TS emitter) |
| Core — repo suites (item 6) | **PROVEN** (go vet + go test) |
| Consumers C1 — CLI | **PROVEN** (9/9) |
| Consumers C2 — portal client-mode | **NOT PROVEN** — v0.3 translator → -32601; v1.0 follow-up |
| Consumers C3 — no double metering | **NOT RUN** — depends on C2; C1 shows service-only metering |
| Gateway G1 — bootstrap + clean teardown | **PROVEN** (byte-restore diff empty) |
| Gateway G2 — CLI chat through gateway | **PROVEN** (3/3) |
| Gateway G3 — gateway tokens == sink | **PROVEN** (464/96 exact, attribution) |
| Gateway G4 — credential isolation | **PROVEN** (1/1) |
| Gateway G5 — MCPRoute allow-list | **PROVEN** (4/4) |
| Gateway G6 — token-budget 429 (stretch) | **NOT RUN** — slice-4 `df6d33b` reference |
| Gateway G7 — repo suites | **PROVEN** |

## Honest gaps / caveats

- **Model was mocked / stubbed.** Answer quality, real tool selection, and real
  token accounting are not exercised — only `MODEL_MODE=anthropic` (real key)
  covers those. Mock token counts are fixed (84/46 total); the gateway stub
  returns deterministic usage (464/96). The meter *plumbing, wire shape, and
  gateway↔sink reconciliation* are proven, not real token accounting.
- **Portal client-mode (C2) and no-double-metering (C3)** are pending the
  portal v1.0 translator follow-up (see Follow-ups in the service README).
- **G6 (429 budget)** was not run this port (stretch; proven in slice 4).
- **OIDC auth mode** is covered by the service's Go unit tests, not this HTTP
  suite (item 3 covers `AUTH_MODE=dev`).

## Harness notes

- **Stale git-tracked `e2e/out/` (fixed).** The prior slice committed per-run
  evidence under `e2e/out/`; `git checkout` restored it and a mid-run read
  briefly mistook 3-day-old files for fresh output (a would-be false C3 pass,
  caught via timestamps). `e2e/out/` was untracked in `d5f65b7` (it was already
  gitignored). Always regenerate `e2e/out/` from a live run.
- **Wire verification.** Every v1.0 assertion (methods, `TASK_STATE_*` enums,
  oneOf `StreamResponse`, no `kind`/`final`, `supportedInterfaces[]` card,
  nested `httpAuthSecurityScheme`, `ROLE_USER`/`{text}` message) was verified
  against a2a-go v2.3.1 source, then against the running binary.
