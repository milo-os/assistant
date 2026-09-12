---
status: proposed
---

# CRD-backed capability source

> Design record. It describes the decision as it is being taken; once shipped,
> the resulting behavior belongs under
> [docs/architecture](../architecture/README.md).

This is the implementation spec for moving the assistant's capability
configuration off a per-turn HTTP pull (and off the dev fixture file, in
production) and onto a **Kubernetes custom resource this repository owns**,
read through a **short-TTL cached per-project LIST** against that project's
control plane. The `Source` interface (`internal/capability/source.go`) is
unchanged; a third implementation joins `FixtureSource` and `HTTPSource` behind
it.

### Revisions

This document has been corrected twice against facts established in the other
Milo repositories. Both supersessions are recorded rather than edited away,
because in each case the superseded reasoning was sound for what it assumed and
will be re-derived by the next person who assumes the same thing.

**Revision 1 — the read path.** The original design specified an in-process,
cluster-wide informer. A project is a **virtual project control plane**, not a
namespace in the assistant's own cluster, so a watch against
`kubernetes.default.svc` would never observe a single `CapabilityBinding`. The
read path became a cached LIST per project, addressed the way the existing
authorization path addresses a project.

**Revision 2 — the object model, and every open question.** Three things that
this document previously asserted or left open are now settled by code:

| Previously | Now | Evidence |
|---|---|---|
| `metadata.namespace == project` | **Cluster-scoped.** A project plane has its own independent namespace set; nothing creates a namespace named for the project. | `milo/internal/apiserver/storage/project/mux.go:225`; the catalog's analogous `AgentBinding` is `scope=Cluster` and its controller writes `ObjectMeta{Name}` with no namespace (`service-catalog, local branch feat/agent-framework-api (unpushed)`, `internal/controller/agentbinding_controller.go:243-244`) |
| OPEN: where the CRD gets installed | **Resolved: once, at Milo root.** CRD *definitions* are deliberately global; only instance data is partitioned. | `milo/internal/apiserver/storage/project/restoptions.go:35-38` — *"Leave CRD definitions global so discovery is shared cluster-wide"* |
| OPEN: what grants the assistant standing read | **Resolved: root RBAC, not IAM.** Milo IAM structurally cannot authorize a certificate identity. | `openfga-provider/internal/webhook/subjectaccessreview_authorizer.go:333-336`; `milo/pkg/server/filters/projects.go:40-62` |
| Per-project informers were "expensive and need machinery we lack" | **Supported and common** via `multicluster-runtime`; we decline on fan-out cost, not impossibility. | `milo/pkg/multicluster-runtime/milo/provider.go:249-267` |

---

## Context

Today a project's capability documents are fetched on the request path. Each
conversation turn calls `Conversation.loadDocuments`
(`internal/agent/conversation.go:527`), which calls
`Source.Documents(ctx, projectName)`; each *extended agent-card* request does
the same through `Conversation.Entitlements`
(`internal/agent/conversation.go`, reached from the agent-card path in
`internal/agentwiring`). In production that `Source` is `HTTPSource`,
which states its own posture plainly: *"There is no caching in v0 — every call
performs a fresh fetch"* (`internal/capability/http_source.go`), under a 5s
timeout (`httpFetchTimeout`, line 18).

The motivation for this change is not that files or HTTP are distasteful. It is
five specific defects that all have the same root: **the assistant reads its
configuration synchronously, from a remote authority, at the moment it is
needed, and has no memory of the last answer.**

### 1. The config plane sits on the latency path

Every turn and every extended-card request is gated on a network round trip to
the capability provider. That round trip's budget — up to 5 seconds — is spent
before the model is called, on data that changes at the cadence of an
entitlement edit, which is to say approximately never relative to chat traffic.
A provider that is merely *slow* taxes every user of every project.

A cache fixes the common case: the first turn for a project pays one LIST, and
every turn within the TTL is an in-memory read. Be honest about what that is
not — it is weaker than a watch. The config plane stays on the latency path for
a cache miss, and that miss recurs once per TTL per active project. What
changes is that the cost is amortized over a project's traffic instead of paid
per turn, and that a cache *hit* has no timeout to spend.

### 2. "Degraded" is indistinguishable from "entitled to nothing"

`HTTPSource.Documents` returns `nil, nil` on a transport error, a non-2xx
status, an unreadable body, or malformed root JSON. Each path logs a warning and
degrades. That is the right call given no cache — a provider outage must never
fail a chat (`docs/architecture/capabilities.md`, "Degrading, not failing") —
but it produces an indefensible user experience: during an outage the user gets
a built-ins-only Patch that behaves *exactly* as if their project were entitled
to nothing at all. No tools, no provider knowledge, no signal. The only evidence
is a `capability.http.fetch_failed` warn line inside the assistant's own logs.

A cache changes what is available to degrade *to*. When a LIST fails and a
previous good answer for that project is in hand, the assistant serves **stale**
configuration rather than **no** configuration. That is a strictly better
failure mode, and it is only reachable if the last answer is retained. Note the
bound this design accepts and the informer would not have: an outage longer
than a project's last cache entry has been resident still falls back to nothing
for that project, and a project that has had no traffic since the last restart
has nothing to serve stale.

### 3. There is no feedback loop to the producer

`Condition` and `Status` already exist on `CapabilityDocument`
(`internal/capability/document.go:140-151`). Nothing in this repository ever
writes them. When a document fails `Validate()`, `ParseDocuments` skips it and
reports it to an `onSkip` callback that logs
`capability.http.entry_skipped` — *inside the assistant*. The provider who
authored the broken document, and the catalog operator who projected it, learn
nothing. A capability can be silently absent for weeks.

Kubernetes already solved this problem: the consumer writes
`status.conditions` back onto the object, and the producer reads them with
`kubectl get`. Adopting the CRD and declining to write status would be adopting
the ceremony without the payoff, which is why full scope here includes the
status writeback.

### 4. Validation happens at read time, forever, instead of at write time, once

`CapabilityDocument.Validate()` (`internal/capability/document.go:169`)
re-checks required fields on every document on every turn. It is cheap, but it
is in the wrong place: it rejects bad input long after the author could have
been told. A CRD moves the structural half of that check to admission, where a
`kubectl apply` of a document missing `spec.serviceName` fails immediately,
against the person who typed it. The runtime `Validate()` stays — it still
guards the fixture path, and defense in depth at a trust boundary is not
redundancy — but it stops being the *only* gate.

### 5. Tenant scoping rests on a convention the schema cannot enforce

`ScopeDocuments` (`internal/capability/compose.go:333`) is the tenant-isolation
seam. It drops documents whose `Metadata.Namespace` names a project other than
the expected one. Its own comment concedes the gap:

> It fails closed only on a positive mismatch: a document with no namespace is
> kept, because the schema carries no other project handle to cross-check and
> the Source stays the scoping authority there.

A document with no namespace passes for *any* project. The check is a string
comparison against a field a producer populates by convention, in a JSON
document with optional `metadata` — which makes the `Source` itself, not the
check, the real scoping authority.

