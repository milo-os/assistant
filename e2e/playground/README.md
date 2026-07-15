# e2e/playground — QA proof driver (slice 6)

The gatekeeper harness for the **real-environment playground** on the shared
`test-infra` kind cluster. Contract: scratchpad `CONTRACT-REAL-ENV.md`, proofs
**P1–P8**. This directory is QA-owned; it CALLS the builders' playground scripts
and asserts on their effects. **It never tears the playground down** (the env is
persistent and the cluster hosts unrelated live workloads) — P8 uses
`playground-down.sh --dry-run` only.

## Files

| File | Role |
| --- | --- |
| `run-proofs.sh` | Orchestrator: `p1`…`p8`, `base`, `overlay`, `all`. Non-fatal per proof (collects ALL results). |
| `playground-checks.mjs` | Node assertion driver: modes `chat` (P2), `reconfig` (P3), `tokens` (P4), `entitlement` (P5), `sink` (P6). Mirrors `e2e/driver/gateway-checks.mjs`. |
| `snapshot.sh` | `baseline` / `current` / `diff` (guardrail) and `labeled` (P8 truth set). |
| `out/` | Per-run evidence (gitignored). |

## Run

```bash
export KUBECONFIG=/Users/scotwells/repos/datum-cloud/test-infra/kubeconfig
# BASE (Phase 2): P1 P2 P4 P6 P7-assistant P8
e2e/playground/run-proofs.sh base
# OVERLAY (Phase 2, requires playground-up.sh --with-catalog): P3 P5 P7-catalog
e2e/playground/run-proofs.sh overlay
# individual proofs
e2e/playground/run-proofs.sh p2      # chat, writes out/playground-contextid.txt
e2e/playground/run-proofs.sh p8      # dry-run vs labeled set — NO teardown
```

`run-proofs.sh` sources `.run/env` (written by `playground-up.sh`) if present, so
the gateway port / URLs / project names follow the live env automatically.

## Proof map

| Proof | Tier | What it asserts |
| --- | --- | --- |
| **P1** | base | `playground-up.sh` is idempotent (2nd run exits 0, reports UP) and preflight-gated (memory-% abort line present). |
| **P2** | base | Host `patch chat` (gateway mode) streams StreamCo findings through the gateway; captures the contextId. |
| **P3** | overlay | Apply `ServiceAgentConfiguration` v2 (narrower `toolSelector`) → capability docs + next chat turn expose FEWER tools; unpublish → capabilities gone. |
| **P4** | base | Gateway access log has `log.type:llm` records for our contextId, each carrying `x_datum_project`/`x_datum_agent`, tokens > 0. |
| **P5** | overlay | An UNENTITLED project gets zero capability documents (entitled project is the control). |
| **P6** | base | Usage sink shows SERVICE-sourced (`source` ends `/a2a`) token + tool-invocation events for the playground chat. |
| **P7** | both | `go vet`+`go test` (assistant) green; catalog envtest green (overlay). |
| **P8** | base | `playground-down.sh --dry-run` set == the `part-of=agent-framework-playground` labeled set, exactly. No teardown. |

## Interface assumptions (confirm with builders; all env-overridable)

These are what the driver depends on. Drift is a one-line env override, not a
rewrite — but I flag them so mismatches surface before Phase 2.

- **Attribution label**: `app.kubernetes.io/part-of=agent-framework-playground`
  on every playground resource (P8 truth set). — pg-infra
- **Bring-up scripts**: `deploy/playground-up.sh` / `deploy/playground-down.sh`
  (override `PLAYGROUND_UP_CMD` / `PLAYGROUND_DOWN_CMD`); `--with-catalog` and
  `--dry-run` flags; up.sh prints a preflight memory-% line and an "is UP" line;
  down.sh `--dry-run` prints deletable objects as `kind/ns/name`. — pg-infra
- **Gateway**: owning-gateway-name label `patch-ai-gateway`, JSON access logs
  with `x_datum_project`/`x_datum_agent`/`x_datum_conversation` + `input_tokens`
  /`output_tokens`/`total_tokens`, host port `:1975`. — pg-infra
- **Capability provider adapter**: `GET {CAPABILITY_PROVIDER_URL}/projects/
  {name}/capability-documents` returning the doc set (array / `{documents}` /
  `{capabilities}`); tool names appear as `mcpServers[].toolSelector.include[]`
  or `tools[].name`. — pg-catalog
- **Catalog CRs** (P3): commands to apply v1 / apply v2-narrower / unpublish the
  `ServiceAgentConfiguration` — set `SAC_APPLY_V1` / `SAC_APPLY_V2` /
  `SAC_UNPUBLISH`; expected tool sets `V1_EXPECTED_TOOLS` / `V2_EXPECTED_TOOLS`. — pg-catalog
- **Entitlement (P5)**: an unentitled project name (`UNENTITLED_PROJECT`) and,
  optionally, a dev token scoped to it (`UNENTITLED_TOKEN`) for the chat sub-check. — pg-catalog
- **Assistant**: host-run gateway mode on `:7820` by default (`HOST_ASSISTANT=1`);
  if pg-infra runs it in-cluster, set `HOST_ASSISTANT=0` + `ASSISTANT_URL`/`PATCH_URL`
  to its NodePort/forward. Sink on `:7811`, `source` ends `/a2a`. — pg-assistant / pg-infra
- **Catalog tests (P7)**: `make test` in the service-catalog repo (override
  `CATALOG_TEST_CMD`). — pg-catalog

## Guardrails honored

- test-infra READ-ONLY; NEVER `task cluster-down`; NEVER tear the playground down.
- Pre-install baseline captured (`snapshot.sh baseline`) for byte-restore-style
  diffing; additive labeled installs only; resource preflight (>80% mem abort).
- macOS has no `timeout` — all waits are poll loops.
