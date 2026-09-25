# Capability source degraded

**Alerts:** `AssistantCapabilityServingStaleConfig`,
`AssistantCapabilityFetchDegradedToEmpty`,
`AssistantCapabilityStatusWriteForbidden`, `AssistantCapabilityScopeMismatch`
**Severity:** warning → critical (see each alert)
**Source:** `config/overlays/production/alerts.yaml`, group `assistant.capability`

## Read this first: nothing is failing

A capability problem never fails a chat. The turn proceeds with whatever
configuration is in hand — fresh, stale, or none — and says nothing to the user.
There is no 5xx, no failed turn, no error in the product, and no complaint you
will receive that names this. That is a deliberate decision
(`docs/architecture/capabilities.md`, "Degrading, not failing"): telling a user
"your capability configuration may be out of date" is noise they cannot act on,
and it leaks platform state into a customer conversation.

The consequence for you is that **the metrics are the only signal**. If you go
looking for a matching spike in error rate or latency to confirm the alert is
real, you will find none, and the absence proves nothing.

What a user experiences instead: an assistant with fewer tools than yesterday,
or no provider tools at all, answering confidently from built-ins. If a
customer reports "Patch forgot how to do X", start here rather than at the model.

## Which failure is this

The three fetch outcomes are on one counter, and the difference between them is
the whole triage:

```
# Rates by outcome over the last 15 minutes
sum by (outcome) (rate(assistant_capability_fetch_total[15m]))
```

| What you see | What it means |
|---|---|
| `hit` dominant, occasional `miss` | Healthy. One LIST per 60s TTL per active project is the design. |
| `empty` present | Normal. A project with no bindings, or one whose first binding has not landed. Empty is never cached, so this recurs by design. |
| `refresh_failed` ≈ `stale_served` | **Degrading as designed.** LISTs are failing and the last known-good answer is being served. Customers keep working on possibly-outdated entitlements. |
| `refresh_failed` > `stale_served` | **Worse.** Some projects have nothing retained to fall back on and are composing *zero* capabilities — indistinguishable, to their users, from being entitled to nothing. |
| all fetch series absent | The deployment is not in `crd` mode (or `/metrics` is not being scraped — see below). |

Age of what is actually being served:

```
histogram_quantile(0.99, sum by (le) (rate(assistant_capability_cache_entry_age_seconds_bucket[15m])))
```

Under 60 seconds is the TTL doing its job. Minutes means sustained stale
serving; an hour means the control plane has not answered in an hour and the
`critical` tier of `AssistantCapabilityServingStaleConfig` should have fired.

**Stale-serving is not an assistant outage.** If `AssistantDown` or
`AssistantDegraded` are also firing, work those first — they are about the
service; this one is about its configuration, and a restarted pod loses its
cache and converts stale-serving into the `refresh_failed > stale_served` case.

If every series here is missing, confirm you are scraping the assistant at all
before concluding it is healthy:

```
kubectl -n patch-system port-forward deploy/assistant 7820:7820
curl -s localhost:7820/metrics | grep assistant_capability_
```

Scraping is annotation-based rather than via a `ServiceMonitor`: the production
deployment carries `prometheus.io/scrape`, and infra's network policy admits the
telemetry-system collector on 7820 for exactly that reason. If
`assistant_capability_fetch_total` returns nothing in Prometheus while the
`curl` above shows it, the collector is not doing annotation discovery — and
every alert in the capability group is green for the wrong reason.

## Is the control plane reachable, and does the grant resolve

The assistant reads bindings from each project's own virtual control plane:

```
GET {control-plane}/apis/resourcemanager.miloapis.com/v1alpha1/projects/{project}/control-plane
    /apis/capabilities.assistant.miloapis.com/v1alpha1/capabilitybindings
```

with **its own client certificate** (CN `agent@assistant.datumapis.com`), not a
service-account token — Milo validates service-account tokens only against its
own issuer and 401s a token minted by the workload cluster before reading the
body. Reproduce the call exactly as the assistant makes it, from inside the pod,
using the paths the pod is configured with:

```
kubectl -n patch-system exec deploy/assistant -- env | grep AUTHZ_SAR_
# then, with those paths:
kubectl -n patch-system exec deploy/assistant -- \
  curl -sS -o /dev/null -w '%{http_code}\n' \
    --cacert   "$AUTHZ_SAR_CA_CERT_PATH" \
    --cert     "$AUTHZ_SAR_CLIENT_CERT_PATH" \
    --key      "$AUTHZ_SAR_CLIENT_KEY_PATH" \
    "$AUTHZ_SAR_API_URL/apis/resourcemanager.miloapis.com/v1alpha1/projects/<project>/control-plane/apis/capabilities.assistant.miloapis.com/v1alpha1/capabilitybindings"
```

Read the status code, not the body:

- **200** — the grant resolves and the control plane is reachable. This is the
  one-LIST check the `crd` cutover is gated on; record that you ran it.