**An earlier version of this document claimed the CRD fixed this by making the
namespace mandatory. That was wrong, and the correction is more interesting
than the original claim.** `CapabilityBinding` is cluster-scoped (see
[API design](#api-design)), so its objects have no namespace at all. The CRD
source does not make the namespace check fail closed; it **removes the need for
the check** by replacing a conventional handle with a structural one. A LIST
addressed to `projects/acme/control-plane` is served from an etcd keyspace
prefixed `/projects/acme` (`milo/internal/apiserver/storage/project/mux.go:225`)
— another tenant's objects are not filtered out of the response, they are not
*in* the keyspace being read. Isolation moves from a field the assistant
inspects to a path the storage layer enforces.

The defect is therefore real but is fixed only for the CRD source.
`ScopeDocuments` stays exactly as it is, and stays necessary, because the
fixture and HTTP sources still return documents with a conventional namespace
field and no structural guarantee behind it.

---

## Locked decisions

1. **An assistant-owned CRD.** A new `CapabilityBinding` kind whose schema lives
   in this repository. The service catalog runs a controller that projects its
   `AgentBinding` into it. This is the same contract inversion already stated in
   `docs/architecture/capabilities.md` — *"Patch owns this schema. Producers
   conform to it, never the reverse"* — expressed in KRM instead of JSON.
2. **A separate API group.** `assistant.miloapis.com/v1alpha1` is claimed by the
   aggregated apiserver's `APIService`
   (`config/components/api-registration/apiservice.yaml:15-16`). The
   kube-aggregator owns that entire group/version: a CRD registered there would
   be shadowed by the aggregated server, which knows nothing about it. The new
   kind therefore lives in its own group, `capabilities.assistant.miloapis.com/v1alpha1`.
3. **Cluster-scoped, not namespaced.** A project control plane has its own
   namespace set and no namespace named for the project, so a namespaced kind
   would force every producer to invent a namespace convention that means
   nothing. The catalog's analogous `AgentBinding` is already `scope=Cluster`
   with `ObjectMeta{Name}` and no namespace
   (`service-catalog, local branch feat/agent-framework-api (unpushed)`,
   `internal/controller/agentbinding_controller.go:243-244`); we follow the
   house pattern rather than invent a second one.
4. **A cached per-project LIST**, not an external adapter process and not a
   watch. `Documents(ctx, projectName)` LISTs `capabilitybindings` against that
   project's control plane, behind a short-TTL cache that serves stale on
   error. Watching across projects is supported and common on this platform;
   we are declining it on fan-out cost, with precedent — see
   [Rejected alternatives](#rejected-alternatives).
5. **Full scope**, including writing `status.conditions` back onto binding
   objects.
6. **`FixtureSource` stays.** The README promises *"the assistant runs
   standalone — the catalog is one producer of its configuration, not a
   dependency"* (`README.md:45-47`), and `e2e/` plus `test/e2e/chat-smoke` both
   depend on the fixture. "Migrated" means "not the production source", not
   "deleted".

---

## Architecture / data flow

```
service catalog                    project "acme" control plane
──────────────                     ────────────────────────────
AgentBinding ──projection──────>   CapabilityBinding
 (catalog-owned)                   (capabilities.assistant.miloapis.com/v1alpha1,
                                    CLUSTER-scoped; the plane IS the tenancy)
                                          ▲   │
                     PATCH status ────────┘   │ LIST (on cache miss)
                                              ▼
                                   ┌──────────────────────────────┐
   /apis/resourcemanager.miloapis.com/v1alpha1/projects/acme/control-plane
                                   └──────────────────────────────┘
                                              │
                                     assistant process
                                       TTL cache (per project)
                                              │
                                              ▼
                                  CRDSource.Documents("acme")
                                              │
                                              ▼
                                           Compose
```

Three things to notice. The catalog and the assistant never talk to each other
— they meet at a control plane, exactly as the A2A service and the apiserver
meet only at Postgres (`docs/enhancements/assistant-apiserver.md`, decision #6).
The assistant addresses one project at a time, through the same
`projects/{name}/control-plane` path the authorization check already uses
(`internal/auth/sar.go:73`), and the project in that path is the *only* thing
that scopes the read. And on a cache hit, nothing on the request path leaves
the process.

---

## Why this is a cheap change

The `Source` interface is one method:

```go
type Source interface {
    Documents(ctx context.Context, projectName string) ([]CapabilityDocument, error)
}
```

Everything downstream — `ScopeDocuments`, `Compose`, `loadDocuments`,
`Entitlements`, `ProjectSkills`, the extended-card path — is written against
that seam and does not change. The work is one new implementation, one
constructor call in `internal/agentwiring/wiring.go`, the CRD manifest, the grant, and
the config enum. The seam was built for this; this is the third producer it
absorbs.

The seam is also what made the topology correction survivable. The design was
wrong about *how the documents are obtained* and right about everything else, so
the revision replaced the inside of one method — the types, the CRD, the
scoping change, and every consumer were untouched.

---

## API design

**Kind:** `CapabilityBinding`, group/version
`capabilities.assistant.miloapis.com/v1alpha1`, **cluster-scoped**.

The scope is the one design point most likely to look wrong to a Kubernetes
reader, so it is worth stating why. There is no namespace to use. A Milo project
is a virtual view over one apiserver, partitioned by etcd key prefix
(`milo/internal/apiserver/storage/project/mux.go:225`); a project plane has its
own ordinary namespace set (`milo-system`, `default`) and nothing anywhere
creates a namespace named after the project. A namespaced `CapabilityBinding`
would therefore force every producer to invent a namespace convention that
carries no meaning and that the storage layer does not check.

The tenancy axis is the **control-plane path**, not a field. `acme`'s bindings
are the objects reachable through `projects/acme/control-plane`, full stop. This
matches the catalog's analogous `AgentBinding`, which is already `scope=Cluster`
and whose controller writes `ObjectMeta{Name: agent.Name}` with no namespace
(`service-catalog, local branch feat/agent-framework-api (unpushed)`,
`internal/controller/agentbinding_controller.go:243-244`).

`metadata.name` is the binding's identity within a project — the catalog uses
the agent's name, so one entitled service yields one binding.

> **Superseded.** Earlier revisions of this document specified a namespaced kind
> with `metadata.namespace == project`, and built an argument on it: that a CRD
> object is "always namespaced" and therefore lets `ScopeDocuments` fail closed.
> The premise was false for this platform. The consequence is that
> `ComposeOptions.RequireProjectNamespace` — added for that argument — was
> **deleted rather than disabled**. With cluster-scoped objects there is no
> namespace to check, and a check that can only ever compare `""` against a
> project name is not defense-in-depth, it is a fail-closed trap waiting for
> the first binding that reaches it. Isolation for this source is structural;
> see [defect #5](#5-tenant-scoping-rests-on-a-convention-the-schema-cannot-enforce).

**`spec`** is the existing `CapabilitySpec`
(`internal/capability/document.go:113-131`) expressed as an OpenAPI v3 schema:
`serviceRef`, `serviceName`, `serviceAgentRef`, `configurationVersion`
(all required), plus optional `knowledge`, `tools`, `skills`, `authority`,
`reportingProject`. The field names are already the wire names; the JSON tags on
the Go structs are the schema. Required-ness mirrors `Validate()` exactly, so a
document that admission accepts is one `Validate()` will also accept.

**`status.conditions`** uses the standard `metav1.Condition` shape. The
assistant is the sole writer. Condition types:

| Type | True means | False means |
|---|---|---|
| `Accepted` | The spec parsed and validated; the binding is eligible to compose. | Validation failed — `reason`/`message` carry the same path-qualified error `Validate()` returns today (e.g. `spec.skills[0].source: required`). |
| `Composed` | The last turn that consulted this binding could use everything it declared. | An MCP endpoint refused by the operator dial allow-list; an MCP connect failure or timeout; a connected server whose `tools/list` failed; a declared include-list tool the server does not offer; a tool name already won by another binding in the project; or **total** knowledge loss (every declared source failed). |

Two things are deliberately **not** `Composed=False`, both for the same reason —
each leaves a working binding, and each would flap per turn, and a flapping
condition is a per-turn control-plane write. A knowledge source that fails while
its siblings succeed (and byte-cap truncation, where the knowledge arrived, just
less of it) keeps the condition True; only total loss flips it, which is almost
always a dead URL — steady, one write, and the one thing the provider can
actually fix. And a skill body that fails to fetch is never reported at all:
skill bodies are fetched lazily by `load_skill` *during* a turn, not at
composition, so no compose-time verdict for them exists.

`Composed` is the condition that pays for the whole exercise. Today a provider
whose MCP endpoint is unreachable learns nothing; `capability.mcp.connect_failed`
(`internal/capability/compose.go:375`) is a log line in someone else's service.
As a condition it is a `kubectl describe` away for the team that owns the
endpoint.

Status writes are a `PATCH` to the binding's `status` subresource through the
**same project control-plane path** the LIST uses, with the assistant's own
identity. The LIST that produced the document already yielded the object's name,
which — with the project in the path — is all a status patch needs.

**A note on the subresource axis, because it is a platform trap.** An earlier
version of this document said Milo's permission model "has no subresource
axis". That is not quite right, and the truth is worse. A subresource-flavoured
*convention* does exist and is used across `service-catalog` and `ipam-range`:
permissions named `updateStatus`. It is **inert** — `GetVerb()` never returns
`updateStatus`, so no SubjectAccessReview can ever match such a permission,
while plain `patch` silently confers the ability to write status. Anyone reading
a `ProtectedResource` would reasonably conclude that granting `updateStatus`
grants status writes and that withholding it withholds them; neither is true.
Our `capability-publisher` role therefore withholds `patch` outright rather than
relying on the convention (`config/milo/iam/roles/capability-publisher.yaml`),
which is correct by accident of not trusting it. This is a latent platform bug
worth reporting upstream; it is not this design's to fix.

The writes are **rate-limited and coalesced** — a turn must not issue an API
write per binding per message, and this matters more without an informer, since
there is no resync to fold updates into. Write only on transition (condition
type, status, or reason changed, tracked in a small in-memory map keyed by
project and object name), and never from the request goroutine: enqueue onto a bounded
channel drained by a single worker, and drop on overflow. A status update that
is lost is a cosmetic regression; a status update that blocks a chat turn is an
outage.

**Merge patch replaces the conditions array.** The write uses
`application/merge-patch+json` against the status subresource, and merge patch
has no notion of a keyed list — `listType=map` governs server-side apply, not
this. Every patch therefore carries the **full** condition set the writer knows
about, and two replicas can transiently clobber each other: a replica that has
only observed `Accepted` overwrites another's `Composed` until its own next
transition restores it. This is accepted rather than solved. There is
deliberately **no conflict-retry loop** — a retry loop on a status write is the
hot loop this section exists to prevent, and the next observation is a better
retry than an immediate one. `Accepted` is a pure function of the spec, so
redundant replica writes are byte-identical; only `Composed` can legitimately
differ, and its flap rate is bounded by the observation rate rather than
amplified by it. Server-side apply with per-condition field ownership is the
documented upgrade path if that ever stops being an acceptable trade.

---

## The source: a cached per-project LIST

`CRDSource.Documents(ctx, projectName)` resolves a cache entry for
`projectName`, and on a miss issues

```
GET {controlPlaneBaseURL}/apis/resourcemanager.miloapis.com/v1alpha1/projects/{projectName}/control-plane
    /apis/capabilities.assistant.miloapis.com/v1alpha1/capabilitybindings
```

No `namespaces/` segment — the kind is cluster-scoped, and the project in the
prefix is what scopes the read. It then converts each item's spec to a
`CapabilityDocument`, runs `Validate()`, and caches the survivors. The
conversion sets no namespace, because there is none to set; `ExpectedProject`
remains the turn's project and is what every downstream consumer (memory, gap
reports, identity forwarding) already keys on.

The addressing is not new machinery. `internal/auth/sar.go:73` already holds
`projectControlPlanePath` and `sarEndpoint` (line 80) already composes exactly
this prefix for the authorization check; the transport that carries it is
`newControlPlaneTransport` (`internal/auth/transport.go`). This source is the
second consumer of a pattern the service already runs on every request, which
is most of why it is cheap.

**The credential is the assistant's own, and it is a client certificate, not a
service-account token.** `internal/auth/transport.go` documents why at length:
Milo validates service-account tokens only against its own issuer, so a token
minted by the workload cluster is rejected with a 401 before the body is read,
and presenting a client certificate signed by the control-plane CA is the
supported way for an in-cluster service to identify itself. `SARConfig`
(`internal/auth/sar.go:149-165`) carries both `BearerToken` and
`ClientCert`/`ClientKey` for this reason. The capability source must take the
same configuration and make the same choice, and must not be described as
"using the service-account token" — that is the path that silently 401s.

**Failure is a degrade with a fallback, in this order:** a cached entry (fresh,
or stale if the LIST failed), then empty. Any transport error, non-2xx, or
decode failure is logged and falls back rather than propagating — the existing
posture in `docs/architecture/capabilities.md` ("Degrading, not failing") is
unchanged, only improved by having a stale answer to fall back *to*.

**Conversion, not reuse.** `pkg/apis/capabilities/v1alpha1` holds the KRM types;
`internal/capability` keeps its own env-free, client-free structs. The package
doc's claim that these types "carry no control-plane client"
(`internal/capability/document.go:1-11`) stays true: the client and the
conversion live in the new source file, not in the schema.

### TTL: 60 seconds, the same window authorization already accepts

Set the TTL to 60s, matching `DefaultSARCacheTTL` (`internal/auth/sar.go:37`).

The argument is consistency of risk appetite, not convenience. The platform
already accepts that a **revoked user keeps access for up to 60 seconds**,
because the SAR authorizer caches ALLOW for that long. Entitlement is the
weaker claim: a revoked *entitlement* that lingers 60 seconds means a project
can call a provider tool it no longer has a binding for — and that call still
has to pass the AI gateway's own allow-list and the provider's own authorization
on the way. Choosing a tighter TTL for capabilities than for authorization
would be claiming that stale entitlement is more dangerous than stale
authorization, which is not true. Choosing a looser one would be introducing a
new, larger staleness window without an argument for it. 60s is the number the
platform has already justified.

There is also direct precedent on the platform for choosing a TTL cache over a
watch in exactly this situation. `ipam` checks project namespace liveness with
one, and says why in the code (`ipam/internal/access/namespace.go:88-93`):

> A TTL cache rather than an informer, which would hold a watch and a full
> namespace cache per project IPAM has ever served.

ipam picked 10s (`liveTTL`) and bounds its cache at 4096 entries
(`maxLiveEntries`) — the same bound `allowCache` uses. We pick 60s rather than
10s because ipam is gating an allocation write on a liveness fact that can
change abruptly, whereas we are reading configuration that changes at the
cadence of an entitlement edit; and because 60s is the number this service has
already justified for authorization. Either choice is defensible; what matters
is that the platform has twice concluded a TTL cache is the right shape here,
and this is the third.

Revocation latency is therefore the TTL, not ~0. That is the honest cost of
declining the watch, and it is why deletion semantics matter on the catalog side
(see below): the binding must actually be deleted, so that the next LIST after
the TTL returns without it.

### Never cache an empty result

Copy the SAR cache's asymmetry, adapted. `allowCache` stores ALLOW and never
DENY (`internal/auth/sar.go:401-406`), so a just-granted user is never locked
out by a cached negative. ipam reached the identical conclusion independently —
*"Only Live is cached. Caching a refusal would keep failing real claims for a
state that no longer holds"* (`ipam/internal/access/namespace.go:89-91`) — which
makes this a platform convention rather than a local preference. The capability
analogue of a DENY is an **empty result**, and it should likewise not be
cached:

- **Empty is the indistinguishable state.** An empty LIST is what a project
  with no bindings returns, and also what a project returns during the window
  between the catalog creating its first binding and that write landing. Caching
  empty pins a newly-entitled project into a built-ins-only assistant for up to
  a TTL, for no reason the user can see — which is defect #2 recreated inside
  the fix for defect #2.
- **The cost of not caching it is bounded and small.** Projects with no
  bindings are the ones with the least assistant traffic, and the miss costs one
  LIST against a control plane that is otherwise idle for them. A project with
  bindings — the expensive case — always caches.
- **It is also the safe direction.** Not caching empty means the cache can only
  ever hold a *positive* entitlement set, so the failure mode of the cache
  layer is "we re-fetched unnecessarily", never "we withheld a capability the
  project has".

Stale-serving is unaffected: an entry that exists and has expired is still
usable when a refresh LIST fails. The rule is narrower than it sounds — *do not
store an empty result as a fresh entry*, not *discard what you have*.

### Making staleness observable

A cache that serves stale must say so, or a control-plane outage looks like
quiet. Proposed metrics, following the existing `assistant_` naming in
`internal/metrics/metrics.go`:

- **`assistant_capability_fetch_total`** — labeled `outcome` =
  `hit|miss|refresh_failed|stale_served|empty`. One counter answers cache
  effectiveness and outage behavior. `stale_served` is the alertable one; a
  nonzero rate means the control plane is failing and users are on old config.
- **`assistant_capability_fetch_duration_seconds`** — histogram over the LIST,
  labeled `outcome` = `ok|error`. The latency this design claims to remove from
  the common path is only provably removed if the uncommon path is measured.
- **`assistant_capability_cache_entry_age_seconds`** — histogram, observed at
  serve time, of the age of the entry served. A histogram rather than a gauge
  because there is no single "the cache" age once entries are per project; the
  p99 is the number an operator wants.
- **`assistant_capability_cache_entries`** — gauge, resident entry count. Feeds
  the eviction discussion below.

**No per-project label on any of these.** Project cardinality is set by tenant
count, which this repository does not control and cannot bound — the same
reasoning that keeps project out of the existing metric labels. An operator
debugging one project uses logs, which already carry `projectName`, not a time
series per tenant.

**Degradation does not surface to the user.** The turn proceeds with stale
config and says nothing, exactly as it proceeds today with no config and says
nothing. Telling a user "your capability configuration may be out of date" is
noise they cannot act on, and it leaks operational state of the platform into a
customer conversation. The audience for staleness is the operator, and the
channel is the metric. (This is a deliberate limitation: a long-running cache
divergence is invisible in the product. The mitigation is an alert on
`stale_served`, not a chat message.)

---

## The cost, revisited: what the platform answers removed, and what they added

An earlier draft of this document carried a long section arguing that the change
was not obviously good, because a cluster-wide informer would hold **every
tenant's** capability configuration in one address space, and would demote
`ScopeDocuments` from defense-in-depth to the sole isolation boundary. That
argument was sound for the design it described. It no longer applies, and the
reason is worth recording rather than quietly deleting: the cost was an artifact
of the wrong topology assumption, not of the CRD.

Reading one project at a time restores the property the informer would have
given up. The assistant fetches `acme`'s bindings from `acme`'s control plane,
under a request addressed to `acme`; another tenant's config is not in the
response, not in the process, and not one call away. Blast radius returns to
roughly what the HTTP source had.

**Isolation for this source is structural, and that is stronger than the check
it replaces.** The per-project LIST is not merely *filtered* by project — it is
served from an etcd keyspace prefixed `/projects/acme`
(`milo/internal/apiserver/storage/project/mux.go:225`). Another tenant's
bindings are not excluded from the response; they are not in the store being
read. There is no code path in the assistant, correct or buggy, that turns
`Documents(ctx, "acme")` into another project's configuration, because the
project name is consumed by the URL before any of our logic runs.

`ScopeDocuments` survives untouched and stays necessary — for the fixture and
HTTP sources, where a namespace field is a producer's convention and the check
is the only thing behind it. For the CRD source it is a no-op, because
cluster-scoped objects have no namespace to compare.

**`RequireProjectNamespace` is deleted, not disabled**, and this document should
not pretend that was the plan. It was added on the belief that `CapabilityBinding`
objects would be namespaced by project and that the CRD source would therefore
make the namespace gate fail closed. The belief was wrong. Kept as-is against
cluster-scoped objects it would reject every binding; kept and special-cased it
would be a permanently-false branch documenting a platform fact incorrectly.
Deleting it is the honest outcome of finding out that the isolation it was meant
to harden is enforced a layer below us.

What remains true, and is a real if modest change: **the assistant now reads
project configuration, where before it read none.** `config/base/rbac.yaml`'s
comment — "no read access to any project resource ... it never gains the
caller's authority" — needs amending, because the first clause stops being
literally true. The second clause survives intact and is the one that carries
the weight: reading a project's `capabilitybindings` with the assistant's own
identity is not the same as acting with the caller's authority, and nothing
here forwards or borrows a user's credential.

### The grant: root RBAC, because IAM structurally cannot express it

This was an open question in the previous revision, flagged as the most likely
reason the design would not ship. It is resolved, and the answer is not the one
the previous revision expected.

**Milo IAM cannot authorize the assistant's identity at all.** Not "has no role
for it" — cannot. The OpenFGA authorizer requires a user UID and errors out
without one (`openfga-provider/internal/webhook/subjectaccessreview_authorizer.go:333-336`),
and it never consults groups (`GetGroups()` appears zero times in that file).
The assistant authenticates to Milo with an x509 client certificate
(`internal/auth/transport.go`; CN `agent@assistant.datumapis.com`,
`config/milo/rbac/control-plane-auth-delegator.yaml`), and certificate
authentication supplies a CN and organizations — never a UID. `PolicyBinding`
subjects require a UID for the same reason. **No `PolicyBinding` can name this
identity.** That is why every other
platform service that talks to Milo carries `O=system:masters` and rides RBAC
instead.

The grant is therefore an extension of the ClusterRole the assistant already
has at Milo root — `assistant:control-plane-auth-delegator`, today
`create` on `tokenreviews` and `subjectaccessreviews` — with `get`/`list` on
`capabilitybindings` and `patch` on `capabilitybindings/status`.

It works because the project router strips the
`projects/{p}/control-plane` prefix and **recomputes `RequestInfo` before
authorization runs** (`milo/pkg/server/filters/projects.go:40-62`): by the time
the authorizer sees the request it is an ordinary root-scoped request for
`capabilitybindings`, and root RBAC answers it.

Two things must be said plainly about this.

**First, it is a platform-wide grant.** Root RBAC has no project axis, so this
authorizes the assistant to read `capabilitybindings` in *every* project plane,
not only the ones it serves. That is a broader grant than the previous
revision's per-project IAM sketch would have been, and it partially re-creates
the reach that revision congratulated itself on avoiding. What it does not
re-create is the *aggregation*: authority to read every project one at a time
is not the same as every project's config resident in one address space, and
the TTL cache holds only projects with live traffic. The grant is also bounded
to two kinds and three verbs, which is materially better than the
`system:masters` that house practice would have handed us.

**Second, the load-bearing claim is under-evidenced.** "A root
`ClusterRoleBinding` authorizes requests arriving on project paths" is supported
by the filter code above and by a comment in a `service-catalog` **test**
overlay. It is not verified in production, and nothing in this repository
exercises it. If it is false, the assistant gets a 403 on every capability LIST
and every project degrades to built-ins — which the `stale_served` and
`refresh_failed` metrics would show immediately, but only after the flip. The
fallback if it does not hold is the catalog-side adapter
([below](#keep-the-http-adapter-make-it-the-crd-watcher)), which needs no such
grant.

### The certificate must not carry `system:masters`

A 2026-09-12 decision to give the runtime certificate `O=system:masters` for
the initial rollout — sidestepping the confirmation above — was **reversed**.
Two findings did it.

First, the decision was already made, in the place the certificate is actually
issued. `datum-cloud/infra`
`apps/patch-assistant/base/assistant.yaml:204-207`:

> *"Deliberately NO `organizations: system:masters` here, unlike activity: this
> identity is granted exactly create on tokenreviews and subjectaccessreviews by
> config/milo/rbac in the assistant bundle, and nothing else."*

The same file contrasts it with the aggregated apiserver's separate identity,
which does carry the group. Adding it here means deleting that comment.

Second, the narrow grant is not the risky path — it is **the house pattern for a
narrow grant**. Three production ClusterRoles in infra do exactly this shape, a
certificate identity bound by a root ClusterRole, cross-project by construction:
`nso-cell-ipam`, `milo-ipam`, and `billing-offer-snapshot-writer`. Two of them
state the cross-project consequence about themselves in comments, and
`billing-offer-snapshot-writer` gives this design's reasoning almost verbatim:
*"IAM User subjects require a backing User object the mutator can resolve, and
that identity is never registered."*

The interim would have bought one API call's worth of schedule — the 200-vs-403
LIST — at the cost of running a process that composes provider-authored prose
with cluster-admin on every project plane. It is recorded here rather than
deleted because the reasoning that produced it was sound given what was known:
it looked like the only alternative to a question nobody could answer. The
question turned out to be well-evidenced already.

### Cache bounds

Memory now scales with **active projects**, not with the catalog's total object
count, which is a far better shape: it is bounded by traffic the assistant is
already serving. Still bound it explicitly rather than trusting that shape, the
same way `allowCache` does with `maxSARCacheEntries = 4096`
(`internal/auth/sar.go:39-41`): a fixed maximum entry count, expired entries
swept first on overflow, then one live entry evicted. Reuse that eviction
policy rather than inventing a second one — an operator who has reasoned about
the SAR cache's behavior should not have to reason separately about this one.

A cache entry is a project's documents: a few kilobytes each, dominated by
knowledge URLs and tool allow-lists. At 4096 entries that is single-digit
megabytes, and the eviction path is exercised only by deployments with more
than 4096 concurrently-active projects, which is a good problem.

---

## Rejected alternatives

### A cluster-wide in-process informer

This was the approved design until the topology question was asked, so the
reasoning is recorded in full: it is genuinely attractive, and it will be
proposed again by anyone who arrives at this problem from Kubernetes habit.

What made it attractive: revocation latency of approximately zero, no TTL to
argue about, no per-turn network call at all, an obvious place to hang status
writeback, and a mechanism every Kubernetes engineer already knows. It answers
defect #1 outright rather than amortizing it.

What killed it is a fact about Milo, not a trade-off: **a project is a virtual
project control plane, not a namespace in the assistant's cluster.**
`CapabilityBinding` objects live behind
`/apis/resourcemanager.miloapis.com/v1alpha1/projects/{name}/control-plane`
(`internal/auth/sar.go:73`), which is exactly why every authorization check in
this service is already addressed that way. A cluster-wide
`get/list/watch` against `kubernetes.default.svc` would be well-formed, would
establish successfully, would report its cache synced — and would observe zero
objects, forever, for every project. The failure would present as "every project
is entitled to nothing", which is precisely the silent failure mode defect #2
exists to eliminate.

This is not recoverable by widening RBAC or fixing a selector, and it is not
recoverable by pointing the informer at Milo's root either: **there is no
cross-project LIST.** The storage mux resolves a request to the named project's
child store or to the root child, with no wildcard across projects
(`milo/internal/apiserver/storage/project/mux.go:318-323`). Reading more than
one project's bindings is always an N-way fan-out, whatever the mechanism. Any
future proposal to watch `capabilitybindings` cluster-wide has to first explain
what changed about that.

### One informer per project control plane

This is the **dominant pattern on this platform**, and a previous revision of
this document understated it badly — it described the machinery as something the
assistant would have to invent. It would not. `sigs.k8s.io/multicluster-runtime`
with Milo's provider does exactly this: it stands up one `cluster.Cluster` per
project, engaged when the `Project` object goes Ready, with the control-plane
path composed for it
(`milo/pkg/multicluster-runtime/milo/provider.go:249-267`). Project discovery,
connection lifecycle, and reconnect are the provider's job, not ours. Every
other controller on the platform that needs project-scoped objects works this
way.

So the TTL cache is a **minority position taken knowingly**, not the only road.
The reason is fan-out cost, not impossibility: one watch and one full cache per
project the assistant has ever served, held open in a process whose primary job
is serving chat, to buy back at most 60 seconds of revocation latency on a
resource whose downstream effects are independently gated at the AI gateway.

We are not the first to weigh this and decline. ipam reasoned about the
identical trade and wrote the conclusion into the code
(`ipam/internal/access/namespace.go:91-93`):

> A TTL cache rather than an informer, which would hold a watch and a full
> namespace cache per project IPAM has ever served.

The shapes match closely enough that following ipam is the conservative choice
and the watch is the adventurous one. Worth revisiting if revocation latency is
ever shown to matter in practice — the framework is there, and adopting it later
costs one `Source` implementation — but it should be driven by that evidence,
not by a preference for watches.

### Keep the HTTP adapter, make *it* the CRD watcher

Leave `HTTPSource` in place and have the catalog's capability-provider service
watch `CapabilityBinding` on the assistant's behalf, continuing to serve
`GET /projects/{projectName}/capability-documents` from its own cache. Note that
this alternative *survives* the topology correction — the adapter is a catalog
component and may well sit closer to those control planes.

What it preserves: the assistant reads no project resource at all, needs no
grant at Milo root, and `config/base/rbac.yaml`'s comment stays literally true.
That mattered less when the grant looked like narrow per-project IAM; it matters
again now that the grant is platform-wide root RBAC resting on an
under-evidenced claim (see [the grant](#the-grant-root-rbac-because-iam-structurally-cannot-express-it)).
This is the live fallback, not a courtesy entry.

What it costs is the other defects. The config plane stays on the latency path
(defect #1) — a cache in someone else's process is still a network hop and still
a 5s timeout, and the assistant cannot serve stale through an outage *of the
adapter*, which is the failure actually observed (defect #2). Status writeback
would have to travel back over a second, invented HTTP verb (defect #3).
Validation stays at read time unless the CRD is adopted anyway (defect #4). And
tenant isolation stays a string comparison against a producer's convention
rather than an etcd prefix (defect #5).

It also keeps a service in another team's repository on this service's critical
path — the thing the contract inversion in `docs/architecture/capabilities.md`
exists to avoid. It is the fallback if the root-RBAC claim does not hold in
production, and adopting the CRD now
does not foreclose it: the adapter would read the same kind.

---

## Configuration

`internal/config/config.go:305-313` currently rejects setting both
`CAPABILITY_DOCS_FIXTURE` and `CAPABILITY_PROVIDER_URL`, with a pairwise
exclusion check and a pairwise error message. The source selection (since main
moved agent construction into `internal/agentwiring`, in `wiring.go`; it was
`cmd/assistant/runner.go`) chose between them with a `switch` whose `default`
warned about exactly two names. Neither extends to a third mode without becoming a combinatorial mess.

Replace the implicit selection with an explicit one:

```
CAPABILITY_SOURCE=fixture|http|crd   # default: unset ⇒ no source (today's behavior)
CAPABILITY_DOCS_FIXTURE=<path>       # required when CAPABILITY_SOURCE=fixture
CAPABILITY_PROVIDER_URL=<url>        # required when CAPABILITY_SOURCE=http
```

Validation becomes per-mode: an unknown enum value is an error; a mode whose
companion variable is missing is an error; a companion variable set for a mode
that does not use it is an error (it is almost always a half-finished overlay
edit, and failing on it is how the operator finds out). The `crd` mode takes no
companion variable of its own: the control-plane base URL, CA bundle, and client
certificate it needs are the ones the SAR path is already configured with
(`SARConfig.APIURL`/`CACert`/`ClientCert`/`ClientKey`,
`internal/auth/sar.go:149-165`), and reusing them is deliberate — two ways to
name the same control plane is two ways to point half the service at the wrong
one. The cache TTL is a constant, not configuration, for the same reason
`DefaultSARCacheTTL` is.

For one release, infer the mode when `CAPABILITY_SOURCE` is unset and exactly
one of the two companion variables is set, and log at warn. That keeps existing
overlays and `e2e/run-e2e.sh` working through the transition. Drop the inference
after the overlays are updated.

---

## Deployment, RBAC, and overlays

**CRD manifest — installed once, at Milo root.** This was an open question in
the previous revision ("a kind must be registered where its objects live; does a
project plane inherit CRDs from a parent?"). It is resolved: **CRD definitions
are global in Milo, and only instance data is partitioned.** The storage layer
says so explicitly, returning unwrapped REST options for
`customresourcedefinitions` while decorating everything else with the
project-aware decorator (`milo/internal/apiserver/storage/project/restoptions.go:35-38`):

> 🔒 Leave CRD *definitions* global so discovery is shared cluster-wide

So one apply of `capabilitybindings.yaml` at Milo root makes the kind
discoverable and storable in every project plane. There is no per-project
registration step, and no kcp-style `APIExport`/`APIBinding` to model — Milo has
no kcp dependency.

The consequence is that the manifest **moves out of the workload cluster's
`config/base`** and into `config/milo`, delivered by a Flux `Kustomization` with
`kubeConfig.secretRef.name: milo-configuration-kubeconfig`, modelled on
`infra/apps/billing-system/base/milo-control-plane.yaml`. Shipping it in
`config/base` would install the kind in the cluster where the assistant runs —
which is the one place no `CapabilityBinding` will ever exist.

The earlier argument for `config/base` — that a missing CRD fails silently, so
the schema should ship wherever the service ships — still holds as a *concern*
and is now answered by observability rather than by co-location: a LIST against
a plane with no such kind returns 404, which surfaces as `refresh_failed`
rather than as a plausible-looking empty result.

**RBAC.** See [the grant](#the-grant-root-rbac-because-iam-structurally-cannot-express-it):
`get`/`list` on `capabilitybindings` and `patch` on `capabilitybindings/status`,
added to the existing `assistant:control-plane-auth-delegator` ClusterRole at
Milo root (`config/milo/rbac/control-plane-auth-delegator.yaml`), **not** via
Milo IAM, which cannot bind a certificate identity.

**IAM registration is still required, for the producers.** The `ProtectedResource`
at `config/milo/iam/resources/capabilitybindings.yaml` is what lets Milo
authorize the *catalog's* writes at all — the authorizer fails closed on an
unregistered resource and rejects any verb not listed. The `capability-viewer`
and `capability-publisher` Roles grant project members and the catalog's
automation respectively. None of that authorizes the assistant; the two axes
coexist because they serve different subjects.

The status-write assumption is instrumented rather than merely asserted: a
rejected status write logs `capability.crd.status_forbidden` and increments
`assistant_capability_status_write_total{outcome="forbidden"}`, kept separate
from generic write errors, so a wrong assumption diagnoses itself in minutes
instead of presenting as "conditions just never appear". Chats are unaffected
either way.

`config/base/rbac.yaml` keeps no `capabilitybindings` grant at all, and its
comment says why: a ClusterRole in the workload cluster would apply cleanly,
audit cleanly, and authorize nothing real, which invites the next maintainer to
believe tenant isolation is an RBAC matter there. It is not.

**Admission (optional, follow-up).** The OpenAPI schema covers structure but not
the URL/host rules the SSRF guard enforces at compose time
(`internal/capability/compose.go`, `newIPGuard`/`parseHostAllowList`). A
validating webhook could reject a binding whose `knowledge.sources[].url` or
`mcpServers[].endpoint` resolves outside the operator's allow-list, at apply
time rather than at turn time. This is genuinely nice — it is the same
"validate at write, once" argument as defect #4 — but it is a second deployment
with its own certificate lifecycle, and the runtime guard is not being removed
either way. Out of scope for the first pass; noted so it is not re-derived. The
related and *non*-optional check — that MCP endpoints resolve to the AI gateway
rather than a provider directly — is argued under
[the catalog's obligations](#the-assistant-should-not-trust-the-projection) and
is deliberately placed at composition time, not at admission.

**Overlays.** Three coexisting modes, one per source:

| Overlay | Mode | Notes |
|---|---|---|
| `config/overlays/dev` | `fixture` | Unchanged. The standalone story; no cluster config needed. |
| `config/overlays/dev-catalog` | `http` | Unchanged. Keeps the adapter path exercised while the CRD path is proven. |
| `config/overlays/dev-crd` (new) | `crd` | Seeds `CapabilityBinding` manifests mirroring the StreamCo fixture. In kind there is one apiserver and no project router, so the project path resolves to the same server and root RBAC trivially applies — which makes dev a *weaker* proof than usual here. It exercises the object model and the cache; it does not exercise the claim the production flip is gated on. Say so in the overlay's header. |
| `config/overlays/production` | `http` → `crd` | Flipped last: after `dev-crd` is green, the catalog's projection controller has landed on `main`, and the root-RBAC-authorizes-project-paths claim is confirmed against a real Milo. |

`dev-crd` should seed its bindings from manifests in the overlay rather than
depending on the catalog, so the CRD path is testable in this repository alone —
the same standalone property the fixture gives us today, one layer up.

---

## What the service catalog team must build

This change has a cost outside this repository, and it is a hard dependency for
production rollout.

The catalog must run a **projection controller**: watch its own `AgentBinding`
objects, and for each one that entitles a project, create/update a
cluster-scoped `CapabilityBinding` in that project's control plane with the
projected spec.

**Status of that work, stated precisely, because this document has twice
described it loosely.** It is not shipped, and it is not a playground toy. The
projection controller — API types, reconciler, webhooks, validation, samples,
and tests — is real work on the **local, unpushed branch `feat/agent-framework-api` in
`milo-os/service-catalog`** (head `ed94982`, *"test: cover agent reconcilers,
projection gates, and admission rules"*). It is owned by another team, and the
production flip depends on it landing on `main`. Earlier revisions of this
document said the catalog "already performs this projection"; what already
exists is the HTTP adapter's projection into the wire shape
(`docs/capability-reference.md`, "Capability provider API"), plus this branch.
The change for them is the sink: a `CapabilityBinding` write instead of an HTTP
response body.

The branch is also the source of the object-model precedent this design follows
— `AgentBinding` is `scope=Cluster`, and `ensureAgentBinding` writes
`ObjectMeta{Name: agent.Name}` into the consumer's client with no namespace
(`internal/controller/agentbinding_controller.go:243-244`). Matching it is
deliberate: one projection controller writing two kinds with two different
tenancy models would be a bug waiting to happen.

Concretely, the catalog owns: ownership/finalizer semantics (a revoked
entitlement must **delete** the binding, not merely stop serving it — this is
the revocation path, and the one place where the cache's staleness has security
weight: a binding that is deleted stops being served after one TTL, whereas a
binding merely abandoned is served indefinitely); the `reportingProject` resolution it already does; **MCP
endpoint rewriting** (below); and reading the `status.conditions` this service
writes, so a projection that the assistant rejects is visible on the catalog
side too.

The catalog must **not** write `status` — the assistant is the sole status
writer. Two writers on one status block is a hot loop.

### MCP endpoint rewriting is part of the projection

The adapter does a transformation today that is easy to miss because it is
invisible in the assistant: it rewrites each `mcpServers[].endpoint` to the
**AI gateway's MCPRoute URL**, not the provider's own address
(`docs/capability-reference.md:236-239`). The dev fixture shows the result —
StreamCo's entry points at
`http://patch-ai-gateway.envoy-gateway-system.svc.cluster.local:80/mcp`
(`config/overlays/dev/capability-documents.json:41`), which is the gateway.
The projection controller must do the same rewrite. Three independent
properties break if it writes a raw provider endpoint into
`CapabilityBinding.spec`:

1. **The second allow-list enforcement point disappears.**
   `docs/architecture/capabilities.md` ("Allow-lists are enforced twice") calls
   the gateway check "the one a compromised or misconfigured assistant cannot
   bypass", and `config/components/streamco/mcp-route.yaml:1-5` is explicit
   that the MCPRoute enforces the reviewed allow-list *at the gateway* — it
   also notes that StreamCo's unlisted `streams_delete` remains reachable on a
   direct-to-StreamCo connection. A raw endpoint leaves only the client-side
   check in `Compose`, which is precisely the check the architecture doc says
   must not stand alone.
2. **Metering breaks.** Provider tool invocations reach billing by passing
   through the gateway. A direct connection is an unbilled connection.
3. **Caller-identity forwarding silently stops.**
   `config/base/assistant.yaml:75-76` sanctions exactly one host in
   `CAPABILITY_IDENTITY_FORWARD_HOSTS` — the gateway — and `identityHeaders`
   (`internal/capability/identity.go:43-62`) fails closed on any host outside
   it. A raw provider endpoint therefore receives no credential, and every
   read-as-the-caller tool starts failing in a way that reads like a provider
   bug rather than a projection bug.

Note that this is a **new** failure mode, not an inherited one. With the HTTP
source, the component that rewrote the endpoint and the component that served
it were the same process: the two could not drift. Splitting projection
(catalog) from consumption (assistant `CRDSource`) introduces a gap where a
controller regression can publish an un-rewritten endpoint that the assistant
will faithfully dial.

### The assistant should not trust the projection

The assistant is about to dial an endpoint chosen by an out-of-repo controller
(see [the cost, revisited](#the-cost-revisited-what-the-platform-answers-removed-and-what-they-added)
for what this change does and does not alter about the trust model). `CapabilityBinding.spec.tools.mcpServers[].endpoint` is now an
attacker-relevant field that an out-of-repo controller writes and this service
dials — with the user's bearer token attached whenever the host happens to
match. The assistant must enforce the gateway constraint itself, not assume it.

**Recommendation: an operator-configured MCP endpoint host allow-list, checked
at composition time, reported through `Accepted`.** A new config value —
`CAPABILITY_MCP_ENDPOINT_HOSTS`, the same comma-separated host-list shape as
`CAPABILITY_IDENTITY_FORWARD_HOSTS` — is parsed with the existing
`parseHostAllowList` (`internal/capability/urlguard.go:69`) and consulted in
`connectTools` before dial: an endpoint whose host is outside the list is
skipped, exactly as an endpoint that fails connect is skipped today, and the
binding gets `Composed=False` with a reason naming the unsanctioned host.
**Not `Accepted=False`:** the spec is perfectly valid, and saying otherwise
would tell a provider to go looking for a schema error that is not there. The
verdict is about dial policy, so it belongs on the condition that reports what
this turn could actually use.
Empty means disabled, so dev and the fixture path are unaffected; production
sets it to the gateway.

Three notes on why this specific shape:

- **Not `ComposeOptions.AllowedHosts`.** The SSRF guard's allow-list posture
  already exists and is unwired in the default config, which makes it the
  tempting reuse. But it governs *all three* provider-URL sinks — knowledge
  sources, skill bodies, and MCP endpoints alike
  (`internal/capability/compose.go:88-101`). Setting it to the gateway would
  confine knowledge and skill fetches to the gateway too, which is not the
  policy anyone wants: those legitimately point at providers' own
  documentation hosts. The constraint being expressed here is narrower than
  SSRF — "MCP traffic goes through the gateway" — and it deserves its own
  knob rather than a repurposed one.
- **Not a reuse of `CAPABILITY_IDENTITY_FORWARD_HOSTS` either**, even though
  in practice it will hold the same value. That variable means "may receive
  the caller's credential"; this one means "may be dialed at all". Collapsing
  them means an operator who widens one silently widens the other, which is
  how a credential-forwarding list grows an entry nobody reviewed.
- **Not a CRD validation rule** as the primary defense. The sanctioned gateway
  host is a deployment fact that differs per cluster, not a schema fact;
  encoding it in a cluster-scoped CRD's CEL rules hard-codes topology into the
  API. More decisively, admission defends against bad *writes*, and the writer
  we are defending against is the controller with write access to these
  objects. The check belongs where the connection is opened, because that is
  the step an attacker cannot route around.

The status condition is the *reporting* channel, not the enforcement — which
is the whole point of defect #3 applied to this case: a projection regression
that would otherwise manifest as "identity forwarding mysteriously stopped"
instead manifests as a `Composed=False` condition naming the wrong host, on
the object the catalog team owns.

---

## Phasing

Two corrections have landed against this plan, and they cut differently.
Revision 1 (topology) invalidated the read path and nothing else — phase 2 was
reverted and re-specified, and the object model was untouched. Revision 2
(cluster scope + the grant) does the opposite: the read path's *shape* survives,
but the object model, the scoping code, and the manifest destination all move.

Phases 4 through 6 are implemented and unaffected. Phases 1, 2 and 3 are
**re-opened** by revision 2 and were revised together, because a cluster-scoped
kind, a LIST path with no `namespaces/` segment, and the deletion of
`RequireProjectNamespace` are one change wearing three hats. All three have
since landed.

1. **CRD types + manifest — DONE (revised).** Go types, deepcopy, the
   `CustomResourceDefinition` with the OpenAPI schema derived from
   `CapabilitySpec`, the `ProtectedResource` and viewer/publisher Roles: all
   written, all correct except the scope. Change `scope: Namespaced` →
   `Cluster` and drop `+kubebuilder:resource` namespacing on the Go type; move
   the manifest's delivery from `config/base` to `config/milo` behind a Flux
   Kustomization with `milo-configuration-kubeconfig`. Deliverable: one apply at
   Milo root, and `kubectl get capabilitybindings` through a project path
   returns that project's objects.
2. **`CRDSource` + TTL cache — DONE (revised).** *(An earlier informer
   implementation was reverted and re-specified after revision 1.)* Implemented in
   `internal/capability/crdsource`, which keeps the client, TLS material, and
   KRM conversion on the far side of `internal/capability`'s client-free
   promise: a project-scoped LIST against the
   control-plane path, spec→`CapabilityDocument` conversion, the TTL cache with
   serve-stale and no-cache-on-empty, bounded eviction, and the four metrics.
   Reuse the SAR transport and credentials rather than building a second client.
   Deliverable: unit tests against a fake HTTP control plane covering hit, miss,
   expiry, stale-on-error, empty-not-cached, and eviction; the source returns
   the right documents for the right project and nothing for an unknown one.
   Revision 2 changes two lines of it: drop the `namespaces/{project}` segment
   from the LIST path, and stop setting `Metadata.Namespace` on the converted
   document. Add a test asserting the request URL has no `namespaces/` segment —
   it is the cheapest possible guard against this being silently re-added.
3. **Delete `RequireProjectNamespace` — DONE (was: fail-closed scoping).**
   The option, its plumbing, and its tests come out; `ScopeDocuments` itself
   stays untouched for the fixture and HTTP sources. Fixtures may keep
   `metadata.namespace` — it is still meaningful there. Deliverable: no code
   path compares a `CapabilityBinding`'s namespace to anything, because there
   isn't one.
4. **Config + wiring — DONE.** `CAPABILITY_SOURCE` enum, per-mode validation, the
   runner's `switch` constructing `CRDSource` from the same control-plane
   config the SAR authorizer uses, `config/overlays/dev-crd`. Deliverable:
   `task dev:deploy OVERLAY=dev-crd` yields a chat with StreamCo's tools,
   sourced from the cluster.
5. **Status writeback — DONE.** The coalescing worker, `Accepted` (from
   `convert`) and `Composed` (from `ComposeOptions.OnDocumentComposed`).
   `kubectl describe capabilitybinding` shows why a broken binding is not
   composing.
6. **Gateway endpoint enforcement — DONE.** `CAPABILITY_MCP_ENDPOINT_HOSTS`, the check
   in `connectTools`, and the `Composed=False` reason for an unsanctioned host;
   set it on the gateway host in `config/base/assistant.yaml` beside the
   existing `CAPABILITY_IDENTITY_FORWARD_HOSTS`. Ordered after phase 5 because
   the condition is how the rejection is reported, and must ship with the
   production flip rather than after it — an un-rewritten endpoint is only
   reachable once the catalog controller is the producer.
7. **Tests + docs — OUTSTANDING.** A chainsaw e2e under `test/e2e/` mirroring
   `test/e2e/chat-smoke` but seeding a `CapabilityBinding` instead of a fixture,
   plus one that seeds a binding in a *different project* and asserts it is not
   composed (the isolation proof — note this now tests the control plane's
   partitioning, not our filtering, and in a single-apiserver kind cluster it
   may not be testable at all; say so rather than writing a test that proves
   nothing), plus one that seeds a binding with a raw provider endpoint and
   asserts it is neither dialed nor credentialed (the projection-regression
   proof). Update `docs/architecture/capabilities.md`,
   `docs/capability-reference.md`, and `docs/configuration.md`.

Production flip is a separate, reversible step, with two gates.

1. **The catalog's projection controller landing on a pushed branch.** It is
   real, tested work, but it currently exists only as a local branch on one
   machine.
2. **One LIST, 200 vs 403**, with the assistant's certificate against a project
   control-plane path. The mechanism is well-evidenced — the router's
   installation point plus two production overlays asserting the behaviour of
   their own grants — but it has not been observed for *this* identity, and it
   is cheap to observe.

The third previous gate is closed by evidence rather than work: CRD
registration is a single root apply, and `config/milo/` already reaches Milo
through an existing Flux Kustomization.

`CAPABILITY_SOURCE=crd`, with `http` one env-var edit away for a rollback.

---

## Environment hazards

Operational facts a reader will otherwise discover the hard way. None blocks
this work; all of them mislead.

- **Two Milo architecture documents describe a design that no longer exists.**
  `milo/docs/architecture/controllers/project-controller/README.md` and
  `infra/apps/datum-control-plane-system/README.md` both describe a
  per-project-apiserver model that has been removed in favour of the single
  partitioned apiserver. Do not cite either as evidence about topology; read
  `internal/apiserver/storage/project/` instead. (A residue of the old model is
  still live in code: the multicluster provider has an
  `InternalServiceDiscovery` mode that addresses
  `milo-apiserver.project-{name}.svc.cluster.local` directly rather than through
  the project path — `milo/pkg/multicluster-runtime/milo/provider.go:257-260`.
  The path-based branch is the one that matters for us.)
- **Milo runs from an unreleased feature branch in both staging and
  production.** `infra` pins `ghcr.io/milo-os/milo` to
  `newTag: manage-project-control-planes` for both the apiserver and the
  controller-manager, in `base/` — so every environment inherits it
  (`infra/apps/datum-control-plane-system/core-control-plane/base/milo-system/{apiserver,controller-manager}-kustomization.yaml`).
  Behaviour verified against `main` is not necessarily the behaviour deployed.
- **Our Milo projection may not be reaching Milo at all today.**
  `config/milo/kustomization.yaml` and
  `config/milo/rbac/control-plane-auth-delegator.yaml` both direct the reader to
  `apps/patch-assistant/...` in `datum-cloud/infra` as the Flux Kustomization
  that applies this bundle. **That path does not exist in the `infra`
  repository.** Either it lives somewhere this investigation did not look, or
  the assistant's `ProtectedResource` and `ClusterRole` have never been applied —
  in which case SAR-mode authorization is running on something other than what
  this repo believes, and the capability grant will have no delivery mechanism
  when it is added. This is a live bug to chase independently of this design,
  not a blocker for it.

---

## Non-goals

- **Deleting `FixtureSource` or `HTTPSource`.** Both stay. The fixture is the
  standalone story the README sells and the substrate for `e2e/`; the HTTP
  source is the rollback path and remains a supported producer of the same
  schema.
- **Serving capability bindings through the aggregated apiserver.** A CRD is the
  right tool: the storage is etcd, the schema is static, and there is no
  relational backing store to view over. The aggregated server exists because
  conversations *do* have one.
- **Reading anything else from a project control plane.** This grants read on
  one kind. It is not the beginning of a general "the assistant reads project
  state" capability. The grant is platform-wide root RBAC, so adding a kind to
  it is a one-line change with no project-level check to stop it — which is
  exactly why any such proposal must re-argue the grant from scratch rather than
  inherit this one's answer.
- **Real-time revocation.** Revocation takes effect within the TTL, not
  immediately. Closing that gap means watches; see
  [Rejected alternatives](#rejected-alternatives).
- **A validating webhook** (see Deployment). Deferred, not rejected.
- **Per-turn cache invalidation hints.** No cache-busting header, no forced
  refresh on the request path, no "reload capabilities" tool. Each would put
  the config plane back on the latency path under someone else's control, which
  is defect #1 with extra steps. The TTL is the invalidation mechanism.

---

## References

- This repo: `internal/capability/{source,http_source,document,compose}.go`
  (the seam, the two existing producers, `ScopeDocuments`),
  `internal/agent/conversation.go:524-548` (`loadDocuments`, `Entitlements`),
  `internal/agentwiring/wiring.go` (source selection and the extended-card
  path — both moved there from `cmd/assistant/runner.go`, which is now a thin
  delegation), `internal/config/config.go`
  (the `CAPABILITY_*` pair), `config/base/rbac.yaml` (the permission promise
  this change amends), `config/components/api-registration/apiservice.yaml`
  (why the group must change), `internal/metrics/metrics.go` (metric naming),
  `internal/auth/sar.go:73` (`projectControlPlanePath` — how a project is
  addressed), `:80` (`sarEndpoint`), `:37` (`DefaultSARCacheTTL`), `:149-165`
  (`SARConfig`, the credentials to reuse), `:401-406` (`allowCache`, the
  never-cache-a-negative asymmetry) and `:39-41` (`maxSARCacheEntries`, the
  eviction policy to copy), `internal/auth/transport.go` (why the credential is
  a client certificate and not a service-account token),
  `internal/capability/compose.go` (`ScopeDocuments`, which now guards only the
  sources whose payload carries a namespace), `pkg/apis/capabilities/v1alpha1` + `config/crd/` + `config/milo/`
  (the object model and its Milo-side projection),
  `config/milo/rbac/control-plane-auth-delegator.yaml` (the ClusterRole the
  capability grant extends, and why the credential is a certificate),
  `internal/capability/identity.go:43-62` (`identityHeaders`, fail-closed on an
  unsanctioned host), `internal/capability/urlguard.go:69`
  (`parseHostAllowList`), `config/base/assistant.yaml:72-76`
  (`CAPABILITY_IDENTITY_FORWARD_HOSTS`, the gateway as the one sanctioned host),
  `config/components/streamco/mcp-route.yaml` (allow-list enforcement at the
  gateway), `config/overlays/dev/capability-documents.json:41` (an endpoint
  already rewritten to the gateway).
- Other Milo repositories (all claims in this document were verified against
  these; paths are relative to the `milo-os` checkout root):
  `milo/internal/apiserver/storage/project/mux.go:225` (etcd prefix per project)
  and `:318-323` (no cross-project LIST);
  `milo/internal/apiserver/storage/project/restoptions.go:35-38` (CRD
  definitions stay global);
  `milo/pkg/server/filters/projects.go:40-62` (prefix stripped and `RequestInfo`
  recomputed **before** authorization — the load-bearing claim for the grant);
  `milo/pkg/multicluster-runtime/milo/provider.go:249-267` (one
  `cluster.Cluster` per project — the watch-based alternative we decline);
  `openfga-provider/internal/webhook/subjectaccessreview_authorizer.go:333-336`
  (UID required, so IAM cannot bind a certificate identity);
  `ipam/internal/access/namespace.go:88-96` (the TTL-cache-over-informer
  precedent, and the never-cache-a-negative rule);
  `service-catalog, local branch feat/agent-framework-api (unpushed)`
  `internal/controller/agentbinding_controller.go:243-244` and
  `api/v1alpha1/agentbinding_types.go:97` (cluster scope, no namespace);
  `infra/apps/billing-system/base/milo-control-plane.yaml` (the Flux delivery
  pattern to copy).
- Docs: `docs/architecture/capabilities.md` (the contract and the
  degrade-not-fail posture), `docs/capability-reference.md` (the schema and the
  provider API being replaced),
  `docs/enhancements/assistant-apiserver.md` (the meet-at-the-store pattern and
  the group/version already claimed),
  `docs/enhancements/capability-gap-reporting.md` (`spec.reportingProject`,
  which the CRD schema carries forward).
