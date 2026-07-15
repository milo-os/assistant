# Real-Environment Playground — Acceptance Report (slice 6)

Owner: QA (pg-qa). Contract: scratchpad `CONTRACT-REAL-ENV.md`, proofs **P1–P8**.
Gatekeeper rule (inherited from the Go-port report): every proof is either
**PROVEN** with commands + trimmed output, or **NOT PROVEN / PENDING** with a
concrete reason. No proof is green without captured evidence from a live run
against the persistent playground on the shared `test-infra` kind cluster.
**Honesty over green.**

## Run metadata

| Field | Value |
| --- | --- |
| Date | 2026-07-15 |
| Cluster | shared `test-infra` kind cluster (persistent; hosts unrelated ipam/etcd/nats/chainsaw workloads — additive labeled installs only, never `cluster-down`) |
| BASE | assistant + gateway + stub-LLM + StreamCo + sink in-cluster (ns `patch-playground`), assistant keyless (MODEL_MODE=gateway), auth dev-token, gateway = Envoy AI Gateway v1.0.0 |
| Overlay | service-catalog CRDs + controller + capability-provider adapter (ns `agent-framework-playground`), `feat/agent-framework-playground` |
| Consumer | host `patch` CLI → `http://localhost:1986` (port-forward), token `pg-demo-token` → `demo-project` |
| QA contextId | `019f6812-7332-746c-8bb6-11243bf6aa36` (all token/sink assertions scoped to it — the user was concurrently using the env) |
| Driver | `e2e/playground/{run-proofs.sh, playground-checks.mjs, snapshot.sh}` |

## Summary verdict

