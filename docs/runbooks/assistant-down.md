# AssistantDown

**Alert:** `AssistantDown`
**Severity:** Critical
**Fires after:** 5 minutes with zero available replicas
**Source:** `config/overlays/production/alerts.yaml`, group `assistant.availability`

## What this means

No `assistant` pod is Ready, so every A2A request is failing.

Know this before you start: `/readyz` gates on **dependency reachability** —
the Postgres conversation store *and* the model gateway — while `/healthz` is a
bare process check (see `internal/server`). A pod that is Running but not Ready
is usually reporting a broken dependency, not a broken assistant.

## Triage

```
kubectl -n patch-system get pods -l app=assistant
kubectl -n patch-system describe deployment assistant
```

**Running but not Ready** → go straight to dependencies:

```
kubectl -n patch-system get cluster conversation-store
kubectl -n patch-system get pods -l cnpg.io/cluster=conversation-store
kubectl -n patch-system logs deploy/assistant | grep -i readyz
```

**CrashLoopBackOff / ImagePullBackOff** → the assistant or its image:

```
kubectl -n patch-system logs deploy/assistant --previous
kubectl -n patch-system get events --sort-by=.lastTimestamp | tail -20
```

## Common causes

**Conversation store unreachable.** `AssistantConversationStoreUnhealthy` will
usually have fired first — follow
[conversation-store-unhealthy.md](conversation-store-unhealthy.md).

**Model gateway unreachable.** Readiness gates on it. Confirm the gateway
Service has endpoints; a selector-based alias Service still resolves in DNS
when it has no backends, so a successful `nslookup` proves nothing.

**Model calls rejected.** If the gateway is fronted by a policy requiring a
credential and `MODEL_MODE=gateway` is sending none, every model call returns
401. Readiness may still pass while all real work fails — check the logs for
401s on the gateway leg.

**`GATEWAY_URL` missing its `/v1` suffix.** The OpenAI-compatible client
appends `/chat/completions`; without `/v1` every model call 404s. See
`docs/configuration.md`.

**Image pull.** `ghcr.io/milo-os/assistant` must be published and public.

## Escalation

If the cause is the model gateway or the control plane rather than the
assistant, hand off to whoever owns that component rather than continuing here.
