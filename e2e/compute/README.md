# Datum Compute — AI plugin prototype

The provider side of an AI plugin for
[datum-cloud/compute](https://github.com/datum-cloud/compute): the tools Patch
may call to diagnose compute Workloads, plus the knowledge and skills it reads.

This is a **prototype**, deliberately living in the assistant repo so the
tool/knowledge/skill design can be iterated in the existing dev loop. The
design is meant to land in the compute repo as the real provider-side plugin —
see [Porting](#porting-to-datum-cloudcompute).

## Why the tools look like this

Compute's API already carries its own diagnostic vocabulary. Every blocking
cause is a stable `reason` on a condition, and `api/v1alpha`'s doc comments say,
for each one, whether the customer must act, whether the platform is at fault,
or whether it is simply in flight.

Two properties of that API shape the plugin:

1. **Top-level reasons are pointers, not causes.** `Workload.Available=False`
   with reason `QuotaNotGranted` says *which subsystem* is blocking; the real
   reason (`QuotaExceeded` vs `QuotaNoBudget` vs `QuotaBackendUnavailable`) is
   on an Instance's `QuotaGranted` condition further down. Reporting the pointer
   is a wrong answer that looks like a right one. `workload_diagnose` walks
   Workload → WorkloadDeployment → Instance and returns the leaf cause.

2. **Actionability is the most useful thing to say.** `QuotaExceeded` and
   `QuotaNoBudget` both surface as "quota is blocking you", but one is the
   customer's to fix and the other is Datum's. Telling a customer to change
   their spec for a platform fault wastes their time. Every reason in the
   catalog is classified `user` / `platform` / `transient`, and the skills tell
   the model to lead with it.

## Layout

```
src/catalog.ts    Reason catalog: every compute condition reason, classified,
                  explained, with remediation. Derived from api/v1alpha.
src/diagnose.ts   The walk: follows pointer reasons down to the leaf cause.
src/data.ts       Canned Workloads/Deployments/Instances (throwaway — the real
                  plugin reads live objects).
src/server.ts     MCP server + knowledge/skill HTTP routes.
src/selftest.ts   Asserts the walk resolves each failure class correctly.
knowledge/        llms-full.txt (the resource model + how to read conditions)
                  and runbooks/*.md (the skills).
```

## Tools

Read-only, and all five are on the reviewed gateway allow-list:

| Tool | Purpose |
|---|---|
| `workloads_list` | Fleet view, worst first, with root-cause reason |
| `workloads_get` | Raw condition tree for one workload |
| `instances_list` | Per-instance conditions — how a failure is distributed |
| `workload_diagnose` | The walk, the leaf cause, and next steps |
| `reason_explain` | Any reason, explained and classified |

`workload_delete` is also exposed by the provider and deliberately **excluded**
from the gateway `MCPRoute` toolSelector — the same allow-list enforcement
control StreamCo's `streams_delete` provides. It must be reachable on a direct
connection and absent through the gateway.

## Skills

Loaded on demand via `load_skill` (only name + description sit in the prompt):
`workload-not-available`, `quota-triage`, `instance-not-ready`,
`referenced-data-triage`, `placement-triage`.

## Running it

In the dev cluster it is deployed by the `dev` overlay (and everything that
composes it) as the `compute` Deployment, fronted by the `patch-mcp-compute`
MCPRoute at `/mcp-compute`:

```bash
task dev:setup OVERLAY=dev-anthropic     # or dev, for stub mode
task dev:chat MSG="Why is the api-backend workload not available?"
task dev:chat MSG="The edge-cache workload has been down for two days. What do I need to change in my spec?"
```

Locally (needs Node >= 22.18 for native TS type stripping):

```bash
npm install
npm run selftest     # asserts the diagnosis walk
npm run typecheck
npm start            # serves on 127.0.0.1:7830
```

## Fixture scenarios

| Workload | Root cause | Class |
|---|---|---|
| `web-frontend` | — (3/3 ready) | healthy |
| `api-backend` | `QuotaExceeded` (2/6 ready) | user |
| `batch-processor` | `ImageUnavailable` | user |
| `telemetry-agent` | `InstanceCrashing` | user |
| `config-consumer` | `SourceNotFound` | user |
| `edge-cache` | `NoMatchingLocation` | **platform** |

`edge-cache` is the one to demo: ask what to change in the workload spec and the
assistant should refuse the premise and say it is Datum's to fix.

## Porting to datum-cloud/compute

`catalog.ts` and `diagnose.ts` are the artifacts meant to move; they depend on
nothing but condition data. In compute they become Go, with `data.ts` replaced
by real reads:

- The catalog belongs next to `api/v1alpha`, where the reason constants live, so
  a new reason and its classification land in the same change.
- The diagnosis walk reads Workloads, WorkloadDeployments, and Instances through
  the project control plane with the caller's identity.
- The MCP server becomes a new `cmd/` in compute, published through the service
  catalog's `AgentBinding` rather than the fixture file used here.

The knowledge and runbooks port as-is; `docs/runbooks/` in compute is the
natural home, and the existing reconcile-storm runbook is a candidate skill for
an operator-facing binding.
