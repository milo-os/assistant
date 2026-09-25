# Capabilities — knowledge, tools, skills

How a provider service contributes to Patch: the capability-document schema this service owns, and the three producers that serve it — a fixture file, the capability-provider HTTP API, and the `CapabilityBinding` CRD.

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
| `locations_list(service)` | Where a named service is offered to this project, joined from `ServiceAvailability` (`services.miloapis.com/v1alpha1`) to the `Location` (`locations.miloapis.com/v1alpha1`) it names, with topology and readiness. |
| `quota_get(service?)` | Limit, used and available per resource type, from `AllowanceBucket` (`quota.miloapis.com/v1alpha1`, namespace `milo-system`, label `quota.miloapis.com/consumer-kind=Project`), converted into the unit `ResourceRegistration` publishes. |

Three more tools make up the change path, described below.

### The change path

Three tools. Only one of them changes anything.

Patch composes them only when a plan-token key is available (`PLAN_TOKEN_KEY`,
or one generated for the process). A service that cannot check a token must not
issue one.

| Tool | What it does |
|---|---|
| `resources_validate(manifests[])` | Asks the platform to judge each manifest and keeps nothing. Returns the field path behind each rejection, whether the resource already exists, and what applying would change. **Writes nothing.** |
| `resources_plan(manifests[])` | Validates everything, decides create or update per resource, orders them so a manifest referring to another goes after it, reports the differences, and returns canonical manifests with a **plan token**. **Writes nothing.** |
| `resources_apply(manifests[], planToken)` | Re-derives the token from what it was handed and refuses any mismatch. Then dry-runs once more and applies in plan order. **The only tool here that writes.** |

#### The plan token

The token is an `HMAC-SHA256` over:

- the canonical JSON of every manifest, in plan order
- the project
- the resource version of each object the plan saw
- the expiry, 15 minutes out

Anything that moves fails to re-derive: a manifest edited after the plan (one
character is enough), a reordered list, a token minted in another project, or a
resource someone else changed in between. Apply refuses rather than writing
something nobody agreed to. Every refusal says what happened, that nothing
changed, and to plan again.

The token proves the change is the one shown. It cannot prove the person agreed.
That is a human step, required by Patch's fixed operating rules: nothing the
assistant reads in a tool result, a provider document, or a resource's status
counts as the person's answer. See `internal/plantoken`.

#### Ordering

A manifest whose spec carries a `*Ref` or `*Refs` field naming another manifest
in the batch is applied after it. A `kind` on the reference is honored when
present. A manifest that refers to nothing else keeps the order you gave it.
Order is part of what the token covers, so a reordered list is refused.

#### What counts as a change

`Composed.Mutating` lists the composed tools that can change something:

- provider tools flagged in a capability document's `mcpServers[].mutating`
- `resources_plan` and `resources_apply`

`resources_plan` is listed even though it writes nothing, because it alone
authorizes a write. An operator asking what this project's assistant can change
wants both answers. Each tool's trace span carries the same value as
`tool.mutating`.

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

### Two failures that must not read alike

`locations_list` refuses loudly when the project does not serve
`ServiceAvailability` or `Location`, naming the missing kind and saying the
person did nothing wrong. It never degrades to an empty list. An empty list is a
real answer — the service is offered nowhere this project may use — and
returning it when nothing actually looked would tell a customer to wait for a
location that is already there. The two call for opposite actions.

`quota_get` behaves the same way: a project that does not serve the allowance
kinds is told the answer is unknown, not that there is no limit.

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

