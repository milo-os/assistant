# Assistant Service — E2E Confirmation Report

Owner: assistant-qa (platform QA). Contract:
scratchpad `CONTRACT-ASSISTANT.md`, section "Definition of done / QA".

Rule: every item below is either **PROVEN** with commands + trimmed output, or
marked **NOT PROVEN** with a concrete reason. No item is green without captured
evidence from an actual run against the REAL running service over HTTP with a
dev bearer token. Honesty over green.

> This report is the unfinished business of the prior slice: in the previous
> engagement the real chat path was **NOT PROVEN** because the portal assistant
> was cookie-auth-bound with no scriptable API surface. This slice proves it
> against the standalone bearer-token service — the full path (auth → agent
> loop → real MCP tool call → metering → SSE) is exercised end to end.

## Run metadata

| Field | Value |
| --- | --- |
| Date | 2026-07-11 |
| Host | Darwin arm64 (macOS), node v22.22.0, bun 1.3.10 |
| Assistant repo | `/Users/scotwells/repos/datum-cloud/assistant` @ `7ef1137` ("feat(auth): add SubjectAccessReviewAuthorizer as a named production stub"). Also run green verbatim on parent `bf7896c`; `7ef1137` is `src/auth`-only, not wired in → zero behavior change. |
| Boot command | `PORT=7820 AUTH_MODE=dev AUTH_DEV_TOKENS='e2e-token:e2e-user:demo-project;wrong-token:other:other-project' MODEL_MODE=mock AGENT_BINDINGS_FIXTURE=<repo>/e2e/fixtures/agent-bindings.fixture.json USAGE_GATEWAY_URL=http://127.0.0.1:7811 PUBLIC_BASE_URL=http://127.0.0.1:7820 bun run start` |
| Model mode | **MOCK** (`MODEL_MODE=mock`, model id `patch-mock-v0`; no `ANTHROPIC_API_KEY` in env and none in the repo — only `.env.example`) |
| StreamCo demo provider | `e2e/streamco` (lifted verbatim from service-catalog `feat/agent-framework-e2e`) @ `127.0.0.1:7810` |
| Usage capture sink | `e2e/sink` (lifted verbatim from same branch) @ `127.0.0.1:7811` |
| Bindings fixture | `e2e/fixtures/agent-bindings.fixture.json` (round-trips through the REAL portal schema — see item 1) |
| Chat path | **REAL** — A2A JSON-RPC `message/stream` over HTTP (no composition-harness fallback) |

**Model note:** all inference was served by the service's scripted mock model
(contract §Agent loop). The mock calls the first `pipeline_diagnose` provider
tool and folds its result into the final text, so the FULL chat path (auth →
agent loop → real MCP tool call → metering → SSE) is exercised; only the LLM's
token *generation* is faked (it reports fixed input=42/output=23). Real-model
parity is a documented risk in the service README. A real-model pass was NOT run
(no key available); see the appendix.

## How to reproduce

```
e2e/run-e2e.sh all        # selftests + full driver + repo tests (default)
e2e/run-e2e.sh selftests  # StreamCo + sink standalone selftests
e2e/run-e2e.sh full       # boot StreamCo + sink + assistant, run the A2A driver (items 2-5)
e2e/run-e2e.sh repo-tests # bun test + typecheck in the assistant repo (item 6)
```

Raw evidence for each run lands in `e2e/out/` (gitignored): `agent-card.json`,
`message-send.json`, `stream-events.jsonl`, `stream-meta.json`, `tasks-get.json`,
`tasks-cancel.json`, `sink-events.json`, `driver-summary.json`, `streamco.log`,
`assistant.log`, `repo-bun-test.log`, `repo-typecheck.log`.

## Checklist

### 1. Boot service (mock model, fixture bindings, StreamCo + sink)

Status: **PROVEN**

The service booted on `:7820` and served `/healthz` while StreamCo (`:7810`) and
the sink (`:7811`) were up; bindings came from the schema-validated fixture;
model mode was mock.

