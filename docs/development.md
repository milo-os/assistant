# Development

Run the service, the repo layout, the kind-based dev environment, and the tests.

### Runtime

Written in **Go** (module `github.com/milo-os/assistant`, see `go.mod`
for the toolchain version). `go build ./cmd/assistant` produces the
service binary; `go build ./cmd/patch` produces the `patch` CLI consumer.
Standard library `net/http` for the mux. Tests and vet use the Go
toolchain (`go vet ./...`, `go test ./...`). The wire-level acceptance
harness under `e2e/` runs on bun/Node.

## Dev environment (kind via datum-cloud/test-infra)

The full environment — assistant behind the Envoy AI Gateway (metered,
keyless, tool allow-lists), StreamCo MCP provider, usage sink, and a
CloudNativePG-backed durable conversation store — runs on a kind
cluster bootstrapped by [datum-cloud/test-infra](https://github.com/datum-cloud/test-infra),
consumed as a pinned remote Taskfile include (the datum-cloud house
pattern; requires `TASK_X_REMOTE_TASKFILES=1`, see `.env.example`).

```bash
cp .env.example .env
task dev:setup            # kind cluster + operators + images + full stack
task dev:forward          # assistant → localhost:1986, gateway → localhost:1987

PATCH_URL=http://localhost:1986 PATCH_TOKEN=pg-demo-token \
  go run ./cmd/patch chat -i --project demo-project

task dev:redeploy         # fast loop: rebuild + roll the assistant
task e2e                  # chainsaw environment tests
task dev:clean            # remove the environment (shared operators stay)
```

`task dev:deploy OVERLAY=dev-catalog` switches capabilities from the
fixture file to the service catalog's capability-provider API (requires
the catalog side from milo-os/service-catalog). The managed Envoy
Service name is pinned (`patch-ai-gateway`), so every URL in the config
tree is static — no discovery or templating anywhere.

## Testing

```bash
task test        # go vet + unit tests (TEST_DATABASE_URL adds Postgres store tests)
task e2e         # chainsaw against the dev environment
task e2e:local   # wire-level A2A harness against locally-booted binaries
```

Highlights: an in-process MCP round-trip in `internal/capability` /
`agentcore/mcptool`; full httptest integration through the real mux
(agent card, auth 401/403/200, `SendMessage`/`SendStreamingMessage`
driving a tool call over real MCP with usage landing at an in-process
sink, `GetTask`/`CancelTask`); OIDC with a locally generated key; the
usage emitter golden test pinning the billing wire; and the agent
loop's exit/usage-aggregation rules pinned against the mock model.

The **e2e acceptance harness** (`e2e/`, bun) drives the built binaries
end to end (core + consumers + gateway legs) and byte-diffs the sink
wire against the recorded golden — see `e2e/E2E-REPORT.md`.

## Repo layout

```
cmd/
  assistant/          service binary: config → runner → server
  patch/              the `patch` CLI consumer (on a2aclient)
agentcore/            extractable model/loop library (no env, no internal/ imports)
  model.go, usage.go, loop.go   unified stream parts, usage, tool loop
  anthropic/          adapter on anthropic-sdk-go
  openaicompat/       adapter on openai-go (gateway mode)
  mockmodel/          scripted in-process model
  mcptool/            MCP go-sdk client → agentcore ToolSet adapter
internal/
  capability/         capability documents: schema, fixture source, compose
  agent/              run-conversation orchestration (prompt, loop, usage events)
  a2a/                a2asrv AgentExecutor glue, task store wiring
  auth/               dev tokens + OIDC, fail-closed authorizer seam
  usage/              CloudEvents emitter (golden-pinned billing wire)
  config/             env → Config
  server/             net/http mux: /healthz, agent-card, /a2a
  logger/             slog setup
config/               kustomize dev environment (house pattern)
  base/               assistant Deployment/Service + namespace
  components/         gateway (Envoy AI Gateway resources), llm-stub,
                      streamco, sink
  overlays/dev        fixture-capability mode (default), images :dev
  overlays/dev-catalog  catalog capability-provider mode
  dependencies/       cnpg (vendored operator + Cluster), ai-gateway
                      (EG HelmRelease extension patch)
test/e2e/             chainsaw environment tests (env-health, chat-smoke)
fixtures/             capability-documents.json (sample)
e2e/                  wire-level A2A acceptance harness (bun/Node)
deploy/               Dockerfiles for the dev images
Taskfile.yaml         dev-environment entrypoints (see the dev-environment section above)
```

