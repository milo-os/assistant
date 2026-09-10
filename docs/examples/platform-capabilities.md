# Example: declaring the platform's own capabilities

A worked example of `PLATFORM_CAPABILITY_DOCS_FIXTURE` — the mechanism a
platform operator uses to give **every** project the capabilities the platform
itself publishes. See
[Platform capabilities](../capability-reference.md#platform-capabilities-every-project-no-entitlement)
for what it means and
[Configuration](../configuration.md) for the settings.

Nothing here is applied by this repository. The manifests live in the
deployment repo (`datum-cloud/infra`, `apps/patch-assistant/overlays/<env>/`)
and are shown in that overlay's shape.

## What this declares

Two services, neither of them catalog services:

| Service | Endpoint | Tool |
|---|---|---|
| `locations.miloapis.com` | `http://locations-mcp.locations-system.svc.cluster.local:8080/mcp` | `locations_list` |
| `quota.miloapis.com` | `http://milo-quota-mcp.milo-system.svc.cluster.local:8080/mcp` | `quota_get` |

Both keep a record the platform owns — where a service is offered, and what a
project's allowance has left. Nothing entitles a project to either, and every
project needs both, which is exactly the case the platform source exists for.

This sits **alongside** the per-project capability fixture the overlay already
maintains (`capability-documents.json`, wired through
`CAPABILITY_DOCS_FIXTURE`). The two answer different questions and both are
set: what every project gets, and what this project was entitled to. They are
not mutually exclusive, and the assistant boots with both.

The tools arrive as `locations__locations_list` and `quota__quota_get`. Platform
providers are namespaced like every other provider; only Patch's own built-ins
(`resources_list`, `load_skill`, `memory_remember`) are un-prefixed.

Neither service is metered. That follows from the documents being platform
documents and needs no second setting — `CAPABILITY_UNMETERED_SERVICES` exists
for anything *else* an operator wants off the meter.

## 1. The documents

`platform-capability-documents.json`, mounted from a ConfigMap:

```json
[
  {
    "apiVersion": "services.miloapis.com/v1alpha1",
    "kind": "AgentBinding",
    "metadata": { "name": "locations-platform-binding" },
    "spec": {
      "serviceRef": { "name": "locations" },
      "serviceName": "locations.miloapis.com",
      "tools": {
        "mcpServers": [
          {
            "name": "locations",
            "endpoint": "http://locations-mcp.locations-system.svc.cluster.local:8080/mcp",
            "toolSelector": { "include": ["locations_list"] }
          }
        ]
      }
    }
  },
  {
    "apiVersion": "services.miloapis.com/v1alpha1",
    "kind": "AgentBinding",
    "metadata": { "name": "quota-platform-binding" },
    "spec": {
      "serviceRef": { "name": "quota" },
      "serviceName": "quota.miloapis.com",
      "tools": {
        "mcpServers": [
          {
            "name": "quota",
            "endpoint": "http://milo-quota-mcp.milo-system.svc.cluster.local:8080/mcp",
            "toolSelector": { "include": ["quota_get"] }
          }
        ]
      }
    }
  }
]
```

Four things about the shape, since JSON carries no comments:

- **No `metadata.namespace`.** Patch clears it on this path anyway, but writing
  one would say something untrue about a document that belongs to every project.
- **No `serviceAgentRef`, no `configurationVersion`.** Those are catalog fields
  and these services are not in the catalog. They are optional on the platform
  path precisely so nobody has to invent a value; they stay required on the
  per-project path.
- **`serviceName` is load-bearing.** It is the metering dimension, the
  unmetered-service key and the log key. Get it wrong and the tools still work
  and the logs name the wrong service.
- **`toolSelector.include` is an allow-list, enforced client-side.** A tool the
  provider serves but this list does not name is not composed. Adding a tool to
  a platform service is a deliberate edit here, reviewed like any other change to
  what every project's assistant can do.

Generate the ConfigMap with `disableNameSuffixHash: true` if the mount is
referenced from inside a Flux patch string — kustomize's name-reference
transformer does not rewrite those, and a hashed name leaves the pod stuck in
`ContainerCreating`.

```yaml
configMapGenerator:
  - name: assistant-platform-capability-documents
    files:
      - platform-capability-documents.json
    options:
      disableNameSuffixHash: true
```

## 2. The mount and the setting

```yaml
- op: add
  path: /spec/patches/-
  value:
    target:
      kind: Deployment
      name: assistant
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: assistant
      spec:
        template:
          spec:
            containers:
              - name: assistant
                env:
                  - name: PLATFORM_CAPABILITY_DOCS_FIXTURE
                    value: /etc/assistant/platform-capabilities/platform-capability-documents.json
                volumeMounts:
                  - name: platform-capability-documents
                    mountPath: /etc/assistant/platform-capabilities
                    readOnly: true
            volumes:
              - name: platform-capability-documents
                configMap:
                  name: assistant-platform-capability-documents
```

`CAPABILITY_DOCS_FIXTURE` stays exactly as it is. `PLATFORM_CAPABILITY_DOCS_FIXTURE`
is only exclusive with `PLATFORM_CAPABILITY_PROVIDER_URL`.

`CAPABILITY_ALLOW_PRIVATE_NETWORKS` must already be `true` (the base overlay
sets it), or the SSRF guard refuses both ClusterIPs.

## 3. Identity forwarding — read before adding

Both services read the **caller's own** records, so both need the caller's
credential. That means adding both hosts to `CAPABILITY_IDENTITY_FORWARD_HOSTS`,
which is the service's credential-forwarding trust boundary: every host named
there receives end-user bearer tokens.

```yaml
- op: add
  path: /spec/patches/-
  value:
    target:
      kind: Deployment
      name: assistant
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: assistant
      spec:
        template:
          spec:
            containers:
              - name: assistant
                env:
                  - name: CAPABILITY_IDENTITY_FORWARD_HOSTS
                    value: "compute-mcp.compute-system.svc.cluster.local,locations-mcp.locations-system.svc.cluster.local,milo-quota-mcp.milo-system.svc.cluster.local"
```

The two entries this example adds, exactly:

```
locations-mcp.locations-system.svc.cluster.local
milo-quota-mcp.milo-system.svc.cluster.local
```

- **No port.** The match runs against `url.Hostname()`, which has already
  stripped `:8080`. An entry carrying the port silently matches nothing and
  forwards nothing, and the failure surfaces as an unauthenticated tool call
  rather than a configuration error.
- **Exact hosts, never a suffix.** Matching is `host == entry ||
  strings.HasSuffix(host, "."+entry)`, so `svc.cluster.local` would be a
  legitimate suffix of every Service in the cluster and would hand user tokens
  to anything a provider-controlled document cared to name.
- **The list replaces, it does not append.** Include the hosts the overlay
  already forwards to, or they stop working.
- **It must stay in lockstep with the endpoints above.** Change an endpoint's
  host and this list is the second edit.

## 4. NetworkPolicy — the operator's second edit

The assistant's egress is default-deny. Without a rule per destination the MCP
connect does not fail, it **hangs** until the connect timeout and degrades to
"no tools", which reads as a provider outage rather than a missing rule.

```yaml
- op: add
  path: /spec/egress/-
  value:
    to:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: locations-system
        podSelector:
          matchLabels:
            app.kubernetes.io/name: locations-mcp
    ports:
      - protocol: TCP
        port: 8080
- op: add
  path: /spec/egress/-
  value:
    to:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: milo-system
        podSelector:
          matchLabels:
            app.kubernetes.io/name: milo-quota-mcp
    ports:
      - protocol: TCP
        port: 8080
```

- `path: /spec/egress/-` **appends**. `op: add path: /spec/egress` replaces the
  array and would cut DNS, Postgres, the AI gateway and the control plane in one
  edit.
- The port is the **container** port, not the Service port: NetworkPolicy is
  enforced post-DNAT on the pod IP and pod port.
- `namespaceSelector` and `podSelector` in **one** element. Separate elements
  are OR'd and open the whole namespace.
- The matching **ingress** rule belongs to each service's own deployment. Both
  halves move together.

## 5. Verifying

At boot:

```
agent.capability.platform_source  type=fixture path=/etc/assistant/platform-capabilities/platform-capability-documents.json
```

At the first conversation, once the documents have been read:

```
capability.platform.services  services=[locations.miloapis.com quota.miloapis.com]
```

The names land on the first fetch rather than at boot, because a platform
provider URL is only readable on a request; a fixture reports the same way for
consistency.

Then, in any project — including one with no entitlements at all — the extended
agent card advertises both services, and the assistant has
`locations__locations_list` and `quota__quota_get`. A tool call to either emits
no `tool-invocations` usage event.

If the tools are missing, look for `capability.mcp.connect_failed` (NetworkPolicy
or endpoint), `capability.mcp.tool_missing` (the `toolSelector.include` name does
not match what the server serves), or `capability.fixture.entry_skipped` (a
document failed validation — the message names the field).
