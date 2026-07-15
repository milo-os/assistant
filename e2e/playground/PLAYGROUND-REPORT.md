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

## Environmental caveat — shared cluster control-plane instability

Throughout this run the shared `test-infra` cluster's `kube-controller-manager`
(and intermittently `kube-scheduler`) were **crashlooping** under VM contention
("leaderelection lost"; 200+ restarts over the cluster's lifetime, oscillating
on a ~minutes cadence). This is a property of the shared host, not the
playground. Its effect is scoped: **a crashlooping controller-manager/scheduler
only blocks scheduling NEW pods; it does not evict or affect already-Running
pods** (kubelet keeps them serving). Consequently the proofs split by need:

- **Serve-path / read-only proofs — unaffected, run during the flap** (with a
  documented `SKIP_CLUSTER_HEALTH` where the driver's CP gate would otherwise
  over-hold): P2 (chat exercises running pods), P4 (reads gateway logs), P6
  (reads the sink), P7-assistant (host), P8 (read-only dry-run), P5 (reads the
  adapter pod). All PROVEN — the workloads were Running+Ready the whole time.
- **Scheduling-dependent proofs — gated on a healthy CP** (the driver's
  `cluster_health()` reports HELD otherwise): P1's full recreate and the
  `--with-catalog` env flip (new ReplicaSet). The driver distinguishes the two
  classes (`require_healthy_workloads` vs `require_healthy_cluster`).

The user also held a "pause the other kind clusters" decision open to relieve
the contention; these proofs proceeded against the live env meanwhile.

## Summary verdict

| Proof | Status |
| --- | --- |
| P1 — bring-up idempotent + preflight-gated | **PROVEN** for the catalog-preserving invocation (clean no-op); **plain BASE re-run over an overlaid env is an open defect** (see below) |
| P2 — host chat through the gateway | **PROVEN** (3/3) |
| P3 — live reconfiguration (v1→v2→unpublish) | **PROVEN** (6/6, behavioral) |
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

## P1 — bring-up idempotent + preflight-gated — **PROVEN (catalog-preserving) + one open defect**

**Preflight gate — proven present.** `up.sh` runs a resource preflight and aborts
if node memory requests exceed 80%: `node memory requests committed: 43%`.

**Idempotency — PROVEN for the catalog-preserving invocation.** The env is
persistent + catalog-wired, so the correct re-run is the invocation pg-infra
prescribes, which re-asserts the SAME adapter env without churning the overlay:

```
CATALOG_UP_CMD=skip playground-up.sh --with-catalog --skip-build
```

Result — a clean no-op:
```
✓ assistant now sources capabilities from …capability-provider… (fixture unset)
▶ Playground is UP (BASE + CATALOG overlay tier)
✓ assistant reachable on :1986 (HTTP 200 /healthz)
labeled footprint: 25 objects before == 25 after (IDENTICAL); assistant pod
  unchanged (no roll, exit 0).
```

**Open defect (real bug found — fix owed by pg-infra).** A *plain* BASE
`playground-up.sh --skip-build` (no `--with-catalog`) run on an env that HAD the
catalog wired re-renders the assistant from the fixture-mode BASE manifest,
re-adding `CAPABILITY_DOCS_FIXTURE` **without** clearing the
`CAPABILITY_PROVIDER_URL` the catalog flip set. They are mutually exclusive, so
the new pod crashes on startup:

```
Invalid configuration:
  - CAPABILITY_PROVIDER_URL: CAPABILITY_PROVIDER_URL and CAPABILITY_DOCS_FIXTURE
    are mutually exclusive — set at most one capability source
```

The already-Running (adapter-mode) pod kept serving via RollingUpdate, so the
env stayed up, but the Deployment was left in a stuck rolling state. Recovered
with `kubectl rollout undo deploy/assistant` (team-lead authorized).

**FIXED by pg-infra** (commits `4e05e43` + `74d5e0c`), re-verified by QA:
1. *Deterministic capability source* — `70-assistant.tmpl.yaml` now omits BOTH
   capability envs and sets exactly one via `kubectl set env`, so `kubectl
   apply`'s 3-way merge can never keep a stray `CAPABILITY_PROVIDER_URL` from a
   prior flip.
2. *Mode guard* — a plain BASE `up.sh` over a live env in CATALOG/adapter mode
   now **REFUSES** ("keep overlay: --with-catalog … / force: FORCE_BASE=1")
   instead of silently reverting the wiring a running P3 depends on. The guard
   sits *before* the assistant manifest render/apply, so it aborts without
   touching the Deployment.

