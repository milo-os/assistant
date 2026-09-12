# Environment e2e (chainsaw)

Suites that run against the live dev environment stood up by `task dev:setup`,
invoked with `task e2e` (or `task e2e -- test/e2e/<suite>` for one). They assert
against real state; nothing here is a unit test with a cluster attached.

| Suite | What it proves |
|---|---|
| `env-health` | Every component of the dev environment is up. Pure asserts, no mutations. |
| `chat-smoke` | A two-turn conversation through the full path, with memory replay and a negative control. |
| `assistant-apiserver` | Conversation history is discoverable through the kube-aggregator, project-scoped. |
| `capability-crd` | A `CapabilityBinding` object becomes a provider tool the model calls, and its verdict is written back as conditions. |
| `capability-endpoint-guard` | A binding naming a raw provider endpoint instead of the gateway is neither dialed nor credentialed. |

The last two require `CAPABILITY_SOURCE=crd` — deploy with
`task dev:deploy OVERLAY=dev-crd`. Under any other overlay they report `SKIP`
rather than failing, because a binding that is not the source of truth cannot
prove anything about the source.

## What is deliberately not tested here: tenant isolation

There is **no e2e asserting that a `CapabilityBinding` in project A is invisible
to project B**, and the absence is deliberate rather than an oversight.

Isolation for the CRD source is not something this repository implements. A Milo
project is a virtual control plane — one apiserver partitioned by the etcd key
prefix `/projects/<project>` — and the assistant reads bindings through
`/apis/resourcemanager.miloapis.com/v1alpha1/projects/<project>/control-plane`.
Another tenant's bindings are not filtered out of the response; they are not in
the keyspace being read. There is no code path in the assistant, correct or
buggy, that turns `Documents(ctx, "acme")` into another project's configuration,
because the project name is consumed by the URL before any of our logic runs.

A kind cluster has none of that. It runs one apiserver with no project router,
and `projects` is an ordinary CRD there with a `status` subresource and nothing
else, so the control-plane prefix resolves to the same server — or to a 404. The
bindings the `dev-crd` overlay seeds are cluster-scoped objects in the only plane
there is, visible to every project a dev overlay addresses. A test that seeded a
binding "in another project" and asserted it was not composed would therefore be
asserting something the environment cannot express: it would pass by accident, or
fail for reasons unrelated to tenancy, and either way it would read as evidence
for a property it never touched. A green test that proves nothing is worse than
no test, because the next person to consider writing the real one will see it and
stop.

Proving it needs a real Milo: two project control planes, a binding written
through one, and a LIST through the other returning without it. That belongs to
the production-flip gate — the same step that confirms root RBAC is evaluated for
requests arriving on project control-plane paths — not to this suite. Until then
the guarantee rests on Milo's storage partitioning, which is argued and cited in
`docs/enhancements/crd-capability-source.md` and summarized in
`docs/capability-reference.md`.

The same limitation bounds what `capability-crd` proves. It exercises the object
model, the conversion, the cache, and the status writeback. It does not exercise
per-project addressing, the root-RBAC grant, the CRD's real install path at Milo
root, or the client-certificate credential — see the header of
`config/overlays/dev-crd/kustomization.yaml`, which lists each one.
