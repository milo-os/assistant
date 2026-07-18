# Capability gap reporting

## Context

A user asked Patch for a StreamCo pipeline ID; Patch had no lookup tool for
it, said so, and told the user to go find it in the console. That's a
correct answer, but it's also a missed signal: a provider service (StreamCo)
is missing a tool its own customers need, and today the only way that
reaches StreamCo's team is if the user manually reports it — which they
usually won't.

The fix is a tool the model calls when it identifies a capability gap
(similar shape to `memory_remember`: model-invoked, not a passive transcript
scan, because the model has already done the work of articulating *why* it's
stuck). The hard part isn't the tool — it's where the report goes.

**The report must land in the provider's project, not the consumer's.**
`demo-project` is where this conversation happened, but the team that can
act on "we need a `streams_list` tool" works out of StreamCo's own project.
Every existing piece of project-scoped state in this codebase — history,
the new `internal/memory` — is keyed by the *consumer* project
(`ExpectedProject` / `params.ProjectName`). None of it has a second axis for
"which provider does this belong to." Getting that axis right is the actual
design problem here.

## Why there's no shortcut

`CapabilityDocument.Spec` (`internal/capability/document.go`) already
carries `ServiceRef.Name` and `ServiceName` — e.g. `"streamco"` /
`"streaming.streamco.example"` — but these are free-text provenance labels,
used today only for tool namespacing, logging, and the metering dimension
on `ProviderToolInvocation`. Neither is a Milo project/namespace, neither
carries an auth boundary, and nothing maps `"streamco"` to a real project a
gap-report write could target. `Metadata.Namespace` on the same document is
the *consumer's* namespace (`demo-project`), not the provider's — it's what
`scopeDocuments`/`ExpectedProject` already check, and using it here would
misdirect every report to the wrong project.

So this feature needs a genuinely new field, not a reinterpretation of an
existing one — and per `docs/capabilities.md`, **this service owns that
schema**: the capability-provider API is a contract the catalog's adapter
is written against, not the reverse. Adding to it here is the correct
direction.

## Design

### 1. Schema addition: `spec.reportingProject`

Add a required-when-tools-are-declared field to `CapabilitySpec`
(`internal/capability/document.go`) and the wire schema
(`docs/capabilities.md`):

```jsonc
"spec": {
  "serviceRef":       { "name": "streamco" },
  "serviceName":       "streaming.streamco.example",
  "reportingProject":  "streamco-platform",   // NEW — where gap reports for
                                               // this service's tools land
  ...
}
```

This is resolved and populated by the service catalog when it projects an
`AgentBinding` into a capability document — the catalog already knows which
Milo project a service's own team operates in (that's how the service
registered itself); the assistant does not need to invent or infer it. If
`reportingProject` is absent on a document that declares `tools`, gap
reporting is simply unavailable for that service (degrade gracefully, same
posture as a malformed knowledge source — log and skip, never fail the
chat).

### 2. New package: `internal/gapreport`

Mirrors `internal/memory`'s shape exactly (`Store` interface,
`MemoryStore` + `PostgresStore`, idempotent-DDL schema slice), but keyed by
**provider** project, and append-only (reports aren't upserted or edited by
the model — this is a log, not project memory):

```go
package gapreport

type Report struct {
    ID               string    // generated (uuid)
    ProviderProject  string    // e.g. "streamco-platform" — the write key
    ServiceName      string    // spec.serviceName, for filtering
    ConsumerProject  string    // e.g. "demo-project" — provenance only
    ContextID        string    // conversation this arose in, for provenance
    Capability       string    // short: "list pipelines for StreamCo"
    Summary          string    // what the user was trying to do
    CreatedAt        time.Time
}

type Store interface {
    List(ctx context.Context, providerProject string) ([]Report, error)
    Insert(ctx context.Context, r Report) error
}
```

