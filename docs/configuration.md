# Configuration & auth

Environment variables, model backends, and the authentication/authorization seams. Production values live in [deployment.md](deployment.md) and `config/overlays/production/`.

## Configuration (env)

| Var | Default | Description |
| --- | --- | --- |
| `PORT` | `7820` | HTTP listener port |
| `HOST` | `0.0.0.0` | HTTP listener host |
| `PUBLIC_BASE_URL` | `http://localhost:${PORT}` | Base URL for the card interface `url` (→ `<base>/a2a`) and CloudEvents `source` |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `AUTHN_TOKENREVIEW_API_URL` | in-cluster (derived) | Control-plane base URL for the TokenReview call; unset ⇒ derived from `KUBERNETES_SERVICE_HOST/PORT`. Required off-cluster |
| `AUTHZ_SAR_API_URL` | derived in-cluster | Control-plane base URL for the SubjectAccessReview call; required off-cluster. **Also the control plane `CAPABILITY_SOURCE=crd` reads `CapabilityBinding` objects from** |
| `AUTHZ_SAR_CA_CERT_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA bundle verifying that control plane's certificate |
| `AUTHZ_SAR_CLIENT_CERT_PATH` / `AUTHZ_SAR_CLIENT_KEY_PATH` | — | Client certificate identifying the assistant to the control plane. Unset on a plain Kubernetes apiserver, which accepts the service-account token; **required against Milo**, which validates tokens only against its own issuer and 401s a workload-cluster token |
| `AUTHN_TOKENREVIEW_TOKEN_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Assistant's own SA token for the TokenReview call |
| `AUTHN_TOKENREVIEW_CA_CERT_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | Apiserver CA bundle for the TokenReview call |
| `PLATFORM_API_URL` | `AUTHZ_SAR_API_URL` | Platform API the **base platform tools** read and write a project's own resources through. The platform that decides whether a caller may act on a project is the same one that serves that project's resources, so this normally needs no setting. Every request over it carries the **calling user's** own bearer token — the service holds no credential for it, which is why there is no token path beside it. Unset and underivable ⇒ the base tools are not composed |
| `PLATFORM_API_CA_CERT_PATH` | `AUTHZ_SAR_CA_CERT_PATH` | CA bundle verifying the platform API's certificate (same server as the SAR endpoint by default) |
| `PLAN_TOKEN_KEY` | generated per process | Secret that binds a plan to what the person was shown (base64, or a raw string of at least 16 bytes). Set it so one replica can apply a plan another made, and so plans survive a restart. Unset, Patch generates one per process **and warns at startup**: the guarantee still holds, but an outstanding plan is lost on restart or refused by another replica, and the person is asked to plan again |
| `CAPABILITY_SOURCE` | — | Which capability source to build: `fixture` \| `http` \| `crd`. Unset ⇒ no provider capabilities (built-ins only), except for the one-release inference described in [Capability source](#capability-source) |
| `CAPABILITY_DOCS_FIXTURE` | — | Path to a capability-documents JSON file. Required by, and only by, `CAPABILITY_SOURCE=fixture`; setting it under another mode is an error |
| `CAPABILITY_PROVIDER_URL` | — | Base URL of the capability-provider HTTP API. Required by, and only by, `CAPABILITY_SOURCE=http`; setting it under another mode is an error |
| `CONVERSATION_STORE_URL` | — | `postgres://` URL for durable conversation history. Unset ⇒ in-memory (process lifetime). Set but unreachable ⇒ boot fails (no silent fallback to amnesia) |
| `CAPABILITY_ALLOW_PRIVATE_NETWORKS` | `false` | Relax the capability SSRF guard's loopback/RFC1918 block. In-cluster capability endpoints (the AI gateway, provider pods) are private ClusterIPs, so every real deployment sets this `true`; link-local/cloud-metadata stay blocked either way. Leave `false` only when all endpoints are public and providers untrusted |
| `CAPABILITY_IDENTITY_FORWARD_HOSTS` | — | Comma-separated hosts whose MCP endpoints may receive the **calling user's** bearer token and the turn's project (`Authorization` + `X-Datum-Project`), so a provider that reads the customer's own resources can act as that user. An entry matches a host exactly and as a domain suffix (`datum.net` also permits `mcp.datum.net`). Unset ⇒ forwarded to nobody. A capability document is provider-controlled data, so naming an endpoint never grants it a credential — only this list does. See [Identity and access](architecture/identity-and-access.md#acting-as-the-caller) |
| `CAPABILITY_MCP_ENDPOINT_HOSTS` | — | Comma-separated hosts an MCP endpoint may be **dialed at**, matched exactly and as a domain suffix. An endpoint outside the list is skipped before any connection is opened, logged as `capability.mcp.endpoint_not_sanctioned`, and reported on the binding as `Composed=False`. Empty ⇒ the check is disabled and any endpoint may be dialed. Production sets it to the AI gateway. Deliberately **not** the same knob as `CAPABILITY_IDENTITY_FORWARD_HOSTS` — see [Two host lists, not one](#two-host-lists-not-one) |
| `MODEL_MODE` | `anthropic` if key else `mock` | `anthropic` \| `mock` \| `gateway` |
| `ANTHROPIC_API_KEY` | — | Required when `MODEL_MODE=anthropic` |
| `ANTHROPIC_MODEL` | `claude-sonnet-4-6` | Anthropic model id |
| `GATEWAY_URL` | — | Required when `MODEL_MODE=gateway`; Envoy AI Gateway (OpenAI-compatible) base URL |
| `GATEWAY_MODEL` | `patch-stub-v1` | Model name the gateway routes to the upstream |
| `GATEWAY_TOKEN_FILE` | — | Path to a bearer token proving which workload is calling the gateway. Re-read per request, so a rotated token keeps working. Not a model credential — the gateway still injects that |
| `GATEWAY_CA_CERT` | — | Optional CA PEM path for a self-signed gateway TLS cert |
| `GATEWAY_TLS_INSECURE` | `false` | Skip gateway TLS verification (local only) |
| `USAGE_GATEWAY_URL` | — | Usage collector base URL; unset ⇒ emission is a no-op |
| `USAGE_GATEWAY_API_KEY` | — | Optional `x-api-key` for the collector |

> `GATEWAY_URL` (AI gateway, model traffic) is distinct from
> `USAGE_GATEWAY_URL` (the metering collector) — different subsystems.

## Capability source

Provider capabilities — knowledge, tools, skills — reach the assistant through
one `Source` implementation, chosen explicitly by `CAPABILITY_SOURCE`. Three
modes ship, and each takes exactly the configuration it uses:

| `CAPABILITY_SOURCE` | What it reads | What it needs |
| --- | --- | --- |
| `fixture` | A capability-documents JSON file on disk | `CAPABILITY_DOCS_FIXTURE` |
| `http` | The capability-provider HTTP API, per turn, uncached | `CAPABILITY_PROVIDER_URL` |
| `crd` | `CapabilityBinding` objects in each project's control plane, behind a 60s cache | Nothing of its own — the `AUTHZ_SAR_*` control-plane settings below |
| unset | Nothing. The assistant composes base platform tools only | — |

Validation is per mode and refuses anything half-configured: an unknown value is
an error, a mode missing its companion variable is an error, and a companion
variable set for a mode that does not use it is **also** an error. That last one
looks pedantic and is not: it is almost always a half-finished overlay edit, and
failing at boot is how the operator finds out, rather than discovering at the
next incident that the fixture they thought they removed is still the source of
truth.

**The inference shim is temporary — set the mode explicitly now.** Before the
enum existed the mode was implied by whichever companion variable was set. So
for one release, when `CAPABILITY_SOURCE` is unset and exactly one of
`CAPABILITY_DOCS_FIXTURE` or `CAPABILITY_PROVIDER_URL` is set, that mode is
inferred and a deprecation warning is logged at startup. When **both** are set
and no mode is given, boot fails rather than picking one — the operator has not
said which they meant, and guessing would silently serve the wrong catalog. The
inference and its warning are removed in the next release; an overlay that still
relies on them stops composing capabilities at that point, so declare the mode
while the warning is still the only consequence.

### `crd` mode takes the control plane it already has

`CAPABILITY_SOURCE=crd` has no companion variable. The control-plane base URL,
CA bundle, and credential it uses are the ones the SubjectAccessReview path is
already configured with — `AUTHZ_SAR_API_URL`, `AUTHZ_SAR_CA_CERT_PATH`,
`AUTHZ_SAR_CLIENT_CERT_PATH`, `AUTHZ_SAR_CLIENT_KEY_PATH`. Two ways to name one
control plane is two ways to point half the service at the wrong one, and the
half that ends up misdirected here is the one that decides what a project is
entitled to.

**The credential is a client certificate, not the service-account token.** Milo
validates service-account tokens only against its own issuer, so a token minted
by the workload cluster is rejected with a 401 before the body is read; an
in-cluster service identifies itself to Milo by presenting a certificate signed
by the control-plane CA (`internal/auth/transport.go`). A deployment that sets
only `AUTHZ_SAR_TOKEN_PATH` and expects capability reads to work has configured
the path that silently 401s — every project degrades to built-ins, and the only
evidence is `capability.crd.fetch_failed` plus a `refresh_failed` rate on
`assistant_capability_fetch_total`.

Reads are one LIST per project per 60 seconds, cached, serving the last good
answer if a refresh fails. The TTL is a constant rather than configuration, for
the same reason the SAR cache's TTL is: it is a staleness budget the platform
has already justified once, and a second, differently-tuned copy of it would be
a second thing to reason about during an incident. See
[capability-reference.md](capability-reference.md#crd-source-capabilitybinding)
for the object and the degradation contract.

### Two host lists, not one

`CAPABILITY_MCP_ENDPOINT_HOSTS` and `CAPABILITY_IDENTITY_FORWARD_HOSTS` hold the
same value in production — the AI gateway — and are still two variables on
purpose, because they answer different questions. One says an endpoint **may be
dialed at all**; the other says it **may receive the calling user's bearer
token**. Collapsing them would mean that an operator widening the reachable set
— to try a new provider endpoint, say — silently adds an entry to the
credential-forwarding list that nobody reviewed. Widening reachability is a
routine change; widening who gets a customer's credential is not, and the two
must not share an edit.

Neither is `ComposeOptions.AllowedHosts`, the SSRF guard's allow-list. That one
governs *all three* provider-URL sinks — knowledge sources, skill bodies, and
MCP endpoints alike. Setting it to the gateway would confine knowledge and skill
fetches to the gateway too, and those legitimately point at providers' own
documentation hosts. The constraint expressed here is narrower than SSRF: MCP
traffic goes through the gateway, and nothing else about a provider's URLs
changes.

The failure this prevents is a catalog-side one. The capability projection is
responsible for rewriting each MCP `endpoint` to the gateway's MCPRoute URL; if a
regression publishes a provider's raw address instead, the assistant would
otherwise dial it faithfully — and the gateway copy of the tool allow-list, the
metering of that call, and (on a host that happened to match the forward list)
the identity check would all be bypassed at once. The check runs at the dial
site, in `connectTools`, because that is the step an attacker or a buggy
controller cannot route around. Empty disables it, which is what keeps the
fixture path, `e2e/`, and the dev overlays working unchanged.

## Model modes

The model/loop layer is the in-repo **`agentcore`** package — a
provider-neutral library (unified stream parts, a tool loop with
per-step usage aggregation, and adapters). Modes:

- **`anthropic`** — `agentcore/anthropic` over the official
  `anthropic-sdk-go`, keyed by `ANTHROPIC_API_KEY`. Full usage fidelity
  including cache read/write tokens.
- **`mock`** — `agentcore/mockmodel`, a scripted in-process model. It
  exists so the **full** chat path — a provider tool call over real MCP,
  the tool result folded into the final answer, usage reported — is
  provable with **no API key and no model-provider network**.
- **`gateway`** — `agentcore/openaicompat` over the official `openai-go`,
  routed through the **Envoy AI Gateway** (see below).

### Gateway mode

`MODEL_MODE=gateway` points the model client at the Envoy AI Gateway's
OpenAI-compatible endpoint (`GATEWAY_URL`) with model `GATEWAY_MODEL`. It
exercises the production metering/policy path: token usage is counted **at
the gateway** (`llmRequestCosts`) and upstream credentials are injected
**by the gateway** (`BackendSecurityPolicy`).

Two properties this mode guarantees from the service side:

- **No upstream credential in the service.** The client sends **no
  `Authorization` header** — the gateway owns the real key. There is no
  model API key in the service env in this mode.
- **Consumer attribution on every model call.** Each request carries
  `x-datum-project: <projectName>`, `x-datum-conversation: <contextId>`,
  and `x-datum-agent: patch`, so the gateway can meter and attribute usage
  per consumer. (These are attached only in gateway mode — the service
  never leaks project/conversation ids to the real Anthropic API.)

For local TLS, use plain `http://` or set `GATEWAY_CA_CERT` /
`GATEWAY_TLS_INSECURE`.

### Mock model caveat

The mock is a **canned script**, not a language model: if the latest
user message mentions **"diagnose"** and a `…pipeline_diagnose` tool is
available it emits one tool call, then quotes the tool's findings in the
final text (a two-step run); otherwise it returns a short generic reply.
Every response reports **fake-but-nonzero** token usage. This proves
plumbing and event shapes — **not** answer quality or real tool
selection. Treat mock-green as "the wiring holds", not "the assistant is
good".

## Auth (authN + authZ are separate seams)

Two independent interfaces (`internal/auth/`), and **only one implementation of
each**. Both answers come from the control plane; the service decides nothing
about access locally.

- **Authenticator** — *who are you*: bearer token → principal. A token the
  control plane will not vouch for is **401**.
- **Authorizer** — *may you act on this project*: **403** on deny.

The principal carries **only a subject**. It has no grant fields at all, so a
credential cannot describe its own authority and there is no path by which a
token's contents widen what it can reach. Access is re-decided per request.

There is no dev or static-token mode. A local run authenticates the same way a
deployed one does — see [development.md](development.md).

### Authentication — TokenReview

Resolves the bearer token by POSTing a TokenReview to the control plane
(`/apis/authentication.k8s.io/v1/tokenreviews`), authenticating the call with
the assistant's own service-account token + CA (`AUTHN_TOKENREVIEW_TOKEN_PATH` /
`AUTHN_TOKENREVIEW_CA_CERT_PATH`). The endpoint is `AUTHN_TOKENREVIEW_API_URL`,
derived from `KUBERNETES_SERVICE_HOST`/`PORT` when unset.

The principal is the reviewed username. Fail-closed: any transport error,
timeout, non-authenticated status, or empty username → **401**. Successful
resolutions are cached briefly; rejections are never cached.

### Authorization — SubjectAccessReview

Issues a SAR against the control plane (resolved by the platform's OpenFGA-backed
webhook) asking whether the subject may `create`
`conversations.assistant.miloapis.com` in the project's namespace. Override the
triple with `AUTHZ_SAR_GROUP` / `AUTHZ_SAR_RESOURCE` / `AUTHZ_SAR_VERB`;
credentials come from `AUTHZ_SAR_TOKEN_PATH` / `AUTHZ_SAR_CA_CERT_PATH`.

Fail-closed on **every** failure mode — empty subject, transport error, timeout,
non-2xx, missing or negative status — so the control plane is authoritative and
the service never permits on doubt.

Caching is deliberately asymmetric: **allows** are cached for a short TTL,
**denies never are**. A revoked user therefore keeps access for at most that
window, while a just-granted user is never locked out, because the next request
re-checks and permits immediately.

### What the assistant is permitted to do

In the cluster it runs in, only `system:auth-delegator` — create TokenReviews
and SubjectAccessReviews (`config/base/rbac.yaml`). It never acquires the
caller's authority: it asks the control plane questions about the caller, and
the answer is a yes or a no.

In `CAPABILITY_SOURCE=crd` mode it also reads one project resource, as itself:
`get`/`list` on `capabilitybindings` and `patch` on
`capabilitybindings/status`, granted by a ClusterRole at Milo root
(`config/milo/rbac/control-plane-auth-delegator.yaml`) rather than in this
cluster, because that is where the objects are. Reading a project's entitlement
with the service's own identity is a different thing from acting with the
caller's — nothing there forwards or borrows a user's credential — but the
"reads no project resource" half of the old promise is no longer literally true,
and a reader checking that claim against the code should know which half
survived.

### Boot requirements

The control-plane endpoint is required. In-cluster it derives from the injected
service env and needs no configuration; off-cluster, `AUTHN_TOKENREVIEW_API_URL`
and `AUTHZ_SAR_API_URL` must be set, or the service refuses to boot rather than
start in a state where every request is undecidable.