| Proof | Status |
| --- | --- |
| P1 — bring-up idempotent + preflight-gated | **PENDING** — needs one `up.sh` re-run (bounces the user's live port-forward; coordinating a window with pg-infra) |
| P2 — host chat through the gateway | **PROVEN** (3/3) |
| P3 — live reconfiguration (v1→v2→unpublish) | **PENDING** — assistant is fixture-mode; needs pg-infra to wire `--with-catalog` + a stable-overlay window. Adapter mechanism proven live (see P5). |
| P4 — gateway token attribution + reconciliation | **PROVEN** (4/4, exact) |
| P5 — entitlement isolation | **PROVEN** (3/3) |
| P6 — service-emitted usage at the sink | **PROVEN** (5/5) |
| P7-assistant — go vet + go test | **PROVEN** |
| P7-catalog — envtest | **PROVEN** |
| P8 — playground-down --dry-run == labeled footprint | **PROVEN** (26/26 exact, no teardown) |

## How to reproduce

```bash
export KUBECONFIG=~/repos/datum-cloud/test-infra/kubeconfig
e2e/playground/run-proofs.sh p2   # chat (writes out/playground-contextid.txt)
e2e/playground/run-proofs.sh p4   # gateway token attribution for that chat
e2e/playground/run-proofs.sh p5   # entitlement isolation (adapter)
e2e/playground/run-proofs.sh p6   # service-emitted usage at the sink
e2e/playground/run-proofs.sh p7-assistant
e2e/playground/run-proofs.sh p7-catalog
e2e/playground/run-proofs.sh p8   # dry-run vs labeled set — NO teardown
```

---

## P2 — host chat through the gateway — **PROVEN (3/3)**

`run-proofs.sh p2`: the host `patch` CLI (gateway mode) drives a chat that the
in-cluster assistant answers by calling StreamCo's `pipeline_diagnose` **through
the Envoy AI Gateway**.

```
PASS  [pg.chat.exit]     patch chat (playground gateway mode) exits 0 — exit=0
PASS  [pg.chat.findings] chat streams StreamCo findings (host→gateway→stub + MCP via gateway) — matched [CONSUMER_LAG, vod-transcode, p-1]
PASS  [pg.chat.context]  captured the conversation contextId — contextId=019f6812-7332-746c-8bb6-11243bf6aa36
```

## P4 — gateway token attribution + reconciliation — **PROVEN (4/4, EXACT)**

`run-proofs.sh p4`. Capture recipe (proven, macOS-safe): resolve the running
data-plane Envoy pod by `gateway.envoyproxy.io/owning-gateway-name=patch-playground`,
`kubectl logs <pod> --all-containers --tail=-1`, filter `"log.type":"llm"` by
`x_datum_conversation == contextId`, poll for the flush.

```
PASS  [pg.tokens.record]      gateway access log has llm record(s) for our chat conversation — matched=2 (of 16 records)
PASS  [pg.tokens.attribution] access log carries x_datum_project + x_datum_agent — project=demo-project agent=patch
PASS  [pg.tokens.present]     gateway counted input/output/total tokens — input=494 output=96 total=590
PASS  [pg.tokens.equal_sink]  gateway-counted == sink self-reported — gateway 494/96 == sink 494/96 (delta in=0 out=0)
```

The gateway-metered (tamper-independent) token counts EXACTLY equal the service's
self-reported usage, and every model call is project- and agent-attributed at the
gate. This is the core "every token gateway-metered and project-attributed" proof.

## P5 — entitlement isolation — **PROVEN (3/3)**

`run-proofs.sh p5`: the capability-provider adapter
(`GET /projects/{name}/capability-documents`).

```
PASS  [pg.entitlement.control]  entitled 'demo-project' receives capability documents — status=200 tools=[streamco-backend__pipeline_diagnose, streamco-backend__streams_get, streamco-backend__streams_list]
PASS  [pg.entitlement.reached]  adapter reachable for the unentitled query — status=200
PASS  [pg.entitlement.isolated] UNENTITLED 'unentitled-project' gets NO capability documents — status=200 tools=[]
```

An unentitled project receives an empty document set (HTTP 200 `[]`) while the
entitled control project receives its bound agent — capabilities are gated by
entitlement, live from the catalog CRs. (This also proves the adapter serves
live CR-derived documents, the mechanism P3 builds on.)

## P6 — service-emitted usage at the sink — **PROVEN (5/5)**

`run-proofs.sh p6`. Assertions scoped to the QA contextId (the user's concurrent
chats also land in the shared sink).

```
PASS  [pg.sink.events]          sink captured usage events for the conversation — events=4
PASS  [pg.sink.tokens]          sink has input-tokens AND output-tokens meters — [.../input-tokens, .../output-tokens, .../messages, .../tool-invocations]
PASS  [pg.sink.toolinvocations] sink has tool-invocations meter — present=true
PASS  [pg.sink.service_sourced] usage events SERVICE-sourced (source ends in /a2a) — sources=[http://localhost:1986/a2a]
PASS  [pg.sink.subject]         events attributed to subject projects/demo-project — subjects=[projects/demo-project]
```

The usage events are emitted by the **service** (CloudEvent `source` = the
assistant's `…/a2a`), not the host CLI — proving the "portal/CLI is just a client;
the SERVICE meters" architecture on the real environment.

## P7 — suites green — **PROVEN**

```
P7-assistant:  go vet ./...  clean;  go test ./...  ok (a2a, agent, auth, capability, config, server, usage)
P7-catalog:    envtest (setup-envtest 1.31.0) — ok (cmd/capability-provider, internal/controller, internal/validation)
```

## P8 — playground-down --dry-run == labeled footprint — **PROVEN (no teardown)**

`run-proofs.sh p8`. `playground-down.sh --dry-run` lists what it WOULD remove;
the driver diffs it against an independent broad all-api-resources sweep of the
`part-of=agent-framework-playground` label, canonicalized to `kind/name`.

```
P8: dry-run rc=0 labeled=26 dryrun=26 only-in-labeled=0 only-in-dryrun=0
```

The dry-run set equals exactly the 26 BASE-labeled objects (namespace
`patch-playground` + `patch-pg-gw` GatewayClass + the labeled deployments/
services/configmaps/secrets/routes within). **Nothing was torn down** — the env
is persistent. Note (by design): the catalog OVERLAY (ns
`agent-framework-playground`) shares the `part-of` label but is torn down
separately (catalog-engineer); the BASE `playground-down.sh` correctly scopes to
the BASE footprint only. `snapshot.sh labeled-all` shows the full 73-object
cross-namespace footprint.

---

## Pending proofs (honest gaps)

### P1 — bring-up idempotent + preflight-gated — **PENDING**

The env is already up (persistent + pg-infra brought it up). The driver's
low-impact idempotency test is one `playground-up.sh --skip-build` re-run,
asserting a NO-OP (labeled footprint byte-identical before/after) + exit 0 +
"Playground is UP" + the preflight memory-% gate ran. This re-run restarts the
assistant/gateway port-forwards, briefly dropping the user's live `:1986`
session — so it is being coordinated with pg-infra (who owns bring-up and asked
to be pinged) rather than run unilaterally while the user is active. The
preflight gate and idempotency are present in `playground-up.sh` by inspection;
this proof captures the live re-run evidence once a window opens.

### P3 — live reconfiguration — **PENDING (assistant not yet wired)**

The demo — `kubectl apply` a narrower `ServiceAgentConfiguration` → the next
chat turn's capabilities shrink; unpublish → capabilities gone — requires the
in-cluster assistant to fetch capabilities from the adapter. The live assistant
is currently **fixture-mode** (`CAPABILITY_DOCS_FIXTURE=/config/…`, no
`CAPABILITY_PROVIDER_URL`), so a mid-session CR change is not reflected in a chat
turn. pg-infra must wire the assistant to the adapter (`playground-up.sh
--with-catalog`, which sets `CAPABILITY_PROVIDER_URL` on the deployment). The
driver (`run-proofs.sh p3`) applies the real v1/v2 SAC samples with pg-catalog's
required entitlement **poke** after each apply (the AgentBinding reconciler only
watches `ServiceEntitlement`; without the poke a change waits out the 5-minute
resync) and **restores the v1 baseline** after the destructive unpublish.
Additional caveat: the overlay CRs were being actively modified during testing
(the live v1 capability doc drifted between two fetches), so P3's apply/restore
needs a window where the overlay is not being changed out from under it.

The adapter mechanism P3 depends on IS proven live (P5): the adapter serves
live, entitlement-gated, CR-derived capability documents.

## README quickstart verification (`deploy/playground/README-PLAYGROUND.md`)

Followed the "Try it as a consumer" quickstart literally as a new consumer:

| Step | Result |
| --- | --- |
| `go build -o /tmp/patch ./cmd/patch` + `patch card` | ✅ prints the Patch A2A v1.0 card |
| `PATCH_URL=…:1986 PATCH_TOKEN=pg-demo-token patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project` | ✅ streams the real StreamCo diagnosis (CONSUMER_LAG on vod-transcode) |
| no/invalid token → rejected | ✅ `401 Unauthorized` (auth boundary is real) |
| keyless assistant (`exec deploy/assistant -- printenv \| grep -i anthropic`) | ✅ no model key in the assistant env |
| **usage via `exec deploy/sink -- wget -qO- …/events`** | ❌ **fails** — the sink image has no `wget` (`failed to start exec "…wget"`). The events ARE captured (`/data/captured-events.jsonl` had 15 lines). **Suggested README fix**: inspect the sink via a port-forward instead — `kubectl -n patch-playground port-forward svc/sink 7811:7811 & curl -s localhost:7811/events` (this is what P6 uses, and it works). |

Reported the sink-command fix to pg-infra.
