# Conversation aggregated apiserver — design & build plan

Status: **All phases complete.** Phase 1 (API types + scheme, `dd63ecd`), Phases 2–4
(bespoke REST + apiserver binary, config/dev-overlay wiring, kubectl-based CLI) live-verified
against the kind dev stack, and Phase 5 (unit + chainsaw e2e) done — see the per-phase
notes in the [Phasing](#phasing) section below for deviations.

This is the implementation spec for exposing conversations as a Kubernetes KRM
resource via a **milo aggregated API server**, so milo users authenticate with
their normal k8s identity and can `list`/`get`/reopen conversations under
platform authz. It fulfils the item already noted in
[`product-architecture.md`](./product-architecture.md): *"Conversation as a KRM
resource via an aggregated apiserver, messages behind a subresource, so the
portal/CLI can list and reopen conversations under platform authz."*

The build **mirrors the sibling repo `milo-os/ipam`** — a standalone
`k8s.io/apiserver` genericapiserver backed by PostgreSQL. Read `ipam`'s
`cmd/ipam/serve.go`, `internal/apiserver`, `internal/registry/ipam/ipallocation`
(the simplest resource), and `config/` as the working templates. Where this doc
and ipam's `ARCHITECTURE.md` disagree, trust the ipam **code** — its
ARCHITECTURE.md describes an aspirational type set.

---

## Locked decisions

1. **Standalone apiserver** — a separate binary + Deployment (mirrors ipam), not embedded in the A2A service.
2. **Follow ipam's model** for auth, tenancy, and config structure.
3. **Dev = in-cluster authz.** Delegated authn/authz use the empty-default → in-cluster fallback (the kind cluster's own TokenReview/SAR). Prod points the delegated kubeconfigs at milo-apiserver.
4. **Read-only v1:** `list` + `get` Conversations, and a `messages` subresource. No create/update/delete yet.
5. **Messages embedded** in the subresource response (whole transcript), not paginated (v1).
6. **Shared store, not service-to-service.** The A2A service keeps writing the relational `conversations`/`messages` tables directly on the chat hot path; the apiserver is a **read view** over those same tables. They meet only at Postgres — no A2A↔apiserver network calls.
7. **"Resume"** = the portal/CLI lists/gets via the apiserver to find a conversation + its `context-id`, then continues the chat over the existing A2A path (`patch chat --context-id`). An apiserver is not a chat transport.

### Key divergence from ipam
ipam stores every object as an opaque codec-encoded blob in a generic
`ipam_objects` table (it owns its data). We already have **real relational
`conversations`/`messages` tables** written by the A2A hot path
(`internal/history/postgres.go`). So we do **not** copy ipam's blob store.
Instead the Conversation apiserver gets a **bespoke read-storage** that maps the
existing rows → API objects via the shared `internal/history` package. No
double-storing, no etcd, no generic `genericregistry.Store`.

---

## Architecture / data flow

```
milo user ──(k8s token)──> kube-aggregator ──/apis/assistant.miloapis.com/v1alpha1──> assistant-apiserver
                                                                                          │ (delegated authn+authz)
                                                                                          │ tenancy: project from identity
                                                                                          ▼
A2A assistant service ──writes conversations/messages──> Postgres <──reads (list/get/messages)── assistant-apiserver
```

- **A2A service:** unchanged. Writes/reads history directly via `internal/history` on every chat turn.
- **Apiserver:** read-only view; `list`/`get` map to `internal/history` queries, filtered by the caller's project.

---

## API design

- **Group/Version:** `assistant.miloapis.com/v1alpha1` (matches the group already used for SAR in `internal/auth/sar.go`).
- **`Conversation`** — namespaced; **namespace == milo project** (maps to the existing `project_name` column). `metadata.name = context_id`, `creationTimestamp = created_at`. `status: { lastActiveAt, messageCount }`. No spec (born from chat).
- **`conversations/messages` subresource** → `ConversationMessages { items: []ConversationMessage{ seq, role, content, createdAt } }` — the transcript.
- **Verbs (v1):** `list`, `get`, get-`messages`. (`watch`/`delete` later.)

Types already exist: `pkg/apis/assistant/{,v1alpha1,install}` (Phase 1). Internal hub + versioned, hand-written deepcopy + 1:1 conversion. `openapi-gen` output (`zz_generated.openapi.go`) is **not yet generated** — needed in Phase 2/3 for discovery/OpenAPI.

---

## Repo layout to build (mirror ipam, minus allocator/blob-store/etcd)

```
cmd/assistant-apiserver/
  main.go            cobra root (+ optional `migrate` no-op / ensure-tables)
  serve.go           RecommendedOptions, delegated authn/authz, storage map, install
internal/apiserver/
  apiserver.go       Scheme/Codecs, New(): VersionedResourcesStorageMap, InstallAPIGroup
  registry/conversation/
    storage.go       rest.Scoper+Lister+Getter+TableConvertor over internal/history
    messages.go      the `messages` subresource REST (rest.Getter) over internal/history.Turns
internal/history/     EXTEND: GetConversation(ctx, project, id); ensure list returns created_at/last_active_at/messageCount
internal/tenant/      small helper: project from UserInfo.Extra (see ipam/internal/tenant)
pkg/apis/assistant/   DONE (Phase 1). Add pkg/generated/openapi (openapi-gen) in Phase 2/3.
pkg/client/           optional generated clientset for the CLI (Phase 4)
config/               mirror ipam/config (see below)
hack/                 codegen (deepcopy/conversion/openapi) — optional; Phase 1 hand-wrote
```

---

## serve.go — the auth crux (authenticate as milo users)

Mirror `ipam/cmd/ipam/serve.go`:

- `options.NewRecommendedOptions("/registry/assistant.miloapis.com", Codecs.LegacyCodec(...))` — bundles SecureServing, **`DelegatingAuthenticationOptions`**, **`DelegatingAuthorizationOptions`**, Audit, Features, CoreAPI, Admission, Etcd.
- `genericConfig := genericapiserver.NewRecommendedConfig(Codecs)`; `EffectiveVersion = NewEffectiveVersionFromString("1.36","","")`.
- OpenAPI: `openapinamer.NewDefinitionNamer(Scheme)` + `GetOpenAPIDefinitions` (needs openapi-gen output).
- `o.RecommendedOptions.Etcd = nil` — **no etcd**; our bespoke REST is the only backend.
- Disable admission (delegating apiserver; the front kube-apiserver already ran webhooks).
- `o.RecommendedOptions.ApplyTo(genericConfig)` — **this installs the delegated authenticator/authorizer** into `genericConfig.Authentication.Authenticator` / `Authorization.Authorizer`.
- Storage: set `genericConfig.RESTOptionsGetter` to our bespoke getter (or skip it entirely — our REST doesn't use the generic Store; it wraps `internal/history` directly).
- `AddReadyzChecks("postgres", db.Ping)`.

**Delegated auth is wired by Deployment flags, not code** (see `config/`):
```
--authentication-kubeconfig=$(AUTHENTICATION_KUBECONFIG)   # TokenReview (empty ⇒ in-cluster ⇒ dev)
--authorization-kubeconfig=$(AUTHORIZATION_KUBECONFIG)     # SubjectAccessReview (empty ⇒ in-cluster ⇒ dev)
--requestheader-client-ca-file=/etc/kubernetes/pki/requestheader/ca.crt
--requestheader-username-headers/-group-headers/-uid-headers/-extra-headers-prefix
--authorization-always-allow-paths=/healthz,/readyz,/livez
--secure-port=8443 --tls-cert-file/--tls-private-key-file
```
Empty auth-kubeconfig defaults = in-cluster fallback → **dev uses the kind apiserver's own TokenReview/SAR** (decision #3). Prod points them at milo-apiserver so milo identities resolve.

---

## REST storage (bespoke, over internal/history)

`internal/apiserver/registry/conversation`:
- `ConversationREST` implements `rest.Scoper` (NamespaceScoped()=true), `rest.Lister` (`List(ctx, *ListOptions)` → `internal/history.ListConversations(project, limit)` → `ConversationList`), `rest.Getter` (`Get(ctx, name, *GetOptions)` → `history.GetConversation(project, name)`), `rest.TableConvertor` (start with `rest.NewDefaultTableConvertor`, add custom columns later).
- `MessagesREST` implements `rest.Getter` (and `rest.Storage`) for the subresource → `history.Turns(project, id)` → `ConversationMessages`.
- Register in the storage map:
  ```go
  v1alpha1Storage["conversations"]          = conversationREST
  v1alpha1Storage["conversations/messages"] = messagesREST
  ```
- **Tenancy:** get the project from the request context. Namespace in the request == project. Also read the milo identity's parent-Project from `UserInfo.Extra["iam.miloapis.com/parent-name"]` (see `ipam/internal/tenant`) and reconcile with the namespace. Every store query filters `WHERE project_name = <project>`. In dev (in-cluster authz, no milo Extra), fall back to the request namespace as the project.
- **List cacher:** ipam bypasses the apiserver cacher for lists (it can't serve project-scoped lists). Our REST doesn't use the generic Store/cacher at all, so this is moot — we read Postgres directly.

`internal/apiserver/apiserver.go`: package-level `Scheme`/`Codecs` init'd via `install.Install(Scheme)` + `metav1.AddToGroupVersion` + `AddUnversionedTypes(v1, &Status{}, …)`; `New()` builds `NewDefaultAPIGroupInfo`, fills `VersionedResourcesStorageMap["v1alpha1"]`, `InstallAPIGroup`.

---

## migrate / schema

The `conversations`/`messages` tables already exist and are created idempotently
by `internal/history/postgres.go` (`CREATE TABLE IF NOT EXISTS`). Options:
- **Preferred:** the apiserver reuses them read-only; no migration owner change. Either have the apiserver run the same idempotent `CREATE TABLE IF NOT EXISTS` at startup (harmless), or rely on the A2A service having created them.
- A `migrate` subcommand is optional (ipam has one via goose). For a read-view we likely don't need one.

Do **not** add ipam's `*_objects`/changelog blob tables.

---

## config/ — deployment + APIService registration (mirror ipam/config)

`config/base`: Deployment (`assistant-apiserver`, ns e.g. `assistant-system`, serve `/assistant-apiserver serve` on `:8443`, TLS via cert-manager CSI driver, `control-plane-ca` configmap mounted at `/etc/kubernetes/pki/requestheader`, `CONVERSATION_STORE_URL`/DSN env), Service (`:443 → 8443`), ServiceAccount, `rbac-auth-reader` (RoleBinding in **kube-system** to `extension-apiserver-authentication-reader` — **mandatory** for delegated authn), `rbac-cluster` (ClusterRole: read `configmaps[extension-apiserver-authentication]`, flowschemas/prioritylevelconfigurations, **`create subjectaccessreviews`**; ClusterRoleBindings to it and to `system:auth-delegator`), NetworkPolicy, PDB.

`config/components/api-registration`: the `APIService`:
```yaml
apiVersion: apiregistration.k8s.io/v1
kind: APIService
metadata: {name: v1alpha1.assistant.miloapis.com}
spec:
  service: {name: assistant-apiserver, namespace: assistant-system, port: 443}
  group: assistant.miloapis.com
  version: v1alpha1
  groupPriorityMinimum: 1000
  versionPriority: 15
  # prod: caBundle via cert-manager CA injection; dev overlay: insecureSkipTLSVerify: true
```

`config/components/iam` (milo): a cluster-scoped `ProtectedResource` for
`conversations.assistant.miloapis.com` (plural/singular + permissions `get`,
`list`; `parentResources: Project`) and namespaced `Role`s (viewer/consumer)
granting `assistant.miloapis.com/conversations.{list,get}`. This is the resource
`internal/auth/sar.go` already models.

`config/overlays/dev`: wire the apiserver + APIService into `task dev:setup`;
set `insecureSkipTLSVerify: true` and rely on in-cluster authn/authz (kind).

---

## CLI / portal integration (list + resume) — DONE (Phase 4)

- `patch conversations list --project <p>` — table (context-id, created, last-active, message count). `patch conversations show <context-id> --project <p>` — full transcript. Both **shell out to `kubectl`** (`get conversations` / raw `messages` subresource GET) rather than embedding client-go: `cmd/patch` is a deliberately thin `a2a-go` client with no k8s client dep, and kubectl already carries the kubeconfig + serving-cert TLS trust for the aggregated apiserver (the verified Phase 3 path). The command deserializes into the local `pkg/apis/assistant/v1alpha1` types (pure apimachinery, already a dep). Auth is the caller's **k8s identity via `KUBECONFIG`/`--kubeconfig`**, not `PATCH_TOKEN` — consistent with decision #7 (an apiserver is not a chat transport). Code: `internal/patchcli/conversations.go`.
- Resume: `patch chat --context-id <id>` (or `task dev:chat CTX=<id>` / the TUI's `--context-id`).
- `task dev:chats` helper added (list; `ID=<id>` shows one transcript). Reads the cluster directly via `KUBECONFIG` — no port-forward (unlike `dev:chat`), since conversations come from the apiserver, not the A2A service.
- Deferred: the **Bubble Tea TUI conversation picker** (`internal/patchcli/chat_tui.go`) — a nice-to-have that adds an interactive selection state hard to test headlessly; the list/show commands cover the discovery deliverable.

---

## Phasing (each phase must `go build ./...` clean before the next)

- **Phase 1 — DONE** (`dd63ecd`): `pkg/apis/assistant` types + scheme; k8s.io/apiserver v0.36.0 deps; Go 1.26.
- **Phase 2:** bespoke REST over `internal/history` (+ `GetConversation`), `internal/apiserver`, `cmd/assistant-apiserver/serve.go` with delegated authn/authz, `internal/tenant`. Add `pkg/generated/openapi`. Deliverable: the binary runs locally against the store; `curl`/kubectl through a local secure port lists/gets.
- **Phase 3:** `config/` (deployment, service, RBAC, APIService, milo ProtectedResource/Roles) + dev overlay wiring into `task dev:setup`. Deliverable: `kubectl get conversations -n demo-project` works in the kind cluster (in-cluster authz).
- **Phase 4 — DONE:** CLI (`patch conversations list`/`show`, kubectl-based) + `task dev:chats`. TUI picker deferred.
- **Phase 5 — DONE:** tests. Unit — `internal/apiserver/registry/conversation/storage_test.go`
  (fake-Reader get/list/404/tenancy/messages, from Phase 2), plus new
  `internal/tenant/tenant_test.go` (ProjectFromContext: namespace-only dev path, Extra
  match/mismatch→Forbidden, non-Project parent ignored, missing/empty namespace→BadRequest,
  multi-valued Extra) and `cmd/assistant-apiserver/serve_test.go` (the required-DSN
  `validate()` guard). E2e — `test/e2e/assistant-apiserver/chainsaw-test.yaml`: asserts the
  APIService is Available, seeds a marked conversation via the chat path, then proves plain
  `kubectl get conversations`/`get conversation <id>` + the raw `messages` subresource all work
  and are project-scoped (a project with no conversations lists `[]`, not an error; no
  cross-project leak). Runs green via `task e2e -- test/e2e/assistant-apiserver`.
  Deviation: seeding posts to the agent card's advertised `PUBLIC_BASE_URL` (`:1986` in dev),
  reusing an existing `task dev:forward` or standing up its own, since the a2a client dials the
  card URL rather than the fetch URL.

---

## Gotchas (from the ipam mapping)

- **Delegated auth is the whole game.** Without the kube-system `extension-apiserver-authentication-reader` RoleBinding + `system:auth-delegator` binding + `create subjectaccessreviews`, the aggregated server can't authenticate anyone. Get these right in `config/base`.
- **Tenancy is by project, from the milo identity** (`UserInfo.Extra` parent-Project), not by k8s namespace alone. Our `conversations.project_name` already is that axis — filter every query by it. Dev has no milo Extra → use the request namespace as project.
- **No etcd** (`RecommendedOptions.Etcd = nil`). Our storage is Postgres via `internal/history`.
- **`EffectiveVersion "1.36"`** and k8s.io/* pinned at **v0.36.0**; module Go directive **1.26** (matches ipam). Bump the assistant's Dockerfiles / CI to Go 1.26.
- **TableConvertor:** start with `rest.NewDefaultTableConvertor`; add custom `kubectl get` columns (last-active, messages) later.
- **openapi-gen** output is required for full discovery/OpenAPI v3 — generate it in Phase 2/3 (Phase 1 hand-wrote deepcopy/conversion but deferred openapi).

## References
- Template: `../ipam/{cmd/ipam/serve.go,internal/apiserver,internal/registry/ipam/ipallocation,internal/tenant,config}` and `ipam/ARCHITECTURE.md` (aspirational type names — trust the code).
- This repo: `internal/history/{history,postgres}.go` (store + `ListConversations`/`Turns`), `internal/auth/sar.go` (the `assistant.miloapis.com/conversations` SAR model), `internal/config/config.go` (`CONVERSATION_STORE_URL`, SAR defaults), `config/overlays/dev` (dev wiring), `pkg/apis/assistant` (Phase 1 types).