- **403** — the grant does not resolve for this identity on a project path. The
  claim that a root `ClusterRoleBinding` in the core control plane authorizes a
  request arriving on `/projects/{p}/control-plane` is well-evidenced (Milo's
  project router strips the prefix and recomputes `RequestInfo` *before*
  authorization) but was never a production observation until someone ran this.
  No edit to `config/milo/rbac/control-plane-auth-delegator.yaml` fixes a 403
  here: Milo IAM structurally cannot bind a certificate identity (the OpenFGA
  authorizer requires a user UID; x509 supplies only CN and O), so there is no
  second grant to try. The fallback is `CAPABILITY_SOURCE=http` and the
  catalog-side adapter, which needs no standing grant.
- **404** — the `CapabilityBinding` CRD is not registered at Milo root. Fixed
  by one apply of the `config/milo` bundle (which pulls in `config/crd`) at
  Milo root: CRD *definitions* are global in Milo and only instance data is
  partitioned, so there is no per-project registration step. A 404 surfaces as
  `refresh_failed` rather than as a plausible-looking empty result, which is the point of
  preferring it to a silent zero.
- **timeout / connection refused** — control-plane reachability. Check the
  NetworkPolicy egress in this overlay and the Milo apiserver itself. The
  assistant will keep serving stale for as long as its entries survive.

Per-project detail is in the logs, never in the metrics (project cardinality is
tenant count, which this repo cannot bound):

```
kubectl -n patch-system logs deploy/assistant | grep capability.crd.
```

`capability.crd.stale_served` carries `projectName`, `ageSeconds` and the
underlying error. `capability.crd.fetch_failed` is the harder case — it names
the project that got nothing.

## `forbidden`: what it means and the exact hypothesis to test

`AssistantCapabilityStatusWriteForbidden` fires on
`assistant_capability_status_write_total{outcome="forbidden"}` — Milo returned
403 for the status `PATCH`. It is deliberately a separate outcome from `error`
because it is the only externally visible evidence for one specific unverified
assumption.

**The assumption.** The assistant writes conditions with a JSON merge patch to

```
.../capabilitybindings/{name}/status
```

and the ClusterRole grants `patch` on `capabilitybindings/status` *only* —
never on the parent, because a service that can rewrite the spec it was handed
is not reading an entitlement, it is granting itself one. That split is
expressible because ordinary Kubernetes RBAC has a subresource axis. **Milo IAM
does not** — its `updateStatus`-style permission convention is inert (`GetVerb`
never returns it), so on that leg a status write is just `patch` on the parent.
The open question is which leg answers, and whether the project router's
`RequestInfo` recomputation preserves the `status` subresource across the
prefix strip. If it does not, the request authorizes as plain `patch` on
`capabilitybindings`, which this identity does not have, and **every** status
write 403s.

**How to test it, in this order:**

```
# 1. Does the identity believe it may patch the subresource, on a project path?
kubectl auth can-i patch capabilitybindings --subresource=status \
  --as='agent@assistant.datumapis.com' -n '' \
  --server=<control-plane>/apis/resourcemanager.miloapis.com/v1alpha1/projects/<project>/control-plane

# 2. And the parent, which should be NO:
kubectl auth can-i patch capabilitybindings \
  --as='agent@assistant.datumapis.com' \
  --server=<same as above>
```

A **no** on (1) with a **yes** on (2) means the subresource axis is being lost
and the assumption is wrong. A **no** on both, while the LIST above returns
200, means the same thing from the other side.

If it is wrong, the options are, in order of preference: fix the subresource
handling in Milo's router (the correct fix, not ours to make); or widen the
ClusterRole to `patch` on the parent resource — which must be argued for
explicitly, because it hands a chat service write access to the spec it reads;
or accept the loss of status writeback, which costs the producer feedback loop
and nothing else.

**Nothing user-facing depends on this.** Composition, chats, and the LIST path
are unaffected by a 403 on status. What breaks is the reason the CRD was worth
adopting: a provider whose MCP endpoint is unreachable learns nothing, and the
`Accepted`/`Composed` conditions the catalog team reads never appear. That is
also why `forbidden` is a `warning`, not a page.

Related outcomes on the same counter, neither of them alerted and both fine:
`dropped` is the bounded write queue shedding under an outage (preferred to
blocking a turn — a lost status update is cosmetic), and `error` is a transient
write failure that the next observation retries by itself. There is no retry
loop on purpose.

## `scope_dropped{reason="mismatch"}`

A capability source returned a document whose own metadata names a project other
than the one being served, and `ScopeDocuments` dropped it. Nothing crossed
tenants — the gate held — but a producer emitted something it should not have.

