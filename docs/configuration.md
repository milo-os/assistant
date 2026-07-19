# Configuration & auth

Environment variables, model backends, and the authentication/authorization seams. Production values live in [deployment.md](deployment.md) and `config/overlays/production/`.

## Configuration (env)

| Var | Default | Description |
| --- | --- | --- |
| `PORT` | `7820` | HTTP listener port |
| `HOST` | `0.0.0.0` | HTTP listener host |
| `PUBLIC_BASE_URL` | `http://localhost:${PORT}` | Base URL for the card interface `url` (→ `<base>/a2a`) and CloudEvents `source` |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `AUTH_MODE` | `dev` | `dev` \| `oidc` \| `tokenreview` (production default) |
| `AUTH_DEV_TOKENS` | — | Required in dev mode (format in the Auth section below) |
| `OIDC_ISSUER` | — | Required in oidc mode |
| `OIDC_AUDIENCE` | — | Required in oidc mode |
| `OIDC_PROJECTS_CLAIM` | `projects` | JWT claim carrying granted projects |
| `AUTHN_TOKENREVIEW_API_URL` | in-cluster (derived) | Control-plane base URL for the TokenReview call; unset in tokenreview mode ⇒ derived from `KUBERNETES_SERVICE_HOST/PORT` |
| `AUTHN_TOKENREVIEW_TOKEN_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Assistant's own SA token for the TokenReview call |
| `AUTHN_TOKENREVIEW_CA_CERT_PATH` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | Apiserver CA bundle for the TokenReview call |
| `CAPABILITY_DOCS_FIXTURE` | — | Path to a capability-documents JSON file (fixture source); mutually exclusive with `CAPABILITY_PROVIDER_URL` |
| `CAPABILITY_PROVIDER_URL` | — | Base URL of the capability-provider HTTP API (HTTP source); mutually exclusive with `CAPABILITY_DOCS_FIXTURE`. Both unset ⇒ no provider capabilities |
| `CONVERSATION_STORE_URL` | — | `postgres://` URL for durable conversation history. Unset ⇒ in-memory (process lifetime). Set but unreachable ⇒ boot fails (no silent fallback to amnesia) |
| `CAPABILITY_ALLOW_PRIVATE_NETWORKS` | `false` | Relax the capability SSRF guard's loopback/RFC1918 block. In-cluster capability endpoints (the AI gateway, provider pods) are private ClusterIPs, so every real deployment sets this `true`; link-local/cloud-metadata stay blocked either way. Leave `false` only when all endpoints are public and providers untrusted |
| `MODEL_MODE` | `anthropic` if key else `mock` | `anthropic` \| `mock` \| `gateway` |
| `ANTHROPIC_API_KEY` | — | Required when `MODEL_MODE=anthropic` |
| `ANTHROPIC_MODEL` | `claude-sonnet-4-6` | Anthropic model id |
| `GATEWAY_URL` | — | Required when `MODEL_MODE=gateway`; Envoy AI Gateway (OpenAI-compatible) base URL |
| `GATEWAY_MODEL` | `patch-stub-v1` | Model name the gateway routes to the upstream |
| `GATEWAY_CA_CERT` | — | Optional CA PEM path for a self-signed gateway TLS cert |
| `GATEWAY_TLS_INSECURE` | `false` | Skip gateway TLS verification (local only) |
| `USAGE_GATEWAY_URL` | — | Usage collector base URL; unset ⇒ emission is a no-op |
| `USAGE_GATEWAY_API_KEY` | — | Optional `x-api-key` for the collector |

> `GATEWAY_URL` (AI gateway, model traffic) is distinct from
> `USAGE_GATEWAY_URL` (the metering collector) — different subsystems.

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

Two independent interfaces (`internal/auth/`):

- **Authenticator** — *who are you*: bearer token → principal (subject +
  the project grants the credential carries). Selected by `AUTH_MODE`. A
  bad token is **401**.
- **Authorizer** — *may you act on this project*, **403** on deny. It is
  fail-closed and async so a control-plane call can slot in behind it
  unchanged.

v0 uses a claims authorizer for both auth modes: it decides from the
grants the credential carries (dev-token list / OIDC claim). In
production this seam becomes a **SubjectAccessReview authorizer** issuing
a SAR against the Milo control plane (resolved by the platform's
OpenFGA-backed webhook) — identical 401/403 semantics, swapped with no
call-site churn. The dev-token grants are the v0 stand-in.

### `AUTH_MODE=dev` (static bearer tokens)

`AUTH_DEV_TOKENS` is a `;`-separated list of `token:subject:projects`
entries, where `projects` is a comma-separated grant list and `*` grants
every project:

```
AUTH_DEV_TOKENS=dev-token:local-user:demo-project,other-project;admin:root:*
```

- unknown token → **401**
- known token, project not in its grant list → **403**

### `AUTH_MODE=oidc` (JWT / JWKS)

Verifies the bearer JWT against `OIDC_ISSUER`'s JWKS (default JWKS URI
`<issuer>/.well-known/jwks.json`) and checks `aud == OIDC_AUDIENCE`.
Granted projects are read from a JWT claim (`OIDC_PROJECTS_CLAIM`,
default `projects`; array or space/comma-delimited string). A token with
no such claim grants no projects. Invalid signature / audience / issuer /
expiry → **401**. Unit-tested with a locally generated key (no live IdP
needed).

### `AUTH_MODE=tokenreview` (control-plane TokenReview) — production default

Resolves the bearer token to an identity by POSTing a TokenReview to the
Kubernetes/Milo control plane (`/apis/authentication.k8s.io/v1/tokenreviews`),
authenticating the call with the assistant's own service-account token + CA
(`AUTHN_TOKENREVIEW_TOKEN_PATH` / `AUTHN_TOKENREVIEW_CA_CERT_PATH`; the endpoint
is `AUTHN_TOKENREVIEW_API_URL`, derived in-cluster when unset). The principal
carries **only the subject** (the reviewed username) — it carries **no project
grants**; per-project access is decided separately by the SubjectAccessReview
**authorizer** (`AUTHZ_MODE=sar`). Fail-closed: any transport error, timeout,
non-authenticated status, or empty username → **401**. Successful resolutions
are cached briefly (short TTL); rejections are never cached. This is the
production posture; `oidc` remains a supported alternative.

