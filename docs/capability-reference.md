# Capabilities — knowledge, tools, skills

How a provider service contributes to Patch: the capability-document schema this service owns, and the provider API that serves it.

## Capability documents (provider capabilities)

A **capability document** is how a provider service's contribution
reaches Patch: its tool endpoint(s), the reviewed tool allow-list, and
its knowledge sources. In the platform, these documents are produced by
the **service catalog** — a provider registers agent capabilities with
its service, and entitlement decides which projects receive them. The
document schema itself is **owned and published by this service**
(`internal/capability`), so the catalog conforms to Patch's contract
(never the reverse), and any other producer — a fixture file in dev, a
different control plane — can feed it the same way. Given a project's
documents, composition produces:

- **Knowledge** — each document's knowledge sources fetched over HTTP
  (short timeout, per-source byte cap) and rendered under a provenance
  header, appended to the system prompt.
- **Tools** — one MCP client per `tools.mcpServers[]` entry (official
  `modelcontextprotocol/go-sdk`, Streamable HTTP transport), exposing
  **only** the `toolSelector.include` tools, namespaced `<server>__<tool>`
  (sanitized `[a-zA-Z0-9_-]`, first-wins on collision). The allow-list is
  enforced client-side too. MCP clients are opened per task, given a 5s
  connect timeout, and closed at the terminal state.

Document shape (JSON; the KRM-style envelope — `apiVersion`/`kind`/
`metadata`/`spec` — carries provenance from producers like the catalog's
`AgentBinding` projection; the parser ignores unknown fields and rejects
invalid documents with clear errors):

```jsonc
{
  "kind": "AgentBinding",
  "metadata": { "name": "streamco-binding", "namespace": "demo-project" },
  "spec": {
    "serviceRef":  { "name": "streamco" },
    "serviceName": "streaming.streamco.example",   // used as the tool-invocation meter dimension
    "reportingProject": "streamco-platform",        // where this service's capability-gap reports land
    "knowledge": {
      "sources":  [{ "type": "LLMDocs", "url": "https://…/llms-full.txt" }],
      "concepts": [{ "gvk": { "group": "…", "kind": "Stream" }, "summary": "…" }]
    },
    "tools": {
      "mcpServers": [{
        "name": "streamco",
        "endpoint": "http://provider/mcp",
        "toolSelector": { "include": ["streams_list", "pipeline_diagnose"] }
      }]
    },
    "skills": [{
      "name": "lag-triage",
      "description": "Step-by-step procedure for triaging pipeline consumer lag",
      "source": "http://provider/runbooks/lag.md"   // body fetched on demand
    }]
  }
}
```

### Skills (provider procedures, loaded on demand)

A **skill** is a provider-published, reviewed *procedure* — the middle
rung between knowledge (facts the assistant reads) and tools (endpoints
it calls): "run this tool, interpret these fields, check X before
recommending Y."