Three implementations ship, selected by `CAPABILITY_SOURCE` (see
[configuration.md](configuration.md#capability-source)); exactly one is built:

- **Fixture source** (`CAPABILITY_SOURCE=fixture`, `CAPABILITY_DOCS_FIXTURE`) —
  a local JSON file (bare array or a `{"items": […]}` List). Good for local dev
  and e2e.
- **HTTP source** (`CAPABILITY_SOURCE=http`, `CAPABILITY_PROVIDER_URL`) — the
  **capability-provider API** below. Documents are fetched per conversation (no
  cache in v0).
- **CRD source** (`CAPABILITY_SOURCE=crd`) — a cached per-project LIST of
  [`CapabilityBinding`](#crd-source-capabilitybinding) objects from that
  project's control plane. The production source.

The schema below is the wire contract for the fixture file, the HTTP response
body, and — field for field — a `CapabilityBinding`'s `spec`. **The assistant
owns this schema** — a capability provider (the control-plane adapter, the
catalog's projection controller) produces documents in this shape; if the shape
changes, it changes here first.

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


### CRD source (`CapabilityBinding`)

In production the documents above are not fetched from a file or an HTTP
endpoint. They are read from **`CapabilityBinding` objects**
(`capabilities.assistant.miloapis.com/v1alpha1`) living in each project's own
Milo control plane. This is not a new contract — it is the same schema,
expressed as a Kubernetes resource, so the assistant still owns it and producers
still conform. The Go types are `pkg/apis/capabilities/v1alpha1`; the manifest is
`config/crd/capabilitybindings.yaml`.

#### Cluster-scoped, because the control plane is the tenancy boundary

The kind is `scope: Cluster`. A Milo project is a **virtual control plane** — one
apiserver partitioned by an etcd key prefix — not a namespace in the cluster the
assistant runs in. A project plane has its own ordinary namespace set
(`milo-system`, `default`) and nothing creates a namespace named after the
project, so a namespaced kind would have forced every producer to invent a
namespace convention that means nothing and that the storage layer never checks.
An assistant LISTing `/namespaces/<project>/capabilitybindings` against a project
plane would get a well-formed, permanently empty list, and every project would
compose as though it had bought nothing.

The consequence is that a binding carries **no project handle at all**, and it
does not need one: the objects `acme` is entitled to are the objects reachable
through `acme`'s control-plane path, and another tenant's bindings are not
filtered out of the response — they are not in the keyspace being read. The
`ScopeDocuments` namespace check that guards the fixture and HTTP sources is a
no-op here, and that is a strengthening, not a hole: a string comparison against
a producer's convention has been replaced by a path the storage layer enforces.

#### `spec` is the capability document's `spec`

Field for field, and byte-identical in the JSON tags:

| Document (`spec`) | `CapabilityBindingSpec` | |
|---|---|---|
| `serviceRef`, `serviceName`, `serviceAgentRef`, `configurationVersion` | same names | required, mirroring `Validate()` |
| `knowledge`, `tools`, `skills`, `authority`, `reportingProject` | same names | optional |

The source marshals the KRM object's spec and feeds the bytes straight through
the same `ParseDocuments` path the other two sources use. That is what makes one
schema serve three producers rather than three parsers serving one schema — and
it is also the hazard: a JSON tag that drifts between
`pkg/apis/capabilities/v1alpha1/types.go` and
`internal/capability/document.go` does not fail a build, it silently drops a
field from every project's prompt. Change both or neither.

Required-ness is mirrored into the OpenAPI schema, which moves the structural
half of validation to admission: a `kubectl apply` of a binding missing
`spec.serviceName` fails immediately, against the person who typed it. The
runtime `Validate()` stays — it still guards the fixture path, and a newer
assistant may refuse what an older CRD admitted.

A complete example, the CRD form of the StreamCo fixture, is
`config/overlays/dev-crd/capability-bindings.yaml`.

#### Reading: a cached per-project LIST

On a cache miss the source issues

```
GET {AUTHZ_SAR_API_URL}/apis/resourcemanager.miloapis.com/v1alpha1/projects/{project}/control-plane
    /apis/capabilities.assistant.miloapis.com/v1alpha1/capabilitybindings
```

with the assistant's own client certificate. There is no `namespaces/` segment —
the kind is cluster-scoped, and the project in the prefix is the only thing that
scopes the read.

The cache is the reason this source exists rather than being a third way to say
the same thing:

- **60 seconds, per project**, matching the SAR cache's TTL. The config plane
  comes off the turn path for every request but the first of each window. The
  cost is honest: a revoked entitlement lingers for up to the TTL, which is the
  same window the platform already accepts for a revoked *authorization*.
- **Serve stale on failure.** When a refresh LIST fails and a previous good
  answer for that project is in hand, the assistant serves the stale answer
  rather than nothing. Without a cache, a provider outage and "entitled to
  nothing" are the same user experience: built-ins only, no signal, and the
  only evidence a warn line in the assistant's own logs.
- **Never cache an empty result.** An empty LIST is what a project with no
  bindings returns *and* what a project returns in the window between the
  catalog creating its first binding and that write landing. Caching it would
  pin a newly-entitled project to a built-ins-only assistant for a TTL, for no
  reason the user can see. The cache therefore only ever holds a positive
  entitlement set.
- **Bounded**, with the same eviction policy as the SAR allow-cache: expired
  entries swept first, then one live entry.

The degradation contract is the one the other sources already have — transport
error, non-2xx, or undecodable body is logged and falls back — with the fallback
improved from "nothing" to "the last good answer". Staleness is invisible in the
product by design; its audience is the operator and its channel is
`assistant_capability_fetch_total{outcome="stale_served"}`.

#### `status.conditions`: the feedback loop to the producer

The assistant is the **sole writer** of `status` on these objects, and the
producer must not write it — two writers on one status block is a hot loop. This
is what the CRD buys that neither the fixture nor the HTTP source can: today a
provider whose MCP endpoint is unreachable learns nothing, because the failure is
a log line in someone else's service. As a condition it is a
`kubectl describe capabilitybinding` away for the team that owns the endpoint.

| Type | `True` | `False` |
|---|---|---|
| `Accepted` | The spec parsed and validated; the binding is eligible to compose. | Validation failed. The message is the same path-qualified error the assistant would otherwise only log (`spec.skills[0].source: required`). Reasons: `Validated` / `ValidationFailed`. |
| `Composed` | The last turn that consulted this binding could use everything it declared. | An MCP endpoint outside `CAPABILITY_MCP_ENDPOINT_HOSTS` (named, with "was not dialed"); a connect failure or timeout; a `tools/list` that failed; a declared tool the server does not offer; a name already won by another binding; or total knowledge loss. Reasons: `Composed` / `CompositionDegraded`. |

Two things are deliberately **not** `Composed=False`: a knowledge source that
fails while its siblings succeed (including byte-cap truncation — the knowledge
arrived, just less of it), and a skill body that fails to fetch, which happens
lazily inside a turn and has no compose-time verdict at all. Each leaves a
working binding and each would flap per turn, and a flapping condition is a
per-turn control-plane write.

Writes are coalesced and rate-limited accordingly: only on a transition of
(type, status, reason, observed generation), never from a request goroutine, onto
a bounded queue drained by one worker that drops on overflow. A status update
that is lost is cosmetic; a status update that blocks a chat turn is an outage.
The patch is a `merge-patch` against the `status` subresource and therefore
carries the **full** condition set the writer knows — merge patch replaces an
array wholesale — so two replicas can transiently clobber each other until each
one's next transition. There is deliberately no conflict-retry loop; the next
observation is a better retry than an immediate one.

#### What a producer owes

The service catalog's projection controller materializes one binding per
(project, entitled service). Three obligations are not expressible in the
schema and are load-bearing:

1. **Rewrite `mcpServers[].endpoint` to the AI gateway's MCPRoute URL**, never
   the provider's own address. A raw endpoint loses the gateway-side copy of the
   tool allow-list, goes unmetered, and receives no caller identity. The
   assistant does not trust this: an endpoint outside
   `CAPABILITY_MCP_ENDPOINT_HOSTS` is refused at the dial site and reported as
   `Composed=False` (see
   [configuration.md](configuration.md#two-host-lists-not-one)). The check is
   the enforcement; the condition is how the catalog team finds out.
2. **Delete the binding when an entitlement is revoked**, rather than merely
   ceasing to serve it. A deleted binding stops composing after one cache TTL; an
   abandoned one composes indefinitely.
3. **Read the conditions.** A projection the assistant rejects should be visible
   on the producing side too, which is the entire point of writing them.

#### What this does not prove in dev

`config/overlays/dev-crd` seeds bindings directly and exercises the object model,
the conversion, the cache, and the endpoint guard. It does **not** exercise the
production topology: kind runs one apiserver with no project router, so the
control-plane path collapses onto the same server, the grant is an ordinary
ClusterRole beside the pod rather than root RBAC at Milo, and the credential is a
service-account token that Milo itself would reject. Per-project isolation in
particular is provided by nothing in kind and is satisfied trivially there — see
`test/e2e/README.md` for why no e2e asserts it.
