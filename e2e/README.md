# e2e — assistant service end-to-end confirmation

QA-owned end-to-end harness for the standalone assistant service. It boots the
service plus a demo provider and a usage sink, then drives the real A2A chat
path over HTTP with a dev bearer token and proves the six QA items in
`E2E-REPORT.md`.

## Layout

```
e2e/
  run-e2e.sh                     orchestrator (selftests | full | repo-tests | all)
  driver/a2a-checks.mjs          zero-dep A2A assertion driver (items 2-5)
  fixtures/agent-bindings.fixture.json   AgentBinding fixture (AGENT_BINDINGS_FIXTURE)
  streamco/                      StreamCo demo MCP provider (:7810) + knowledge routes
  sink/                          usage capture sink (:7811)
  E2E-REPORT.md                  the confirmation report (PROVEN / NOT PROVEN per item)
  out/                           per-run evidence (gitignored)
```

`streamco/` and `sink/` are self-contained assets lifted from the service-catalog
`feat/agent-framework-e2e` branch (`hack/agent-framework-e2e/`) — copied, not
imported, so this repo's e2e has no cross-repo dependency. Each carries its own
selftest.

## Prerequisites

- Node >= 22.18 (native TS stripping for StreamCo), `bun`, `curl`, `jq`.
- StreamCo deps installed once: `cd e2e/streamco && bun install`.
- The assistant service must be runnable from its repo (default boot command
  `bun run start` — override with `ASSISTANT_START_CMD`).

## Run

```
e2e/run-e2e.sh selftests   # StreamCo + sink standalone selftests (no service needed)
e2e/run-e2e.sh full        # boot StreamCo + sink + assistant, run the driver
e2e/run-e2e.sh repo-tests  # bun test + typecheck in the assistant repo
e2e/run-e2e.sh all         # full + repo-tests  (default)
```

Ports: StreamCo 7810, sink 7811, assistant 7820 (override via `*_PORT`).

## Model mode

Inference is **mocked** unless `ANTHROPIC_API_KEY` is set in the environment, in
which case the runner also does a real-model pass. The mock still drives the full
chat path (auth → agent loop → real MCP tool call → metering → SSE); only token
generation is faked.

## Configuration knobs (env)

| Var | Default | Purpose |
| --- | --- | --- |
| `ASSISTANT_REPO` | `/Users/scotwells/repos/milo-os/assistant` | repo to boot |
| `ASSISTANT_START_CMD` | `bun run start` | boot command run in the repo |
| `ASSISTANT_NO_BOOT` | `0` | `1` = assume an already-running service |
| `GOOD_TOKEN` / `WRONGPROJ_TOKEN` | minted from the cluster | ServiceAccount tokens for `patch-dev` (allowed) and `patch-dev-unauthorized` (403) |
| `GOOD_TOKEN` / `WRONGPROJ_TOKEN` | `e2e-token` / `wrong-token` | tokens the driver uses |
| `PROJECT` | `demo-project` | project the chat runs under |
| `AGENT_BINDINGS_FIXTURE` | `fixtures/agent-bindings.fixture.json` | bindings source |
| `USAGE_GATEWAY_URL` | `http://127.0.0.1:7811` | sink (the service posts CloudEvents here) |
| `PUBLIC_BASE_URL` | `http://127.0.0.1:7820` | the service builds the agent card `url` from this |
| `MODEL_MODE` | `mock` | `anthropic` for a real pass |

## Service couplings (verified against the running service @ `bf7896c`)

These are the couplings between this harness and the service surface, all
confirmed against the real service:

- **Boot command** `bun run start` (= `bun run src/index.ts`); env vars `PORT`,
  `AUTHN_TOKENREVIEW_API_URL`, `AUTHZ_SAR_API_URL`, `AGENT_BINDINGS_FIXTURE`, `MODEL_MODE`,
  `USAGE_GATEWAY_URL`, `PUBLIC_BASE_URL`.
- **Caller tokens** are real ServiceAccount tokens minted from the kind
  cluster, so the harness exercises the same TokenReview + SubjectAccessReview
  path a deployed pod does. A locally-booted assistant is pointed at the
  cluster's apiserver via `CONTROL_PLANE_ENV`.

- **A2A JSON-RPC endpoint** is discovered from the agent card's `url` field
  (`${PUBLIC_BASE_URL}/a2a`), so the driver does not hard-code the path.
- **SSE events** are JSON-RPC-wrapped `result` objects with `kind`
  (`task` | `status-update` | `artifact-update`), `status.state`, and `final:true`
  at the terminal frame. The driver reads these structurally and also substring-
  scans, and writes every raw event to `out/stream-events.jsonl`.
- **Typecheck**: the service `tsconfig.json` uses `include: ["src"]`, which
  excludes this harness (StreamCo uses Node type-stripping `.ts` import
  extensions the strict service tsconfig rejects). Do not add `e2e` back to it.
