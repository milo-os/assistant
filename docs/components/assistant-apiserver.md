# Assistant apiserver

The assistant apiserver presents conversations and capability gap reports as
Kubernetes resources under `assistant.miloapis.com`. It is a separate binary
from the assistant, registered with the kube-aggregator.

## Why it exists

Customers need to list and resume their threads, and providers need to see the
gaps reported against their services. Both are reads over state the assistant
already owns.

Serving them as Kubernetes resources rather than as bespoke endpoints means the
read path inherits platform identity and RBAC directly. A customer's access to
their conversations is expressed the same way as their access to any other
resource they own, and Patch does not carry a second authorization model for
reads.

## Responsibilities

- **Resource surface**: serves `Conversation` and `CapabilityGapReport` as
  read-oriented resources.
- **Delegated authentication**: resolves callers through the aggregation layer,
  so a `kubectl` client authenticates exactly as it does elsewhere.
- **Delegated authorization**: defers access decisions to the control plane.
- **Storage projection**: reads the conversation store and projects rows as API
  objects.

## Structure

The binary is a generic Kubernetes apiserver with bespoke REST storage. Instead
of etcd, its storage layer reads the same PostgreSQL database the assistant
writes, so there is one source of truth and no synchronization between a write
model and a read model.

Resources are read-oriented by design. Conversations are produced by having a
conversation, not by creating an object, so the API exposes what exists rather
than accepting writes that would bypass the turn path.

## Identity

The apiserver has its own service account and RBAC, separate from the
assistant's: `system:auth-delegator` for delegated authentication and
authorization, permission to read the `extension-apiserver-authentication`
configmap, and the flow-control informers the generic apiserver runs.

## Deployment

An `APIService` registers the group with the kube-aggregator. Development skips
TLS verification against the server's self-signed certificate; production
injects a real CA bundle instead.

## Related documentation

- [Architecture overview](../architecture/README.md)
- [Conversation storage](../architecture/conversation-storage.md) — the state
  this projects.
- [Assistant](./assistant.md) — the writer of that state.
- [Deployment](../deployment.md) — registration and TLS posture.
