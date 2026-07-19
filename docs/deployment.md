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
  token to an identity (OIDC still supported); SubjectAccessReview against the
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

## Operations

- **Liveness** `/healthz` (bare process check) · **Readiness** `/readyz`
  (503 until Postgres and, in gateway mode, the model gateway are reachable).
- **Metrics** `/metrics` (Prometheus): request rate/latency/in-flight,
  task-store and readiness errors. Billing usage is a separate CloudEvents
  stream — see [conversations-and-metering.md](conversations-and-metering.md).
- **Logs** are structured JSON with a per-request id (`X-Request-Id` honored or
  minted); prompt/PII content is never logged at info level.
