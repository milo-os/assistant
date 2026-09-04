# AssistantDegraded

**Alert:** `AssistantDegraded`
**Severity:** Warning
**Fires after:** 15 minutes below the desired replica count
**Source:** `config/overlays/production/alerts.yaml`, group `assistant.availability`

## What this means

At least one `assistant` pod has been unable to reach Ready for 15 minutes
while others keep serving. Requests are still answered, with less headroom.

At a single replica this fires alongside `AssistantDown` and the critical one
is the useful signal. It earns its place as the replica count rises — which
under the HPA in the production overlay it will.

## Triage

```
kubectl -n patch-system get pods -l app=assistant -o wide
kubectl -n patch-system describe pod <the-unready-one>
kubectl -n patch-system rollout status deployment/assistant
```

Check which probe is failing. `/readyz` gates on the conversation store and the
model gateway; `/healthz` is a bare process check. **One unready pod among
healthy ones points at something pod-local**, not a shared dependency — if a
dependency were down, every pod would be unready and `AssistantDown` would have
fired instead.

## Common causes

**Rollout in progress.** `maxUnavailable: 0, maxSurge: 1` means a slow-starting
new pod shows up here. Confirm with `rollout status` before digging further.

**Node pressure or eviction.**

```
kubectl -n patch-system get events --field-selector involvedObject.name=<pod>
```

**Postgres connection exhaustion.** If one pod cannot get a connection while
others hold theirs, the CNPG cluster may be at its connection limit.

## Resolution

If a single pod is stuck with no clear cause, delete it and let the Deployment
replace it. If replacements keep failing readiness, treat it as
[assistant-down.md](assistant-down.md) and work the dependency checks there.
