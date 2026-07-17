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