Skills use **progressive disclosure**: only each skill's name and
one-line description enter the system prompt (under "Available
skills"), so a provider can publish many skills at near-zero prompt
cost. When a request matches, the model calls the built-in
**`load_skill`** tool, which fetches the body from `source` (5s
timeout, 64KiB cap, same degrade-gracefully posture as knowledge) and
returns it framed with provenance.

Security posture: a skill is provider content the model may *follow* —
which is exactly why it goes through the platform's review gate
(catalog-published, versioned configurations) before any customer's
assistant sees it. A skill never grants privileges: it can only direct
the model toward tools that are independently on the enforced
allow-list, and the platform prompt scopes it to that provider's
services. Loading a skill is not a provider tool invocation — no
`tool-invocations` billing event fires; the tokens it adds are billed
as input like the rest of the prompt. Executable skill bundles
(scripts) are deliberately unsupported.

## Base platform tools (always present)

Every project's composition carries a set of tools that belong to no provider.
They are **un-namespaced** — `resources_list`, not `<service>__resources_list` —
which is the same convention the other built-ins use (`load_skill`,
`memory_remember`, `memory_forget`) and is what makes them impossible for a
provider to shadow: every provider tool name carries a `<server>__` prefix.

| Tool | What it answers |
|---|---|
| `resources_list(group, version, kind, namespace?)` | Every resource of one kind in the project, with what the platform is reporting about each. Kinds held in a namespace default to `default`. |
| `resources_get(group, version, kind, name, namespace?)` | One resource as an editable manifest, with the platform's own bookkeeping removed, plus its conditions alongside. |
| `schema_get(group, version, kind, path?)` | What a kind's fields are and which are required, from the project's own published description. `path` narrows a large kind to one part. |

Three read tools, and no more than three. A platform record that another service
already keeps — where a service is offered, what a project's allowance has left
— is read from the service that owns it, through a tool of its own, so the
platform has one answer to that question rather than this service's copy of one.
Those services reach a project the ordinary way, as a capability document naming
their MCP endpoint.

Plus a change path — `resources_validate`, `resources_plan`, `resources_apply` —
described below.

### The change path

Three more tools, of which exactly one can change anything. They are composed
only when a plan-token key is available (`PLAN_TOKEN_KEY`, or one generated for
the process): a service that cannot check a token must not issue something that
looks like one.

| Tool | What it does |
|---|---|
| `resources_validate(manifests[])` | Asks the platform for its verdict on each manifest without keeping any of them. Returns the field paths a rejection names, whether each resource already exists, and what applying would change. **Writes nothing.** |
| `resources_plan(manifests[])` | Validates everything, resolves create vs update per resource, orders them so one that refers to another goes after it, reports the differences, and returns canonical manifests with a **plan token**. **Writes nothing.** |
| `resources_apply(manifests[], planToken)` | Re-derives the token from what it was handed and refuses on any mismatch, then dry-runs again and applies in plan order. **The only tool here that writes.** |

The plan token is `HMAC-SHA256` over the canonical JSON of every manifest, in
plan order, plus the project, plus the resource version of each object the plan
saw, plus the expiry — 15 minutes. So a manifest edited after the plan (one
character is enough), a reordered list, a token minted in another project, or a
resource somebody else changed in the meantime all fail to re-derive, and apply
refuses rather than writing something nobody agreed to. Every refusal says what
happened, that nothing was changed, and to plan again.

The token proves the change was the one shown. It cannot prove agreement
happened — that is a human step, and the platform's fixed operating rules are
what require it: nothing the model reads in a tool result, a provider document
or a resource's own status stands in for the person's answer. See
`internal/plantoken`.

**Ordering.** A manifest whose spec carries a `*Ref` (or `*Refs`) field naming
another manifest in the same batch is applied after it; a `kind` on the
reference is honoured when present. Anything with no reference into the rest of
the batch keeps the order it was given in. The order is part of what the token
covers, so a reordered list is refused.

**What is on the change path.** `Composed.Mutating` names the composed tools
that can change something: the provider tools a capability document flagged in
`mcpServers[].mutating` — the first thing in the runtime to read that field —
plus `resources_plan` and `resources_apply`. `resources_plan` is on the list
even though it persists nothing, because it is the only thing that can authorize
a write, and an operator asking "what can this project's assistant change" wants
both answers. It rides on each tool's trace span as `tool.mutating`.

### The two rules that hold for all of them

**They act as the caller.** Composition binds the platform client to the
caller's own bearer token and the turn's project, once, and hands the tools a
view that carries no other identity (`internal/projectapi`). The service holds
no credential for a customer's project, so a tool call can read nothing the
person could not read themselves. With no caller credential or no project the
tools are **not composed at all** — there is no fallback to reading as the
service.

**The project is never an argument.** No input schema has a project field and no
handler looks for one. The project comes from the request a SubjectAccessReview
already approved, for the same reason `X-Datum-Project` does: an argument naming
a project would be steerable by anything the model reads.

### A failure that must not read like an answer

This is a **requirement on any provider tool that reads a platform record** on a
customer's behalf — where a service is offered, what an allowance has left, and
anything else the platform itself keeps: when the kind that answers the question
is not served in the project, the tool must **fail loudly and name it**, and must
never degrade to an empty list. An empty list is a real answer. "This service is
offered nowhere you can use" and "nothing here could look" call for opposite
actions — one waits for the service to arrive somewhere, the other is a
deployment that needs fixing — so they must never read the same. Returning
nothing when nothing actually looked tells a customer to wait for a location that
is already there, or that they have no limit when the limit is merely unreadable.
A refusal should say which record was not served, that the person who asked did
nothing wrong, and that whoever operates the deployment is who fixes it.

The base tools hold themselves to the same contract: `resources_list`,
`resources_get` and `schema_get` say a kind is not served rather than answering
with nothing.

### Metering

A base tool fires **no** `tool-invocations` billing event, the same as
`load_skill` and the memory tools. The tool event names the provider service
that did the work (see [Metering](architecture/metering.md)), and there is no
provider here — the work is the platform reading the customer's own project as
the customer. Billing it to a provider would attribute it to the wrong party;
billing it to Patch would make reading your own project cost you twice, since
the tokens it adds are already billed as input. A provider tool that happens to
do the same read still meters, because that provider ran it.

### Capability provider API (published contract, v1)

Capability documents reach the assistant through the `Source` seam
(`internal/capability`) — the stable interface between the assistant and
wherever documents come from:

```go
// Source yields the capability documents entitled to a project.
type Source interface {
    Documents(ctx context.Context, projectName string) ([]CapabilityDocument, error)
}
```

Two implementations ship, selected by env and **mutually exclusive** (the
config loader rejects setting both):

- **Fixture source** (`CAPABILITY_DOCS_FIXTURE`) — a local JSON file (bare
  array or a `{"items": […]}` List). Good for local dev and e2e.
- **HTTP source** (`CAPABILITY_PROVIDER_URL`) — the **capability-provider
  API** below. Documents are fetched per conversation (no cache in v0).

The schema below is the wire contract for **both** the fixture file and
the HTTP response body. **The assistant owns this schema** — a capability
provider (the control-plane adapter) serves documents in this shape; if
the shape changes, it changes here first.

#### Endpoint

```
GET {CAPABILITY_PROVIDER_URL}/projects/{projectName}/capability-documents
Accept: application/json
```

- `{projectName}` is path-escaped; it is the caller's authenticated
  project. The provider returns exactly the documents that project is
  entitled to (server-side scoping — the assistant does not filter).
- **200** with a JSON body (see schema) is the only success. The body is
  either a bare array of documents or a `{"items": […]}` List.
- **Degradation contract:** any transport error, a non-2xx status, an
  unreadable body, or a malformed root is logged and treated as **no
  capabilities** (empty set, chat proceeds with built-ins only) — a
  provider outage never fails a chat. Individual documents that fail
  validation are **skipped with a warning**; the valid ones still apply.
  Fetches use a **5s timeout**.

#### Capability document schema (v1)

Derived from the Go types in `internal/capability/document.go`. Unknown
fields are ignored (forward-compatible); required fields are marked.
`configurationVersion` is the provider's own config revision, distinct
from this **document schema version (v1)**.

```jsonc
{
  "apiVersion": "services.miloapis.com/v1alpha1", // optional, provenance
  "kind": "AgentBinding",                          // optional, provenance
  "metadata": {                                    // optional
    "name":      "string",
    "namespace": "string"
  },
  "spec": {                                        // REQUIRED
    "serviceRef":           { "name": "string" },  // REQUIRED, name REQUIRED
    "serviceName":          "string",              // REQUIRED (tool-invocation meter dimension)
    "serviceAgentRef":      { "name": "string" },  // REQUIRED, name REQUIRED
    "configurationVersion": "string",              // REQUIRED (provider config revision)
    "reportingProject":     "string",              // optional — the provider's OWN project,
                                                    // where its team reviews capability-gap
                                                    // reports (see capability-gap-reporting-design.md).
                                                    // Distinct from metadata.namespace, which is the
                                                    // CONSUMER project this document was entitled to.
                                                    // Omitted => gap reporting unavailable for this service.

    "knowledge": {                                 // optional
      "sources": [{
        "type":  "LLMDocs | Runbook | Markdown",   // REQUIRED, must be one of the enum
        "title": "string",                         // optional
        "url":   "string"                          // REQUIRED
      }],
      "concepts": [{
        "gvk":     { "group": "string", "kind": "string" },
        "summary": "string"
      }]
    },

    "tools": {                                     // optional
      "mcpServers": [{
        "name":         "string",                  // REQUIRED
        "endpoint":     "string",                  // REQUIRED (Streamable HTTP MCP URL)
        "toolSelector": { "include": ["string"] }, // client-side allow-list
        "mutating":     ["string"]                 // optional; tools flagged mutating
      }]
    },

    "authority": {                                 // optional
      "reads": [{ "gvk": { "group": "string", "kind": "string" } }],
      "maxTaskDurationSeconds": 0                   // optional int
    }
  },

  "status": {                                      // optional, ignored by the assistant
    "conditions": [{ "type": "string", "status": "string", "reason": "string", "message": "string" }]
  }
}
```

#### Example response (the StreamCo fixture)

```json
{
  "items": [
    {
      "apiVersion": "services.miloapis.com/v1alpha1",
      "kind": "AgentBinding",
      "metadata": { "name": "streamco-binding", "namespace": "demo-project" },
      "spec": {
        "serviceRef": { "name": "streamco" },
        "serviceName": "streaming.streamco.example",
        "serviceAgentRef": { "name": "streamco-agent" },
        "configurationVersion": "v1",
        "reportingProject": "streamco-platform",
        "knowledge": {
          "sources": [
            { "type": "LLMDocs", "title": "Overview", "url": "http://127.0.0.1:7810/llms-full.txt" }
          ],
          "concepts": [
            { "gvk": { "group": "streaming.streamco.example", "kind": "Stream" }, "summary": "A live stream" }
          ]
        },
        "tools": {
          "mcpServers": [
            {
              "name": "streamco",
              "endpoint": "http://127.0.0.1:7810/mcp",
              "toolSelector": { "include": ["streams_list", "streams_get", "pipeline_diagnose"] },
              "mutating": []
            }
          ]
        },
        "authority": {
          "reads": [{ "gvk": { "group": "streaming.streamco.example", "kind": "*" } }],
          "maxTaskDurationSeconds": 60
        }
      },
      "status": { "conditions": [{ "type": "Ready", "status": "True" }] }
    }
  ]
}
```

The catalog-side **capability-provider adapter** projects `AgentBinding`
resources into this shape (rewriting MCP `endpoint`s to the gateway
MCPRoute URL). Because the assistant owns the schema, the adapter is
written against **this** contract, not the other way around.

