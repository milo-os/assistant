# Deployment

Patch runs on Kubernetes as a stateless service backed by a PostgreSQL
(CloudNativePG) store. Two postures ship in-repo, both kustomize overlays on
`config/base`:

| Overlay | Purpose |
|---|---|
| `config/overlays/dev` | Local kind environment (fixture capabilities, stub model). `task dev:setup`. |
| `config/overlays/dev-catalog` | Local kind, capabilities from the service catalog. `task dev:deploy OVERLAY=dev-catalog`. |
| `config/overlays/production` | Production posture (TokenReview + SAR, TLS, HA store, backups). Reviewed-render, GitOps-applied. |

The local environment is covered in [development.md](development.md).

## Published artifacts

Every push to `main` and every release publishes two OCI artifacts
(`.github/workflows/build.yaml`):

| Artifact | Contents |
|---|---|
| `ghcr.io/milo-os/assistant` | The service image, built from `deploy/assistant.Dockerfile`. |
| `ghcr.io/milo-os/assistant-kustomize` | The whole `config/` tree, with the released image tag stamped into `config/base` so every overlay inherits it. |

`config/milo` rides in that bundle but is **not** part of the workload: it is
the assistant's IAM projection — a `ProtectedResource` registering
`assistant.miloapis.com/conversations.*` plus the Roles that grant it — and it
is applied to the *Milo control plane*, not to the cluster the service runs in.
Without it the SubjectAccessReview below has no permission to evaluate and,
because that check fails closed, every request 403s.

## Production

Render and review, then apply through your CD pipeline:

```bash
kubectl kustomize config/overlays/production
```

It is intentionally not a one-shot `kubectl apply` — it references Secrets and
hostnames you supply. The full operator guide (required Secrets, placeholders,
the posture-vs-dev table, SAR RBAC, and follow-ups) lives next to the overlay:
[`config/overlays/production/README.md`](../config/overlays/production/README.md).

What the production overlay establishes, at a glance:

- **AuthN/Z**: TokenReview against the Milo control plane resolves each bearer
  token to an identity; SubjectAccessReview against the
  same control plane authorizes per-project access (both fail-closed). No dev
  tokens.
- **Data**: a 3-instance CloudNativePG cluster with continuous backup / PITR,
  backing both conversation history and the durable task store.
- **Availability**: 3 replicas, HPA 3–12, PodDisruptionBudget, rolling updates
  gated on `/readyz` (which checks Postgres + gateway reachability).
- **Security**: non-root, read-only rootfs, dropped capabilities, seccomp; a
  default-deny NetworkPolicy; TLS terminated at a Gateway-API Gateway, with only
  `/a2a` and `/.well-known` exposed publicly (`/metrics` and probes stay
  internal).
- **Config safety**: the service refuses to boot on an unsafe posture (dev auth
  with a public HTTPS URL, dev tokens left set in a non-dev auth mode, a
  plaintext gateway to an external host).

### Capability source, and the `crd` cutover

Production runs `CAPABILITY_SOURCE=http` today: capability documents are pulled
per turn from the service catalog's provider API. The `crd` source — reading
`CapabilityBinding` objects from each project's Milo control plane behind a 60s
cache, and writing `status.conditions` back — is implemented and pre-staged in
the overlay as a commented block, but **not enabled**. Two gates are open and
neither is this repository's to close: the catalog's projection controller is
unpushed (so nothing writes the objects, and flipping today would compose zero
capabilities for every project), and the assistant's root-RBAC grant has not
been observed resolving against a real project path.

The procedure — both gates, the flip, what to watch in the first ten minutes,
and the one-env-var rollback — lives with the triage it shares an hour with, in
[`docs/runbooks/capability-source-degraded.md`](runbooks/capability-source-degraded.md#cutover-turning-on-capability_sourcecrd).
It is a runbook and not a section here because the operator reaching for it is
either cutting over or backing out, and both need the same "which failure is
this" table above the fold.

Note that a green `config/overlays/dev-crd` is not evidence for the production
topology: kind has one apiserver and no project router, so the control-plane
path resolves to the same server and root RBAC applies trivially. It proves the
object model and the cache; the flip needs a staging soak against a real Milo.

## Operations

- **Liveness** `/healthz` (bare process check) · **Readiness** `/readyz`
  (503 until Postgres and, in gateway mode, the model gateway are reachable).
- **Metrics** `/metrics` (Prometheus): request rate/latency/in-flight,
  task-store and readiness errors, and — under `CAPABILITY_SOURCE=crd` — the
  capability fetch/cache/status-write series, which are the *only* signal for a
  capability failure because one degrades a chat rather than failing it. Nothing
  in this repo scrapes production; see
  [operations/dashboards-and-alerts.md](operations/dashboards-and-alerts.md).
  Billing usage is a separate CloudEvents stream — see [conversations-and-metering.md](architecture/metering.md).
- **Logs** are structured JSON with a per-request id (`X-Request-Id` honored or
  minted); prompt/PII content is never logged at info level.
