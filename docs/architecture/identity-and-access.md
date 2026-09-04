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
about a caller; it never acquires that caller's authority, and it holds no read
access to any project resource.

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

[tokenreview]: https://kubernetes.io/docs/reference/access-authn-authz/authentication/#webhook-token-authentication
[sar]: https://kubernetes.io/docs/reference/access-authn-authz/authorization/#checking-api-access
