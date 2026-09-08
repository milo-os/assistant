---
status: implemented
---

# Per-project extended agent card

> Design record. It describes the decision as it was taken; the shipped
> behavior is documented under [docs/architecture](../architecture/README.md).

## Context

`GET /.well-known/agent-card.json` served one hardcoded card, with a single
generic `project-assistant` skill, to every caller. But Patch's capabilities are
composed per project from that project's capability documents — the card was
describing an assistant nobody actually gets. Routers, portals, and peer agents
learned nothing about what a given project can do, and `patch card` printed the
same line for every project.

The card cannot simply be widened, because it is served unauthenticated.
Provider service names, tool names, and MCP endpoints are entitlement facts: who
buys what from whom. They cannot go on a document anyone can `curl`.

## Design

### 1. Two cards, one disclosure line

The public card stays byte-identical except for one capability flag
(`capabilities.extendedAgentCard: true`). It still names no provider, no tool,
and no endpoint. Everything project-specific moves behind the A2A-standard
`GetExtendedAgentCard`, which the auth middleware authorizes like any other
project-scoped method. That is the whole disclosure rule: **the public card
carries nothing about any project; the authenticated card carries only the
services the authorized project's own documents grant it.**

The flag is a prerequisite, not decoration. a2a-go's handler answers
`ErrUnsupportedOperation` when its `WithCapabilityChecks` copy says false —
before consulting the producer — and `a2aclient` short-circuits unless the
resolved public card sets it. So the handler and the card read one shared
`assistanta2a.Capabilities()` value; letting the two drift silently disables the
method.

### 2. The project rides on `tenant` — a deliberate overload

`a2a.GetExtendedAgentCardRequest` has exactly one field, `Tenant`. A2A defines it
as the ID of the agent owner, and `internal/tenant` uses "tenant" for the milo
project scoping an apiserver read. Here it is neither: it is the milo **project**
the caller is asking about, because the request carries no other field and the
project is what scopes every capability in this service. The overload is recorded
on `assistanta2a.CardRequest` and on the middleware helper that reads it.

The middleware authorizes the same field the producer reads, so authorization
and card content cannot diverge. `SendMessage` keeps authorizing on
`message.metadata.projectName` and deliberately ignores any `tenant` on its
params — a2a-go's client transport auto-stamps a tenant onto *every* method, so
honoring it there would let a client-side default pick the authorized project.
For the same reason `patch` never sets a client-level or context tenant; it
passes the project on the one request that means it.

### 3. One skill per entitled service, derived from documents

`capability.Entitlements` turns already-scoped capability documents into one
`ServiceEntitlement` per service (serviceRef, serviceName, namespaced tool names
from `ToolSelector.Include`, MCP endpoints, published skill names), reusing
`NamespaceToolName` and first-registration-wins on tool and skill names so card
names match the names a turn would register. Two documents that share a
`serviceRef` **merge** into one entitlement rather than the later one being
dropped, because `Compose` registers every document's servers and skills — a
drop would advertise less than the next turn actually runs. `internal/a2a` renders one `a2a.AgentSkill` per entitlement,
with the generic `project-assistant` skill kept first so a project entitled to
nothing still advertises something.

It reads **documents, not a `Composed`**, which is what keeps Patch's own
built-ins (`load_skill`, `memory_remember`, `memory_forget`,
`report_capability_gap__*`) off a card that is supposed to describe provider
surface.

### 4. The same scope gate a turn uses

`Conversation.Entitlements` fetches through the same `Source` and the same
`capability.ScopeDocuments` gate a turn's `Compose` runs, so the card can never
promise a service the next turn would not build. That ordering is the security
property, not a tidiness one: `FixtureSource` ignores the project entirely and
returns the whole file, so in fixture mode — which is what staging runs —
`ScopeDocuments` is the *only* project gate. It is pinned by a test.