Bounds mirror `internal/memory`'s posture: cap value lengths
(`MaxCapabilityLen`, `MaxSummaryLen`) and a per-provider-project report cap
(`MaxReportsPerProject`) enforced as a hard error surfaced to the model
("gap reporting is at capacity for this service") rather than silent
eviction — same reasoning as memory: losing a fact/report silently is
worse than a rejected write the model can retry or drop.

Schema (new table, appended to the existing Postgres schema-slice
convention):

```sql
CREATE TABLE IF NOT EXISTS capability_gap_report (
    id                text        PRIMARY KEY,
    provider_project  text        NOT NULL,
    service_name      text        NOT NULL,
    consumer_project  text        NOT NULL,
    context_id        text        NOT NULL,
    capability        text        NOT NULL,
    summary           text        NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now()
)
```
Index on `provider_project` for the list path.

### 3. New capability tool: `internal/capability/gapreport.go`

Same `agentcore.Tool` template as `rememberMemoryTool` / `loadSkillTool`.
Critically, this tool is **registered per composed document**, not once
globally — its closure is bound to that document's
`(ServiceName, ReportingProject)`, so the model cannot free-text an
arbitrary provider to write into. Namespaced like MCP tools: e.g.
`report_capability_gap__streamco`. If a turn composes documents from three
providers, three narrowly-scoped instances of the tool are registered, one
per provider — mirroring how `NamespaceToolName` already namespaces
provider MCP tools today.

- Input: `{capability, summary}` (no project/service field — that's fixed
  by which tool instance got called, closing off the misdirection risk
  entirely rather than validating it after the fact).
- `Execute`: builds a `Report` from the closed-over `ServiceName` /
  `ReportingProject` plus `ConsumerProject`/`ContextID` threaded in at
  registration time (same as `ExpectedProject` today), inserts, returns a
  short confirmation. Bound violations surface as tool errors.
- `Definition().Description` carries the usage protocol: call this when a
  request needs a capability this service doesn't expose (a lookup, a
  tool, a piece of knowledge) — not for user mistakes or for gaps in
  *other* providers' services (that's a different tool instance, if one
  exists for that provider) or platform-wide gaps unrelated to any single
  provider (out of scope here — see Non-goals).

