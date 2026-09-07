# Identity and access

Two questions gate every turn: who is calling, and may they act on this project.
Patch answers neither itself. The Milo control plane answers both, and the
service is built so that it cannot decide otherwise.

## Two seams

- **Authentication**: a bearer token resolves to a subject through a Kubernetes
  [TokenReview][tokenreview]. A token the control plane will not vouch for is
  rejected with 401.
- **Authorization**: a [SubjectAccessReview][sar] decides whether that subject
  may act on the named project. A denial is 403.

The two are separate interfaces, so the identity source and the access decision
change independently.

## The credential carries no authority

An authenticated caller is represented by a subject and nothing else. There is
no grant list, no project claim, and no wildcard field on the identity Patch
builds. A credential therefore cannot describe what it may reach, and access is
re-decided from the control plane on every request rather than read out of the
token.

This removes a class of escalation rather than defending against it. There is no
code path in which a token's own contents widen what it can do, because there is
nowhere for such contents to live.

## What the control plane is asked

The SubjectAccessReview asks whether the subject may `create`
`conversations.assistant.miloapis.com` in the project's namespace. Modelling
project access as a Kubernetes permission means entitlement is expressed once,
in the platform, and Patch inherits it — including revocation.

Patch authenticates its own review calls with its service account, which holds
`system:auth-delegator` and nothing else. It can ask the control plane questions
about a caller; it is never *granted* that caller's authority, and it holds no
read access to any project resource of its own. Where a provider tool must read
a customer's resources, it borrows the caller's credential for that one call —
see below.

## Acting as the caller

Some provider tools read the customer's own resources. The compute plugin's MCP
server is the first: it clears every credential it might have, sets the token on
the request, and reads as that bearer. It therefore holds no standing access to
anyone's project, and needs no impersonation privilege — but it does need the
caller's credential and the project to read in.

So a turn forwards two headers to such a server:

| Header | Value | Source |
|---|---|---|
| `Authorization` | `Bearer <the caller's own token>` | the authenticated request |
| `X-Datum-Project` | the project the turn was authorized for | the authorized request, never a tool argument |

The project comes from the request that a SubjectAccessReview already approved.
A tool *argument* naming a project would be steerable by prompt injection, which
is why neither side reads one.

### Who may receive it

Forwarding a raw end-user token is credential spreading, and a capability
document is provider-controlled data: a document could name any endpoint at all
and harvest tokens from every project entitled to it. So naming an endpoint
grants it nothing. The credential travels only to hosts an **operator**
sanctioned out-of-band, in `CAPABILITY_IDENTITY_FORWARD_HOSTS`, and the empty
default forwards to nobody. On this platform that list is the AI gateway that
already fronts every provider endpoint and enforces the tool allow-list a second
time.

That list is deliberately separate from the SSRF allow-list. The SSRF guard
answers "may we connect at all"; this answers the much narrower "may we hand
this endpoint the user's credential", and an endpoint can clear the first
without clearing the second.

The decision fails closed on every doubt — no token, no authorized project, no
sanctioned list, an unparseable endpoint, a host outside the list — and the
headers travel as a pair, since a server that cannot read as the caller has no
use for the project name. Knowledge sources and skill bodies are excluded
outright: they are static documents fetched with a plain GET, named by the same
provider-controlled document, so carrying a credential there would widen the
harvest surface for nothing gained.

### What this is not

It is not a scoped delegation. The forwarded token is the caller's full-strength
credential: an MCP server that receives it can do anything its bearer can do,
not merely the reads the capability document declares, and its lifetime is the
token's own rather than the turn's. The sanctioned-host list bounds *who* is
trusted with it; nothing yet bounds *what* they may do with it.

The end state is a token exchange ([RFC 8693][rfc8693]): before a tool call the
assistant swaps the caller's token for a short-lived one, audience-bound to that
one provider endpoint and scoped to the reads the document declares, so a
compromised or curious provider holds something that is useless elsewhere and
expires in minutes. That needs an exchange endpoint on the control plane, an
audience per provider service, and a scope vocabulary derived from
`spec.authority` — a platform project rather than a service change, which is why
the sanctioned-host list stands in the meantime.

## Failing closed

Every failure is a rejection: an empty subject, a transport error, a timeout, a
non-2xx response, or a status the service cannot interpret. The control plane is
authoritative, and Patch never permits on doubt. A service that cannot prove who
it is refuses to authenticate anyone, which is why the endpoint is required at
startup — Patch declines to boot rather than run in a state where every request
is undecidable.

## Caching is asymmetric on purpose

Successful decisions are cached briefly. Denials are never cached.

The asymmetry sets the two error costs against each other deliberately. A
revoked user keeps access for at most the cache window, which is bounded and
short. A newly granted user is never locked out, because the next request
re-asks and is permitted immediately.

## Deny by default over methods

Authorization runs before the A2A library dispatches, and only four methods can
be authorized: `SendMessage`, `SendStreamingMessage`, `GetTask`, and
`CancelTask`. Everything else is rejected, including methods the library
supports and methods that do not exist.

`ListTasks` is the instructive case. It has no project parameter, so an exposed
implementation would return every project's tasks, message history included, to
any valid token — including one entitled to nothing. Scoping it safely needs an
endpoint that threads the caller's entitlement into the query, so until that
exists the method is refused rather than served.

For `GetTask` and `CancelTask`, the project comes from the stored task, not from
the request. A caller cannot name someone else's project to reach their task.

## Related documentation

- [Architecture overview](./README.md)
- [Conversation turn](./conversation-turn.md) — where these checks run.
- [Configuration](../configuration.md) — endpoints, credentials, and timeouts.
- [Deployment](../deployment.md) — the production posture.

[rfc8693]: https://datatracker.ietf.org/doc/html/rfc8693
[tokenreview]: https://kubernetes.io/docs/reference/access-authn-authz/authentication/#webhook-token-authentication
[sar]: https://kubernetes.io/docs/reference/access-authn-authz/authorization/#checking-api-access