The producer therefore refuses to run on an empty project: `ScopeDocuments`
returns documents unchanged when the expected project is empty, so an empty
`tenant` returns the generic public card instead (which is also why the
middleware skips authorization for that case).

### 5. Optional, and degrading

`SkillAdvertiser` is a consumer-side interface asserted off the runner, exactly
like `Compactor`: a runner that cannot describe entitlement simply doesn't offer
the method (`ErrExtendedCardNotConfigured`) rather than failing to boot. A source
failure or advertiser error degrades to the generic card and never a 500 — and
never to a cached or default document set.

## The card claims entitlement, not health

The derivation opens no MCP connection. `Compose` filters
`ToolSelector.Include` against a live `tools/list`; the card uses the declared
include-list only, so the card is a superset of what a turn would compose. This
divergence is accepted and stated so that no consumer reads the card as a
liveness signal: an entitled service that is down still appears on the card.

## The middleware had to fail closed first

Authorization peeked the JSON-RPC body with `json.Unmarshal` and, when that
failed, let the request through for the handler to reject. But a2a-go dispatches
with `json.Decoder`, which stops at the first JSON value and ignores trailing
bytes, so a body `json.Unmarshal` rejected was still executed — appending one
junk byte skipped authorization entirely.

That was a pre-existing hole, but this change routes a new cross-project
disclosure through it, so it is fixed here: `peekEnvelope` decodes exactly as the
handler does, rejects trailing data, and returns a 400 instead of deferring.
`TestTrailingBytesCannotSkipAuthorization` pins it for the card, for
`SendMessage`, and for a deny-by-default method.

## Known limits

- **Namespace-less documents land on every project's card.** `ScopeDocuments`
  passes documents with no `metadata.namespace` by design — the same posture a
  turn takes — so a fixture document without one is advertised to every project.
  Correct for a turn, more visible on a card; it is a property of the fixture
  data, not of the card path.
- **Skill IDs are sanitized service refs** (`SanitizeName`, the same rule tool
  names use), so they are only as stable as the provider's `serviceRef.name`.
  Two refs that differ only in characters the sanitizer strips collapse to one
  skill; their surfaces merge, so nothing is under-advertised.
- **The public card advertises `extendedAgentCard: true` unconditionally**, but
  the producer is only registered when a `SkillAdvertiser` is wired. A
  deployment without one answers `EXTENDED_AGENT_CARD_NOT_CONFIGURED`, so a
  client learns this a round trip later than it could.
- **The card is still unsigned** (no `signatures`), same follow-up as the public
  card.

## Non-goals

- **One skill per tool.** Tools are named inside the service skill's description
  and tags; a card with a skill per tool is a tool catalog, which is what MCP
  itself is for.
- **Invented examples.** A service skill carries examples only when the provider
  published skill descriptions to draw them from.
- **Health or reachability on the card.** See above — a router that needs to know
  whether a service answers should call it.

## Files touched/added

- `internal/capability/entitlement.go` (new) — `ServiceEntitlement`,
  `Entitlements`; `compose.go` — `scopeDocuments` exported as `ScopeDocuments`.
- `internal/agent/conversation.go` — `Conversation.Entitlements`.
- `internal/a2a/card.go` — shared `Capabilities()`, `BuildExtendedAgentCard`,
  `ServiceSkills`; `runner.go` — `CardRequest`, `SkillAdvertiser`.
- `internal/server/server.go` — `Deps.SkillAdvertiser`, extended-card producer,
  shared capability checks; `middleware.go` — `GetExtendedAgentCard` allowed and
  authorized on `tenant`.
- `cmd/assistant/runner.go`, `cmd/assistant/main.go` — `ProjectSkills` and the
  optional assertion.
- `cmd/patch/args.go`, `run.go`, `render.go` — `patch card [--project <p>]`.
- `docs/api.md`, `docs/components/assistant.md`,
  `docs/architecture/capabilities.md`.
