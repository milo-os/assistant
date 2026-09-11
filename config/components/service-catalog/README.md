# Service catalog

The assistant's catalog identifiers — everything the services-operator
and downstream Milo controllers need in order to recognise it as a
billable service, fan out its meters, and register its consumer portal
plugin.

Moved here from `datum-cloud/cloud-portal` (`config/services/`), which
owned it only because the assistant's chat feature used to live
entirely inside cloud-portal. Now that assistant is its own service
with its own repo and deployment, it owns its own producer `Service`
objects the same way every other producer does (e.g.
`compute/config/components/service-catalog`) — see [#2 review
feedback](https://github.com/datum-cloud/services/pull/2): the
`services` repo is the API surface that gets added to Milo; concrete
`Service` + `ServiceConfiguration` registrations are deployable
artefacts the producing service publishes as part of its own
deployment process.

## Contents

| Kind                   | Name                     | What it declares                                                                |
| ---------------------- | ------------------------ | -------------------------------------------------------------------------------- |
| `Service`              | `assistant-miloapis-com` | `serviceName`, display metadata, producer owner.                                 |
| `ServiceConfiguration` | `assistant-miloapis-com` | The Conversation MonitoredResourceType, five conversation metrics, billing, and the consumer portal plugin (`userInterface.consumer`). |

Both ship in `phase: Published`.

## Why one `ServiceConfiguration` instead of raw `MeterDefinition`s

The canonical producer-facing document for billing is the
`services.miloapis.com/v1alpha1.ServiceConfiguration` — see
[`billing/docs/emitting-usage.md`](https://github.com/datum-cloud/billing/blob/main/docs/emitting-usage.md).
A producer authors _one_ document; the services-operator fans it out
into `billing.miloapis.com/MeterDefinition` and `MonitoredResourceType`
objects stamped `app.kubernetes.io/managed-by: services-operator`.
Producers are explicitly told not to author those downstream CRDs
directly — edit the `ServiceConfiguration` and let the fan-out catch
up.

## Immutability after `Published`

Per the CRD contract:

- `spec.metrics[].name`, `.kind`, `.unit` are immutable.
- `spec.monitoredResourceTypes[].type` and `.gvk` are immutable.

Adding an optional label or a new metric is additive and safe.
Removing or renaming either is breaking — version the name (e.g.
`assistant.miloapis.com/conversation/input-tokens/v2`).

## Source-of-truth pinning

The `serviceName`, metric names, and Conversation Kind appear in two
places that must move together:

| Surface               | File                                                    |
| ---------------------- | -------------------------------------------------------- |
| Catalog (declarative) | `service-configuration.yaml` (this dir)                |
| Wire-side constants   | `internal/usage/meters.go`                             |

Any change to a metric name or to the `Conversation` Kind here MUST be
made in that file in the same PR.

Note: `internal/usage/meters.go` also defines `MeterToolInvocations`
(`assistant.miloapis.com/conversation/tool-invocations`), which is not
yet declared in `service-configuration.yaml`'s `spec.metrics`/`billing`
— those events are emitted but not currently billed. Worth a follow-up
if tool-invocation billing is wanted; adding it is additive/safe per
the immutability rules above.

## Deployment

This bundle is **not** wired into assistant's own Deployment
(`config/base/`). The catalog identifiers are control-plane resources;
deployment into the services-operator's namespace happens via Flux
from the infra repo
(`apps/patch-assistant/base/milo-services.yaml`), the same way
`apps/cloud-portal/base/milo-services.yaml` used to.

To preview the bundle locally:

```sh
kustomize build config/components/service-catalog
```