This series is emitted for **every** source, not just `crd`, so this alert is
live in `http` mode too. That is correct: for the fixture and HTTP sources the
namespace is a producer's convention and this check is the only thing behind it.
For the CRD source, isolation is structural (a project's bindings are read from
an etcd keyspace prefixed with that project — another tenant's objects are not
filtered out of the response, they are not in the store being read), so a
mismatch there means a document arrived carrying a namespace from somewhere
else entirely and deserves more alarm, not less.

Find the project and the document in the logs — `capability.scope.rejected` —
and take it to whoever owns that source. Do not silence it.

## Rollback: one env var

`CAPABILITY_SOURCE=http` restores the previous behavior completely. There is no
data migration, no CRD to remove, and no state that outlives the pod: the cache
is in-memory, and the `CapabilityBinding` objects are simply left unread.

```
kubectl -n patch-system set env deploy/assistant \
  CAPABILITY_SOURCE=http \
  CAPABILITY_PROVIDER_URL=http://<capability-provider-host>
```

Both variables, in one command. Config validation rejects a companion variable
set for a mode that does not use it *and* a mode missing its own, so a
half-finished edit fails at boot rather than booting with a source nobody
intended. Then land the same change in
`config/overlays/production/kustomization.yaml` so your GitOps pipeline does not
reinstate `crd` on the next sync — the block is pre-staged there with both
variants.

What rollback costs you: the latency of a per-turn HTTP fetch returns, and so
does the failure mode this design exists to remove — `HTTPSource` has no cache,
so a provider outage again presents as "entitled to nothing" with no stale
answer to fall back on and no `stale_served` metric to tell you. Rolling back
trades a visible degradation for an invisible one. Do it when the control plane
or the grant is the problem; do not do it to quiet an alert.

## Cutover: turning on `CAPABILITY_SOURCE=crd`

This lives here, next to the rollback, because the two are the same procedure in
opposite directions and an operator reaching for one usually needs the other in
the same hour. `docs/deployment.md` links to it.

### Gates — both must be closed first

1. **The catalog's projection controller is pushed and writing.** Until it is,
   nothing creates `CapabilityBinding` objects and every project's LIST returns
   empty — which composes zero capabilities for *every* project while looking
   exactly like a correctly-configured empty tenant. Verify by reading, not by
   asking: pick a project you know is entitled and confirm objects exist.

   ```
   kubectl --server=<control-plane>/apis/resourcemanager.miloapis.com/v1alpha1/projects/<project>/control-plane \
     get capabilitybindings
   ```

2. **The 200-vs-403 LIST has been observed** for the assistant's own
   certificate, against a real project path — the check under "Is the control
   plane reachable" above. It is one API call and it is the difference between
   a rollout and a discovery.

A green `dev-crd` overlay closes **neither** gate. Kind runs a single apiserver
with no project router, so `projects/{name}/control-plane` resolves to the same
server and root RBAC applies trivially: `dev-crd` proves the object model, the
conversion, the cache, and the status writer, and proves nothing whatsoever
about the production topology those depend on. Treat CI green as necessary and
not sufficient, and soak this in staging against a real Milo before production.

### The flip

Edit `config/overlays/production/kustomization.yaml` — the commented block under
"The crd cutover, pre-staged" — to `CAPABILITY_SOURCE: crd`, removing
`CAPABILITY_PROVIDER_URL`. Confirm `CAPABILITY_MCP_ENDPOINT_HOSTS` is set to the
AI gateway before you apply: under `crd` the component that rewrites a provider's
raw endpoint to the gateway URL (the catalog's projection controller) is no
longer the component that dials it, so that allow-list is the only thing
standing between a controller regression and an unmetered, un-allow-listed
direct dial. Render, review, apply through the pipeline as usual.

### The first ten minutes

Watch, in this order:

```
sum by (outcome) (rate(assistant_capability_fetch_total[5m]))
sum by (outcome) (rate(assistant_capability_status_write_total[5m]))
assistant_capability_cache_entries
```

- `miss` then `hit` climbing, `refresh_failed` at zero — the read path works.
- **`refresh_failed` at 100% ⇒ roll back.** That is the 403 case, arriving
  after the flip instead of before it.
- **`empty` for every project ⇒ roll back.** Gate 1 was not actually closed.
- `status_write_total{outcome="ok"}` appearing within a minute or two of the
  first turns — the write path works. `forbidden` instead ⇒ see above; this
  does *not* require a rollback by itself, since chats are unaffected.
- `assistant_capability_cache_entries` should settle near your count of
  actively-chatting projects. Pinned at 4096 means the bound is being hit and
  live entries are being evicted; that is a capacity conversation, not an
  incident.

Also spot-check a binding's conditions from the producer's side, which is the
feature the whole change was bought for:

```
kubectl --server=.../projects/<project>/control-plane \
  describe capabilitybinding <name>
```

Leave the alerts to catch the slow failures. Nothing here needs watching past
the first hour except `AssistantCapabilityServingStaleConfig`, which is designed
to be quiet for 15 minutes before it says anything.
