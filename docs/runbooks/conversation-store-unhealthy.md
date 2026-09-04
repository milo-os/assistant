# AssistantConversationStoreUnhealthy

**Alert:** `AssistantConversationStoreUnhealthy`
**Severity:** Critical
**Fires after:** 5 minutes with no ready CloudNativePG instance
**Source:** `config/overlays/production/alerts.yaml`, group `assistant.availability`

## What this means

Every instance of the `conversation-store` CloudNativePG cluster is unready.
The assistant fails readiness when it cannot reach the store, so
`AssistantDown` will follow within minutes.

Conversation history and task state live here, and nowhere else — the assistant
holds no other durable state.

## Triage

```
kubectl -n patch-system get cluster conversation-store -o wide
kubectl -n patch-system get pods -l cnpg.io/cluster=conversation-store
kubectl -n patch-system describe cluster conversation-store
kubectl -n patch-system logs conversation-store-1 -c postgres
```

CloudNativePG writes detailed failure reasons into events:

```
kubectl -n patch-system get events --sort-by=.lastTimestamp | grep conversation-store
```

## Common causes

**Storage.** Check for pending PVCs, the storage class, and quota:

```
kubectl -n patch-system get pvc
```

**Node placement.** If the cluster carries a `nodeSelector` and tolerations for
a dedicated database node pool, pods stay Pending when that pool is unavailable
or has been relabelled.

**Failover in progress.** A primary switchover briefly leaves no ready
instance. If it recovers within a few minutes, the remaining work is
understanding why the switchover happened.

## Recovery

Recovery depends on how the deploying environment configured backups.
`config/overlays/production/cnpg-cluster.yaml` defines a `barmanObjectStore`
and a daily `ScheduledBackup`; an environment that overrides those away has no
recovery path, and losing the volume means losing conversation history.

Confirm which case you are in before deleting anything:

```
kubectl -n patch-system get cluster conversation-store \
  -o jsonpath='{.spec.backup}{"\n"}'
kubectl -n patch-system get backups
```

The assistant starts clean against an empty database, so a recreated cluster
restores service — it does not restore history.

## Secret coupling worth knowing

The cluster name is load-bearing. CloudNativePG generates a Secret named
`<cluster>-app`, and the Deployment reads `CONVERSATION_STORE_URL` from
`conversation-store-app`, key `uri`. Recreating the cluster under a different
name breaks that `secretKeyRef` silently.