No system-prompt addendum is needed (unlike memory's index) — this is a
write-only action tool, nothing to inject as context.

### 4. Wiring: `Compose()` (`internal/capability/compose.go`)

- `ComposeOptions` gains `GapReports gapreport.Store` (nil disables the
  feature entirely, same convention as `Memory`).
- Inside the existing per-document loop (where `tools.mcpServers` are
  turned into namespaced MCP clients), when `opts.GapReports != nil` and
  `doc.Spec.ReportingProject != ""`, register one
  `reportCapabilityGapTool{store: opts.GapReports, serviceName:
  doc.Spec.ServiceName, providerProject: doc.Spec.ReportingProject,
  consumerProject: opts.ExpectedProject, contextID: <threaded through>}`.

### 5. Wiring: `Deps` / `runner.go`

Same shape as `memory.Store`: `agent.Deps` gains `GapReports
gapreport.Store`; `cmd/assistant/runner.go` builds a
`gapreport.PostgresStore` off the same `ConversationStoreURL` (new table,
same database) when configured, else an in-process fallback with a log
line noting reports won't survive restarts.

### 6. Read path: how StreamCo's team actually sees these (shipped)

Follows the existing `conversations` read-view precedent
(`docs/conversation-apiserver-design.md`): gap reports are exposed through
the same aggregated apiserver pattern, scoped by the caller's k8s identity
against `providerProject` — `kubectl get capabilitygapreports -n
streamco-platform`, or `patch gaps list --project streamco-platform`,
authorized the same way `conversations show` already is (delegated
authn/authz to the front kube-apiserver's RBAC; no service-specific
`ProtectedResource`/`Role` config exists for either resource). This reuses
existing project-scoped RBAC rather than inventing a new auth model — a
StreamCo team member only sees reports where `provider_project` matches a
project they already have access to.

Implemented as `assistant.miloapis.com/v1alpha1` `CapabilityGapReport` (a
new hand-written API type alongside `Conversation`, following the same
internal/versioned/conversion/deepcopy/OpenAPI pattern — this repo has no
wired codegen), registered in `internal/apiserver/registry/capabilitygapreport`
implementing `Storage`/`Scoper`/`Lister`/`SingularNameProvider` only (no
`Getter`; `gapreport.Store` has no single-item lookup and the CLI only
lists).

## Non-goals

- **Passive/heuristic gap detection** (grepping transcripts for "I don't
  have a tool for X"). Noisier, and the model already articulates the gap
  explicitly when it hits one — model-invoked reporting captures that for
  free at higher precision. Can be revisited later as a supplementary
  signal, not a replacement.
- **Platform-wide gaps not tied to a specific provider's own document**
  (e.g. "the assistant itself has no memory of past conversations" — a
  meta-gap about Patch, not about a provider service). Nothing in this
  design routes those anywhere; they'd need a distinct destination (the
  Milo platform team's own project) and are out of scope for this pass.
- **Auto-filing tickets / paging** in the provider's own issue tracker.
  This design stops at "durable, queryable record in the provider's
  project" — wiring that into StreamCo's Jira/PagerDuty is StreamCo's
  integration to build against the read path, not this service's job.

## Open questions

1. ~~Does the service catalog actually have "which Milo project does this
   service's team operate in" as an existing fact it can populate into
   `reportingProject`?~~ Resolved: yes — `spec.reportingProject` is set
   directly on the capability document (see design #1), populated by the
   service catalog from its own service registration.
2. Should `report_capability_gap` be visible to the model as a *tool*
   (self-serve, like memory) or should we require it be a **skill**-gated
   action, so providers can review/tune the exact phrasing of when it's
   invoked? Leaning tool, for symmetry with memory and because there's no
   sensitive action being taken.
3. Rate limiting / abuse: a chatty or confused conversation could spam a
   provider's project with near-duplicate reports. Worth a cheap
   dedup-by-similar-capability-string-per-conversation guard before
   inserting, not just the hard per-project cap.

## Files touched/added (shipped — write path + read path)

- `internal/capability/document.go` — `CapabilitySpec.ReportingProject`.
- `docs/capabilities.md` — schema addition, example.
- `internal/gapreport/gapreport.go` (new) — `Report`, `Store`, in-process
  fallback.
- `internal/gapreport/postgres.go` (new) — `PostgresStore`, schema.
- `internal/gapreport/*_test.go` (new).
- `internal/capability/gapreport.go` (new) — `reportCapabilityGapTool`,
  per-document registration in `Compose()`.
- `internal/capability/gapreport_test.go` (new).
- `internal/capability/compose.go` — `ComposeOptions.GapReports`,
  registration call.
- `internal/agent/conversation.go` — `Deps.GapReports`, `Compose` call
  site.
- `internal/agent/gapreport_test.go` (new).
- `cmd/assistant/runner.go` — construct/wire the store.
- `pkg/apis/assistant/types.go`, `v1alpha1/types.go`,
  `register.go`/`v1alpha1/register.go`,
  `v1alpha1/conversion.go`/`conversion_impl.go`,
  `zz_generated.deepcopy.go` (both packages),
  `v1alpha1/zz_generated.model_name.go` — `CapabilityGapReport` API type.
- `pkg/generated/openapi/zz_generated.openapi.go` — OpenAPI schema.
- `internal/apiserver/registry/capabilitygapreport/storage.go` (new) +
  `storage_test.go` — `CapabilityGapReportREST`, scoped by
  `internal/tenant.ProjectFromContext`.
- `internal/apiserver/apiserver.go` — `ExtraConfig.GapReports`, storage
  registration.
- `cmd/conversations-apiserver/serve.go` — wire the gap-report store.
- `cmd/patch/args.go`, `cmd/patch/gaps.go` (new), `cmd/patch/run.go` —
  `patch gaps list --project <provider>`.