Command: `e2e/run-e2e.sh full`

Evidence (trimmed, from run-e2e.sh output):

```
==> Booting StreamCo demo provider on http://127.0.0.1:7810
ok: streamco healthy at http://127.0.0.1:7810
==> Booting usage capture sink on http://127.0.0.1:7811
ok: sink healthy at http://127.0.0.1:7811
==> Booting assistant service on http://127.0.0.1:7820 (cmd: bun run start)
NOTE: model mode: mock
ok: assistant healthy at http://127.0.0.1:7820
```

Fixture validated against the REAL portal composition schema (extracted verbatim
from cloud-portal `feat/patch-dynamic-composition`: `types.ts` +
`fixture-source.ts`) by round-tripping through the actual `FixtureAgentBindingSource`:

```
PASS  item[0] passes agentBindingSchema.safeParse — ok
PASS  spec.serviceName — streaming.streamco.example
PASS  mcp endpoint — http://127.0.0.1:7810/mcp
PASS  toolSelector.include — ["streams_list","streams_get","pipeline_diagnose"]
PASS  knowledge source urls on :7810 — .../llms-full.txt, .../runbooks/lag.md
FIXTURE VALID (round-trips through real portal schema)
```

Note: the service also ships a sample `fixtures/agent-bindings.json` at repo
root; the e2e run uses the QA fixture at `e2e/fixtures/agent-bindings.fixture.json`.

### 2. GET agent card: valid A2A v1.0 shape, streaming=true, bearer scheme

Status: **PROVEN**

Command: `GET http://127.0.0.1:7820/.well-known/agent-card.json` (via driver; card
also drives endpoint discovery — the driver POSTs to `card.url`).

Evidence (trimmed, from `out/agent-card.json` + driver console):

```
PASS  card.name = "Patch"
PASS  card.protocolVersion = "1.0"
PASS  card.capabilities.streaming = true
PASS  card.provider is Datum — {"organization":"Datum","url":"https://www.datum.net"}
PASS  card advertises HTTP bearer security scheme — {"bearer":{"type":"http","scheme":"bearer",...}}
PASS  card.skills includes project-assistant
PASS  card.url points to JSON-RPC endpoint — http://127.0.0.1:7820/a2a
```

### 3. AuthN matrix: no token → 401; wrong-project token → 403; good token → 200

Status: **PROVEN**

`AUTH_DEV_TOKENS='e2e-token:e2e-user:demo-project;wrong-token:other:other-project'`
(format `token:subject:projects` per `src/auth/dev.ts`). Requests are
`POST /a2a message/send` with `metadata.projectName=demo-project`.

Evidence:

```
PASS  no token -> 401           (POST /a2a, no Authorization header)
PASS  valid token, unauthorized project -> 403   (Bearer wrong-token; grants only other-project)
PASS  good token, granted project -> 200         (Bearer e2e-token)
```

### 4. message/stream lifecycle + tasks/get + tasks/cancel

Status: **PROVEN**

Prompt: `"Diagnose pipeline p-1 for StreamCo"` (project `demo-project`, Bearer
`e2e-token`). SSE response is `text/event-stream`; the observed frame sequence
(20 events) was:

```
task            (status.state = submitted)
status-update   (state = working,   final = false)
artifact-update × 17  (artifactId "response", streamed text chunks)
status-update   (state = completed, final = true)   ← stream closes after this
```

Assertions:

```
PASS  message/stream responds text/event-stream (status 200)
PASS  stream shows a working (non-terminal) state
PASS  stream reaches terminal state completed
PASS  stream signals final=true at terminal
PASS  artifact/message text surfaces canned findings — matched: [CONSUMER_LAG, vod-transcode, p-1, runbooks/lag.md]
PASS  a taskId is observable in the stream
PASS  tasks/get retrieves the task record — status=200 sameId=true completed=true
PASS  tasks/cancel behaves sanely — 200 JSON-RPC error -32002:
      "Task <id> is in terminal state \"completed\" and cannot be canceled"
```