QA re-verification (code-level + behavioral): the guard is present and sits
*before* the assistant manifest render/apply (`playground-up.sh` line ~295, ahead
of the `70-assistant` render at ~307), so it aborts without touching the
Deployment. A live plain-BASE run left the adapter-mode assistant pod completely
untouched (same pod, 0 restarts, adapter-only env, `/healthz` 200, overlay
binding still `1.0.0`). The run did **not** reach the printed REFUSE banner
because up.sh's *pre-guard* step re-asserts the shared Envoy Gateway operator and
waits for its rollout — and that operator was independently CrashLoopBackOff
(9 restarts, pod 63m old, predating this run) under the same VM contention that
flaps the control plane. up.sh hung on that wait and was killed; the playground
data plane was unaffected throughout (a chat during the hang still returned
`CONSUMER_LAG` findings through the gateway). Net: the fix is verified at the
code level and the assistant is provably protected; a clean end-to-end plain-BASE
REFUSE capture is gated on the shared cluster's control plane settling.

## P3 — live reconfiguration — **PROVEN (6/6, behavioral)**

`run-proofs.sh p3`, against the adapter-wired assistant, overlay frozen at the
v1 baseline by pg-catalog. Each stage applies the real cluster-scoped SAC sample,
**pokes** the `ServiceEntitlement` (the AgentBinding reconciler only watches the
entitlement — without the poke a change waits out the 5-minute resync), waits for
the AgentBinding to reproject, then captures the adapter's documents AND runs a
`patch chat "Diagnose pipeline p-1"` turn. The behavioral signal is whether the
turn actually ran `pipeline_diagnose` (its `CONSUMER_LAG` finding appears only
then) — the A2A CLI stream doesn't expose tool names, so findings are the sound
observable.

```
stage=v1          tools=[…pipeline_diagnose, …streams_get, …streams_list]  chat-diagnosed=true
stage=v2          tools=[streamco-backend__streams_list]                    chat-diagnosed=false
stage=unpublished tools=[]                                                  chat-diagnosed=false

PASS  [pg.reconfig.v1_tools]           v1 exposes the broad set (3 tools)
PASS  [pg.reconfig.v2_narrower]        v2 STRICTLY FEWER + a subset — v1=3, v2=1
PASS  [pg.reconfig.v2_expected]        v2 = [streamco-backend__streams_list]
PASS  [pg.reconfig.next_turn_reflects] v1 turn diagnoses (pipeline_diagnose), v2 turn does NOT
PASS  [pg.reconfig.unpublished_gone]   after unpublish: no capability documents ([])
```

This is the demo that matters: **`kubectl apply` a narrower ServiceAgentConfiguration
→ the very next chat turn loses the removed capability** (v2 dropped
`pipeline_diagnose`, so the assistant can no longer diagnose), and **unpublish →
all capabilities gone**. The AgentBinding reprojected v1.0.0 → 2.0.0 → pruned;
the adapter served the matching documents live (per-conversation fetch, no cache);
the chat behavior followed. Afterward the driver **restored the v1 baseline**
(deleted the v2 SAC so the binding falls back to v1; re-published the
entitlement) — verified live: binding `1.0.0`, adapter serving 3 tools.

Prerequisite that was resolved mid-run: the assistant had to be adapter-wired
(`CAPABILITY_PROVIDER_URL`, no fixture); pg-infra flipped it via `kubectl set
env`. A plain BASE `up.sh` re-run reverts that (see P1).

## README quickstart verification (`deploy/playground/README-PLAYGROUND.md`)

Followed the "Try it as a consumer" quickstart literally as a new consumer:

| Step | Result |
| --- | --- |
| `go build -o /tmp/patch ./cmd/patch` + `patch card` | ✅ prints the Patch A2A v1.0 card |
| `PATCH_URL=…:1986 PATCH_TOKEN=pg-demo-token patch chat "Diagnose pipeline p-1 for StreamCo" --project demo-project` | ✅ streams the real StreamCo diagnosis (CONSUMER_LAG on vod-transcode) |
| no/invalid token → rejected | ✅ `401 Unauthorized` (auth boundary is real) |
| keyless assistant (`exec deploy/assistant -- printenv \| grep -i anthropic`) | ✅ no model key in the assistant env |
| usage inspection | ❌→✅ **fixed**. As originally written (`exec deploy/sink -- wget -qO- …/events`) it failed — the sink image has no `wget` (`failed to start exec "…wget"`), though events WERE being captured. Reported to pg-infra, who committed the fix (`1a86676`): the README + up.sh banner now use `kubectl -n patch-playground port-forward svc/sink 7811:7811 & curl -s localhost:7811/events | jq .` (with `kubectl logs deploy/sink` as the shell-free alternative). Verified working. |

Net: the quickstart now works end-to-end as written.