Artifact text (head), confirming the tool result was folded into the answer:

```
Ran the pipeline diagnosis. The provider tool reported: { "id": "p-1", "pipeline
```

Note on tasks/cancel: mock inference completes fast, so a fresh task is already
terminal by cancel time; the service correctly returns the A2A "not cancelable"
JSON-RPC error (`-32002`) rather than a 5xx. Cancel of a still-running task is
covered by the service's own unit tests (`bun test`, item 6).

### 5. PROVE the previously-unprovable chat path (real MCP + usage at sink)

Status: **PROVEN** — this is the item that was NOT PROVEN in the prior slice.

**Real MCP round-trip** — StreamCo's own request log recorded the tool call the
chat turn triggered (Streamable HTTP MCP; the tool executed server-side):

```
[streamco] 2026-07-11T21:43:56.749Z tools/call pipeline_diagnose id=p-1
```

**Usage events landed at the sink.** One `tool-invocations` CloudEvent, dimensioned
by the provider service name:

```json
{"type":"assistant.miloapis.com/conversation/tool-invocations",
 "subject":"projects/demo-project","source":"http://127.0.0.1:7820/a2a",
 "data":{"value":"1","dimensions":{"service":"streaming.streamco.example"},
   "resource":{"group":"assistant.miloapis.com","kind":"Conversation","namespace":"default","name":"<contextId>"}}}
```

Token meters (mock reports fixed nonzero counts, model `patch-mock-v0`):

```json
{"type":"assistant.miloapis.com/conversation/input-tokens",
 "subject":"projects/demo-project",
 "data":{"value":"42","dimensions":{"model":"patch-mock-v0"},
   "resource":{"group":"assistant.miloapis.com","kind":"Conversation","name":"<contextId>"}}}
```

Sink event tally for the run (3 completed diagnose turns during the driver pass):

```
   3 assistant.miloapis.com/conversation/input-tokens
   3 assistant.miloapis.com/conversation/messages
   3 assistant.miloapis.com/conversation/output-tokens
   3 assistant.miloapis.com/conversation/tool-invocations
```

Assertions:

```
PASS  provider tool call went over real MCP (StreamCo log shows pipeline_diagnose id=p-1)
PASS  sink captured tool-invocations meter (service=streaming.streamco.example, subject=projects/demo-project)
PASS  sink captured token meters (input/output-tokens, subject=projects/demo-project, resource.kind=Conversation)
```

Caveat: token counts are mock-fixed (42/23), not real provider usage — the meter
*plumbing and wire shape* are proven, not real token accounting (needs a real key).

### 6. Full `bun test` + typecheck green in the repo

Status: **PROVEN**

Command: `e2e/run-e2e.sh repo-tests` (from repo root: `bun test`; `tsc --noEmit`).

Evidence:

```
==> Assistant repo: bun test
 73 pass
 0 fail
Ran 73 tests across 8 files.

==> Assistant repo: typecheck        (tsc --noEmit — no diagnostics, exit 0)
```

Note: the service typecheck uses `tsconfig.json` `include: ["src"]`, which
intentionally excludes the QA-owned `e2e/` harness (StreamCo uses Node
type-stripping `.ts` import extensions that the strict service tsconfig rejects).
The harness is verified by EXECUTION under `node`/`bun` (StreamCo + sink selftests
pass; the driver runs), not by `tsc`. One honest caveat: the lifted-verbatim
`e2e/streamco/src/selftest.ts` carries a pre-existing type-nit under a standalone
strict `tsc` (`Response.json()` is typed `unknown` under Node types, assigned to a
typed local — `TS2322`); it does not affect the selftest's runtime, which passes.
See "Bugs / integration notes" below.

## Summary verdict

| Item | Status |
| --- | --- |
| 1. Boot (mock model, fixture, StreamCo + sink) | **PROVEN** |
| 2. Agent card (A2A v1.0, streaming, bearer) | **PROVEN** |
| 3. Auth matrix (401 / 403 / 200) | **PROVEN** |
| 4. Stream lifecycle + tasks/get + tasks/cancel | **PROVEN** |
| 5. Real chat path (MCP round-trip + usage at sink) | **PROVEN** |
| 6. bun test + typecheck | **PROVEN** |

Driver totals: `{"checks":23,"passed":23,"failedRequired":0,"failedOptional":0}`.

## Bugs / integration notes (with owner)

- **tsconfig included the QA harness (owner: shared — resolved by engineer).** The
  committed `tsconfig.json` at `f6dccff` had `include: ["src", "e2e"]`, so the
  service `tsc --noEmit` tried to compile `e2e/streamco/*.ts` (Node
  type-stripping, `.ts` import extensions) and failed (`TS5097`, `TS2322`). The
  engineer's `bf7896c` changed it to `include: ["src"]` with a comment noting the
  e2e harness is typechecked separately. Item 6 is green because of that fix. No
  product-code bug.
- **Harness teardown left orphaned server processes (owner: QA harness — fixed).**
  The runner tracked each server's launching subshell PID, but the actual Node
  listener (and `bun run start`'s forked child) is a grandchild that a plain
  `kill` on the subshell did not reap, leaking listeners on 7810/7820 across
  runs. Fixed by adding a port-based backstop in `cleanup()` (kills whatever
  still LISTENs on the ports the runner booted). Verified: ports free after each
  run.
- **Lifted StreamCo `selftest.ts` type-nit (owner: QA harness — left verbatim).**
  Under a standalone strict `tsc`, `e2e/streamco/src/selftest.ts:150` errors
  (`TS2322`: Node's `Response.json()` is `unknown`). It is a pre-existing nit in
  the verbatim-lifted demo asset, does not affect runtime (the selftest is run via
  Node type-stripping and passes), and is outside the service typecheck scope. Left
  byte-identical to the source to preserve provenance; a one-line cast would clear
  it if tsc coverage of the harness is ever wanted (or add `e2e/tsconfig.json`).
- No product-code (service) bugs were found. The auth refactor (`bf7896c`,
  Authorizer seam) and the `SubjectAccessReviewAuthorizer` stub (`7ef1137`) pass
  the full auth matrix and chat path unchanged.

## Honest gaps / caveats

- **Model was mocked.** Answer quality, real tool-selection, and real token
  accounting are NOT exercised — only `MODEL_MODE=anthropic` covers those. Token
  meters carried fixed mock counts (42/23), not provider usage.
- **OIDC auth mode not e2e-tested here.** Item 3 covers `AUTH_MODE=dev` only;
  OIDC is covered by the service's unit tests (`bun test`), not this HTTP suite.
- **tasks/cancel of a running task** is asserted only via the terminal-state
  path (mock completes too fast to catch mid-flight); in-flight cancellation is
  covered by the service's unit tests.

## Appendix: real-model pass

Status: **N/A — no `ANTHROPIC_API_KEY` in the environment or the repo.**

The runner passes `ANTHROPIC_API_KEY`/`ANTHROPIC_MODEL` through to the service
when present; re-run `MODEL_MODE=anthropic e2e/run-e2e.sh full` with a key to add
a real-model pass. Expected delta: token meters carry real provider counts and
the final answer is model-generated (the six structural assertions are unchanged).

---

# Consumers (task #12 / CONTRACT-CONSUMERS.md Workstream C)

Proves the "portal is just a client" architecture: independent consumers drive the
SAME live assistant service, and metering is emitted by the SERVICE ONLY (no double
billing). Run: `e2e/run-e2e.sh consumers` (boots StreamCo + sink + service, mock
model; the portal sub-leg auto-skips until `feat/portal-assistant-client` + its
test command exist). Driver: `e2e/driver/consumers-checks.mjs`.

Tested: assistant repo @ `7af7467` (CLI `b569338`) against portal branch
`feat/portal-assistant-client` @ `37ae4615`; model **MOCK**.

## C1. CLI consumer (`patch`) — **PROVEN**

CLI invoked as `bun run <repo>/cli/main.ts` with `PATCH_URL=http://127.0.0.1:7820`,
`PATCH_TOKEN=e2e-token`. Assistant answer streams to STDOUT; status transitions to
STDERR; exit 0 on completed, non-zero on auth error.

```
PASS  [cli.card]            `patch card` exits 0 and shows the Patch card
PASS  [cli.card.json]       `patch card --json` is a valid A2A AgentCard — name=Patch pv=1.0 url=.../a2a streaming=true bearer=bearer skill=project-assistant
PASS  [cli.chat.exit]       `patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project` exits 0
PASS  [cli.chat.findings]   streams StreamCo findings to stdout — matched [CONSUMER_LAG, vod-transcode, p-1, runbooks/lag.md]
PASS  [cli.sink.onecontext] sink events for the turn share exactly one contextId
PASS  [cli.sink.toolinvocation] sink has tool-invocation (service=streaming.streamco.example, subject=projects/demo-project)
PASS  [cli.sink.tokens]     sink has input/output-token meters for the turn
PASS  [cli.json]            `patch chat --json` prints parseable A2A events with a contextId
PASS  [cli.badtoken]        bad token → non-zero exit with a clear error
CONSUMERS CLI PASS — 9/9 checks passed
```

`patch card` (stdout):

```
Patch  (A2A protocol 1.0, v0.1.0)
Endpoint:   http://127.0.0.1:7820/a2a  [JSONRPC]
Streaming:  yes   Auth: http bearer   Skills: project-assistant
```

`patch chat` — answer on STDOUT (trimmed), status transitions on STDERR:

```
Ran the pipeline diagnosis. The provider tool reported: { "id": "p-1",
  "pipeline": "vod-transcode", "findings": [ { "code": "CONSUMER_LAG", ... } ],
  "recommendation": "... Runbook: http://127.0.0.1:7810/runbooks/lag.md" } ...
--- STDERR ---  » task <id> (submitted)   » working   » completed
```

Bad token (exit 1, clear stderr, no stack trace):

```
patch: unauthorized: Unknown or invalid bearer token (check PATCH_TOKEN / --token) [HTTP 401]
```

## C2. Portal thin-client consumer (client mode) — **PROVEN**

From a read-only `git worktree` of `feat/portal-assistant-client` @ `37ae4615`
(`bun install --frozen-lockfile`), portal-engineer's real-service integration test
(item A4) ran against the LIVE service:

```
ASSISTANT_SERVICE_E2E_URL=http://127.0.0.1:7820 ASSISTANT_SERVICE_E2E_TOKEN=e2e-token \
ASSISTANT_SERVICE_E2E_PROJECT=demo-project ASSISTANT_SERVICE_E2E_CONVERSATION=e2e-portal-conv-1 \
  bun test app/modules/assistant/client/route.e2e.test.ts
→ [INFO] assistant client-mode request {"userId":"e2e-user","projectId":"demo-project","conversationId":"e2e-portal-conv-1","serviceUrl":"http://127.0.0.1:7820"}
→ 1 pass, 0 fail  (drives the real /api/assistant route in client mode; asserts
   200 + x-vercel-ai-ui-message-stream:v1, reassembles text-deltas, requires the
   streamed text match the StreamCo findings, FAILS on the degradation line)
```

Scoped portal suites (`bun test app/modules/assistant app/modules/usage`):
**55 pass, 1 skip, 0 fail** (the 1 skip is the A4 test with no live URL in that run).

Portal branch **typecheck**: `bun run typecheck` (`react-router typegen && tsc`)
exit 0 from the clean frozen worktree @ `37ae4615` — **green** in the same
environment that caught the earlier clean-install failure (fixed by
portal-engineer; see "Consumers bugs / notes" below).

## C3. No double metering — **PROVEN**

During the C2 portal conversation, the sink (wiped first) captured only
SERVICE-emitted usage — the portal emitted nothing:

```
PASS  [portal.sink.nonempty]      sink captured usage events (service emitted) — 4 events
PASS  [portal.meter.serviceonly]  ALL from the SERVICE — sources={"http://127.0.0.1:7820/a2a":4}
PASS  [portal.meter.noportalemit] portal emitted ZERO (no source contains /api/assistant or cloud-portal)
PASS  [portal.meter.contextid]    all events carry the pinned contextId — ["e2e-portal-conv-1"]
PASS  [portal.meter.nodupes]      no duplicate (meter,contextId,value) rows
CONSUMERS NODEMETER PASS — 5/5 checks passed
```

Assertion is sensitive, not vacuous: in the QA oracle self-test, injecting one
portal-sourced (`/api/assistant`) event makes it FAIL as intended.

## Consumers verdict

| Item | Status |
| --- | --- |
| C1. CLI consumer | **PROVEN** (9/9) |
| C2. Portal client-mode | **PROVEN** — live A4 integration test + scoped suites + typecheck all green |
| C3. No double metering | **PROVEN** (5/5) — portal emits zero; service-only, single contextId |

Model MOCKED (same caveat as the base report — plumbing/wire proven, not real token
accounting or answer quality).

### Consumers bugs / notes (with owner)

- **CLI: none.** The `patch` CLI matched the engineer's documented surface
  byte-for-byte (9/9).
- **Portal branch typecheck failed from a clean install — FIXED (owner: portal).**
  Initially, `bun run typecheck` on `feat/portal-assistant-client` @ `3a0c173f`, run
  in a fresh `bun install --frozen-lockfile` worktree (bun 1.3.10), reported 2 errors,
  both `typeof fetch` missing `preconnect` (`TS2352`/`TS2741`) — in the pre-existing
  `app/modules/graphql/client.ts:28` (`buildAuthFetch(): typeof fetch`) and a test's
  fetch-mock cast (`handler.test.ts`). Root cause: the branch's `typeof fetch` (via
  its type-dep pins) requires a `preconnect` member the pre-existing code didn't
  provide; the discrepancy with portal-engineer's local `exit 0` was a genuine
  resolved-type-version difference (their locked `@types/node`/TS did not carry
  `preconnect`), NOT stale `node_modules`. NONE of the client-mode RUNTIME files
  errored. QA flagged it with the exact repro; portal-engineer fixed it in
  `37ae4615` (`as unknown as typeof fetch` on the three sites, type-only, zero
  runtime). **Re-verified**: clean frozen worktree @ `37ae4615`, `bun run typecheck`
  exit 0 in the same environment that caught the failure. Resolved.
- **No double-metering caveat:** proven with the mock model; token *counts* are
  mock-fixed, but the emitter *source/count/contextId* attribution — the thing that
  proves single-emission — is real.

---

# Gateway (task #15 / CONTRACT-GATEWAY.md slice 4)

Proves the production metering/policy path the earlier slices stubbed: model
traffic flows service → Envoy AI Gateway → stub upstream with tokens counted AT
THE GATEWAY (`llmRequestCosts`), provider MCP flows through an `MCPRoute` whose
toolSelector enforces the reviewed allow-list, and the service holds NO upstream
model credential (the gateway's `BackendSecurityPolicy` owns it).

Run: `e2e/run-e2e.sh gateway`. Environment: test-infra kind cluster, **Envoy
Gateway v1.8.1** (test-infra pin) + **Envoy AI Gateway v1.0.0** (Helm) — compatible
per the AI-GW v1.0.x ↔ EG v1.8.1 matrix, no version override needed (infra-engineer
enabled the AI-GW extension via an EG HelmRelease patch, test-infra unmodified).
Model: **STUB** upstream (`patch-stub-v1`, OpenAI-compatible, returns real
deterministic `usage`). Gateway/manifests/stub owned by infra-engineer (`e2e/gateway/`,
commit `86dc3f8`); driver/runner/report/streamco owned by QA.

Endpoints: `GATEWAY_URL=http://localhost:1975/v1`, `MCP=http://localhost:1975/mcp`
(kubectl port-forward); StreamCo + stub run in-cluster (ns `patch-ai-gateway`).

## G1. Cluster bootstrap — repeatable, clean teardown — **PROVEN**

`e2e/gateway/up.sh` (idempotent) brought up the AI Gateway controller + routes;
`down.sh` (on exit) removed our layer. Verified across three runs (up→down→up).

```
==> G1: bring up Envoy AI Gateway env
  ✓ AI Gateway controller ready
  ✓ extension server: ai-gateway-controller.envoy-ai-gateway-system...:1063
...
ok: test-infra working tree byte-clean
```

Teardown scope (honest): `down.sh` removes OUR AI-gateway layer (namespace
`patch-ai-gateway`) and reverts Envoy Gateway to stock, but LEAVES Envoy Gateway
installed (infra-engineer's hardening pass makes full EG removal the default).
So this is "our layer removed, EG reverted to stock," NOT "cluster fully restored."
test-infra's own working tree stayed byte-clean (only its gitignored kubeconfig
is written).

## G2. CLI chat end-to-end through the gateway — **PROVEN**

`patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project` with the
service in `MODEL_MODE=gateway` → service → gateway → stub LLM (+ MCP via the
gateway MCPRoute) → findings on stdout, exit 0.

```
PASS  [gw.chat.exit]     patch chat (gateway mode) exits 0 — exit=0
PASS  [gw.chat.findings] streams StreamCo findings — matched [CONSUMER_LAG, vod-transcode, p-1]
PASS  [gw.chat.context]  captured contextId=7b68ff8b-d5fe-449b-809f-a03392608242
GATEWAY CHAT PASS — 3/3
```

## G3. Gateway-counted tokens + attribution — **PROVEN (and it caught a real billing bug)**

Verified by team lead with the corrected capture (resolve the RUNNING envoy pod via
`-l gateway.envoyproxy.io/owning-gateway-name=patch-ai-gateway`, `kubectl logs
--all-containers` after a ~12s flush wait, filter `"log.type":"llm"`, match
`x_datum_conversation`). Evidence preserved in `e2e/out/`.

**Pre-fix run** (conversation `0f34187e…`, records in
`g3-llm-records-lead-verified.jsonl`): the gateway counted 2 model calls —
in 189+274=**463**, out 1+94=**95** — every record carrying
`x_datum_project=demo-project`, `x_datum_conversation=<contextId>`,
`x_datum_agent=patch`. The service's sink events reported only **274/94**: the
tool-call step's usage was silently dropped. **The cross-check found a real
under-billing defect** — the loop read streamText's final-step `usage` instead of
`totalUsage` — fixed in `5c1bc7f` (with a fail-before/pass-after regression test);
the same pattern was found and fixed on the cloud-portal branches, and a
`fix/assistant-usage-undercount` branch was prepared off portal main.

**Post-fix run** (conversation `5df8ae3c…`, records in
`g3-llm-records-postfix-exactmatch.jsonl`, sink dump in
`g3-sink-events-postfix.json`):

```
GATEWAY RECORDS=2
gateway: input=463 output=95   (x_datum_* attribution asserted on every record)
sink:    input=463 output=95   (events=4)
```

Gateway-counted and service-reported totals now match EXACTLY. The gateway is the
authoritative, tamper-independent count; service self-reporting is reconciliation.

## G4. Credential isolation — **PROVEN**

Service booted in gateway mode with NO model API key (`ok: no model API key in the
gateway-mode boot env`), chat still succeeded (G2), and a direct call to the stub
without the gateway-injected key was rejected:

```
PASS  [gw.cred.direct401] direct-to-stub WITHOUT the gateway-injected key → 401
      body: "missing or invalid api key — the gateway BackendSecurityPolicy must inject it"
```

## G5. Gateway-enforced allow-list (MCPRoute toolSelector) — **PROVEN**

```
PASS  [gw.allowlist.control]      direct StreamCo = 4 tools [pipeline_diagnose, streams_delete, streams_get, streams_list]
PASS  [gw.allowlist.included]     gateway tools/list = [streamco-backend__pipeline_diagnose, streamco-backend__streams_get, streamco-backend__streams_list]
PASS  [gw.allowlist.excluded]     gateway reachable AND does NOT expose streamco-backend__streams_delete
PASS  [gw.allowlist.call_blocked] calling it through the gateway → rejected "invalid tool name: streams_delete"
GATEWAY ALLOWLIST PASS — 4/4
```

The QA-added 4th StreamCo tool `streams_delete` is visible direct but absent/blocked
through the gateway MCPRoute — the enforcement is real, not StreamCo simply lacking
the tool. Assertions require the gateway to be REACHED (an unreachable gateway fails,
not falsely passes).

## G6. (STRETCH) token-budget 429 — **PROVEN**

Demonstrated by infra-engineer via `up.sh --with-ratelimit` (commit df6d33b):
a `BackendTrafficPolicy` with a 40-token/hour budget keyed on
`x-datum-project`, consuming the gateway-metered `llm_total_token` cost:

```
req1 → 200, req2 → 200, req3-8 → 429
429 headers: x-ratelimit-limit: 40, 40;w=3600 / x-ratelimit-remaining: 0 / x-ratelimit-reset: 840
per-consumer isolation: a DIFFERENT x-datum-project still → 200 (its own budget)
```

Consumer spend caps enforce at request time from the same counts that drive
billing. Redis addon removed on teardown; restore-diff PASS.

## G7. Assistant repo suites green with the new seams — **PROVEN**

```
104 pass / 0 fail (12 files); tsc --noEmit clean
```

## Gateway verdict

| Item | Status |
| --- | --- |
| G1. Cluster bootstrap (repeatable, our-layer teardown clean) | **PROVEN** |
| G2. CLI chat through gateway | **PROVEN** |
| G3. Gateway-counted tokens + attribution | **PROVEN** — post-fix exact match: gateway 463/95 == sink 463/95; cross-check caught + drove the fix of a real under-billing bug (5c1bc7f) |
| G4. Credential isolation | **PROVEN** |
| G5. Gateway-enforced allow-list | **PROVEN** |
| G6. Token-budget 429 (stretch) | **PROVEN** — 40-token budget, 429 on exhaustion, per-consumer isolation (df6d33b) |
| G7. Repo suites green | **PROVEN** |

Bugs found (owner): **QA harness** — (1) `gateway-checks.mjs` top-level MCP-SDK
import broke non-allowlist modes, and `NODE_PATH` is ignored by Node ESM (fixed:
`e2e/package.json` + `node_modules`); (2) allow-list assertions falsely passed on an
unreachable gateway (fixed: reachability precondition); (3) `kubectl port-forward`
processes orphaned on teardown (fixed: port-based cleanup backstop); (4) G3 access-log
capture missed the envoy container and raced the log flush (fixed: resolve running
pod + `--all-containers` + flush retry — recipe in G3). **PRODUCT (service)** —
(5) multi-step token under-billing: only the final streamText step's usage was
emitted; found by the G3 gateway cross-check, fixed in `5c1bc7f`
(`result.totalUsage`), same pattern fixed on cloud-portal branches
(`5dc50454`/`edde99fc`, `516efa81`/`668e4aa7`) and prepared for portal main as
`fix/assistant-usage-undercount` (`678e5c8a`+`16ebac11`, incl. the separate
cache-token-meters-never-emit bug). Honest gaps: model is a STUB (deterministic
usage — real-model token accounting still unproven); G6 stretch NOT RUN; the
scripted driver's capture was validated against real records but the final
exact-match run was executed by the lead interactively, not via run-e2e.sh.
