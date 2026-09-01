// The compute condition-reason catalog.
//
// Datum Compute's API already carries the diagnostic vocabulary: every
// blocking cause is a stable, machine-readable `reason` on a condition, and
// api/v1alpha's doc comments say, for each one, whether the user must act,
// whether the platform is at fault, or whether it is transient. This file is
// that knowledge lifted into a lookup table so the assistant can turn a raw
// condition into an explanation and a next step.
//
// Source of truth: datum-cloud/compute api/v1alpha/{instance,workload,
// workloaddeployment}_types.go. When compute adds a reason, add it here.
//
// PORTABILITY: this table (and diagnose.ts) is the part of this prototype that
// is meant to move into the compute repo as Go. It deliberately depends on
// nothing but the reason string.

/**
 * Who has to do something about it.
 *
 * - `user`     — the workload spec or project config is wrong; the customer fixes it.
 * - `platform` — Datum's fault; the customer cannot fix it, escalate to Datum.
 * - `transient`— normal in-progress state; the right action is to wait.
 */
export type Actionability = 'user' | 'platform' | 'transient';

export interface ReasonInfo {
  /** The condition reason as written by the controllers. */
  reason: string;
  /** Condition types this reason is observed on. */
  conditionTypes: string[];
  actionability: Actionability;
  /** Plain-language explanation of what actually happened. */
  explanation: string;
  /** What to do next. Empty for terminal-healthy reasons. */
  remediation: string;
  /** Skill (runbook) that covers this class of failure, if any. */
  skill?: string;
}

const CATALOG: ReasonInfo[] = [
  // ---------------------------------------------------------------- healthy
  {
    reason: 'Available',
    conditionTypes: ['Ready', 'Available'],
    actionability: 'transient',
    explanation: 'The instance is serving.',
    remediation: '',
  },
  {
    reason: 'StableInstanceFound',
    conditionTypes: ['Available'],
    actionability: 'transient',
    explanation: 'The deployment has at least one ready instance and is serving.',
    remediation: '',
  },
  {
    reason: 'Programmed',
    conditionTypes: ['Programmed'],
    actionability: 'transient',
    explanation: 'The infrastructure provider has fully programmed the instance.',
    remediation: '',
  },
  {
    reason: 'QuotaAvailable',
    conditionTypes: ['QuotaGranted'],
    actionability: 'transient',
    explanation: 'Quota was evaluated and granted for this instance.',
    remediation: '',
  },
  {
    reason: 'Ready',
    conditionTypes: ['ReferencedDataReady'],
    actionability: 'transient',
    explanation: 'All referenced ConfigMaps and Secrets are resolved and present on the cell.',
    remediation: '',
  },

  // ------------------------------------------------------------------ quota
  {
    reason: 'QuotaExceeded',
    conditionTypes: ['QuotaGranted'],
    actionability: 'user',
    explanation:
      'The project asked for more compute than its allowance permits, so the quota backend explicitly denied the claim. Nothing will proceed until headroom exists.',
    remediation:
      'Reduce the workload\'s replica count or instance size, release capacity by deleting unused workloads, or request a quota increase for the project.',
    skill: 'quota-triage',
  },
  {
    reason: 'QuotaNoBudget',
    conditionTypes: ['QuotaGranted'],
    actionability: 'platform',
    explanation:
      'The quota claim was created and is pending because no AllowanceBucket has been configured for this project at all. This is distinct from QuotaExceeded (explicitly denied) and PendingEvaluation (evaluation in flight) — the project has no budget to spend against.',
    remediation:
      'The project needs an AllowanceBucket provisioned. Escalate to Datum — the customer cannot configure this.',
    skill: 'quota-triage',
  },
  {
    reason: 'PendingEvaluation',
    conditionTypes: ['QuotaGranted'],
    actionability: 'transient',
    explanation:
      'The quota claim has not been created yet, or its first evaluation is still in flight.',
    remediation: 'Wait. If it persists for more than a few minutes, treat it as a quota-backend problem.',
    skill: 'quota-triage',
  },
  {
    reason: 'QuotaBackendUnavailable',
    conditionTypes: ['QuotaGranted'],
    actionability: 'platform',
    explanation:
      'Quota enforcement is configured but the Milo quota backend could not be reached — network error, TLS failure, or a 401/503 from the backend.',
    remediation: 'Escalate to Datum. Nothing in the customer\'s workload spec affects this.',
    skill: 'quota-triage',
  },
  {
    reason: 'QuotaProjectNotFound',
    conditionTypes: ['QuotaGranted'],
    actionability: 'platform',
    explanation:
      'The Milo project referenced by this instance does not exist — the project control plane returned 404.',
    remediation: 'Escalate to Datum; the project registration is missing or was removed.',
    skill: 'quota-triage',
  },
  {
    reason: 'QuotaNamespaceNotFound',
    conditionTypes: ['QuotaGranted'],
    actionability: 'platform',
    explanation: 'The claim namespace does not exist on the Milo project control plane.',
    remediation: 'Escalate to Datum.',
    skill: 'quota-triage',
  },
  {
    reason: 'QuotaMisconfigured',
    conditionTypes: ['QuotaGranted'],
    actionability: 'platform',
    explanation:
      'The Milo admission plugin rejected the ResourceClaim (403/422): the ResourceRegistration is absent, or the claimingRules do not match.',
    remediation: 'Escalate to Datum — this is a platform quota configuration error.',
    skill: 'quota-triage',
  },
  {
    reason: 'QuotaProjectIDUnresolvable',
    conditionTypes: ['QuotaGranted'],
    actionability: 'platform',
    explanation:
      'The namespace label required to derive the Milo project ID is missing or unreadable, so the claim cannot be attributed to a project.',
    remediation: 'Escalate to Datum.',
    skill: 'quota-triage',
  },
  {
    reason: 'QuotaDisabled',
    conditionTypes: ['QuotaGranted'],
    actionability: 'transient',
    explanation:
      'Quota enforcement is intentionally switched off in this environment because no credential path was configured. Not a failure.',
    remediation: '',
  },
  {
    reason: 'ValidationFailed',
    conditionTypes: ['QuotaGranted'],
    actionability: 'user',
    explanation: 'The quota claim failed validation before it could be evaluated.',
    remediation: 'Check the instance resource requests for invalid or unsupported values.',
    skill: 'quota-triage',
  },
  {
    reason: 'PendingQuota',
    conditionTypes: ['Programmed'],
    actionability: 'transient',
    explanation:
      'Programming is deliberately held back until quota is granted. The real cause is on the instance\'s QuotaGranted condition — read that one.',
    remediation: 'Diagnose the QuotaGranted condition; this reason only points at it.',
    skill: 'quota-triage',
  },

  // ------------------------------------------------------- instance runtime
  {
    reason: 'ImageUnavailable',
    conditionTypes: ['Ready', 'Programmed'],
    actionability: 'user',
    explanation:
      'The provider could not pull the instance image: a bad image name, missing registry credentials, or an unreachable registry.',
    remediation:
      'Verify the image reference in the workload spec (tag exists, registry path correct) and that pull credentials for a private registry are configured.',
    skill: 'instance-not-ready',
  },
  {
    reason: 'InstanceCrashing',
    conditionTypes: ['Ready', 'Programmed'],
    actionability: 'user',
    explanation:
      'The process started but keeps exiting and being restarted (CrashLoopBackOff in the underlying runtime). The application itself is failing — the platform delivered it correctly.',
    remediation:
      'Read the instance logs for the exit cause: a failing entrypoint, a missing env var or mount, or an unmet dependency at startup.',
    skill: 'instance-not-ready',
  },
  {
    reason: 'ConfigurationError',
    conditionTypes: ['Ready', 'Programmed'],
    actionability: 'user',
    explanation:
      'The runtime rejected the instance configuration before the process could start — for example an invalid environment-variable injection or a missing device.',
    remediation: 'Correct the workload spec; the runtime refused it as written.',
    skill: 'instance-not-ready',
  },
  {
    reason: 'Provisioning',
    conditionTypes: ['Ready'],
    actionability: 'transient',
    explanation:
      'The runtime is still setting up the execution environment — creating the container, unpacking the image.',
    remediation: 'Wait; this is normal and non-actionable.',
  },
  {
    reason: 'SchedulingGatesPresent',
    conditionTypes: ['Ready'],
    actionability: 'transient',
    explanation:
      'Scheduling gates are still attached to the instance, so it is intentionally held before scheduling.',
    remediation: 'Wait for the gates to clear; if they never do, the gating controller is stuck — escalate.',
  },
  {
    reason: 'PendingProgramming',
    conditionTypes: ['Programmed'],
    actionability: 'transient',
    explanation: 'The infrastructure provider has not started programming the instance yet.',
    remediation: 'Wait. If it persists, the provider controller may not be running.',
  },
  {
    reason: 'ProgrammingInProgress',
    conditionTypes: ['Programmed'],
    actionability: 'transient',
    explanation: 'The infrastructure provider is actively programming the instance.',
    remediation: 'Wait.',
  },
  {
    reason: 'Starting',
    conditionTypes: ['Available'],
    actionability: 'transient',
    explanation: 'The instance is starting up.',
    remediation: 'Wait.',
  },
  {
    reason: 'Stopping',
    conditionTypes: ['Available'],
    actionability: 'transient',
    explanation: 'The instance is shutting down.',
    remediation: 'Wait.',
  },
  {
    reason: 'Stopped',
    conditionTypes: ['Available'],
    actionability: 'user',
    explanation: 'The instance is stopped and is not serving.',
    remediation: 'Start the instance, or check whether it was stopped deliberately.',
  },
  {
    reason: 'Suspended',
    conditionTypes: ['Ready', 'Available'],
    actionability: 'platform',
    explanation:
      'The instance was intentionally stopped because the project is suspended. Its placement, disk, and quota allocation are retained and the process restarts from disk on reinstatement.',
    remediation:
      'Resolve the project suspension (usually billing or an account hold). No workload change will help.',
  },

  // ------------------------------------------------------- referenced data
  {
    reason: 'SourceNotFound',
    conditionTypes: ['ReferencedDataReady'],
    actionability: 'user',
    explanation:
      'One or more ConfigMaps or Secrets referenced by the workload template do not exist in the project namespace.',
    remediation:
      'Create the missing ConfigMap/Secret in the project namespace, or correct the reference in the workload spec.',
    skill: 'referenced-data-triage',
  },
  {
    reason: 'SourceUnauthorized',
    conditionTypes: ['ReferencedDataReady'],
    actionability: 'platform',
    explanation:
      'The management identity does not have permission to read one or more referenced ConfigMaps or Secrets.',
    remediation: 'Escalate to Datum — the platform\'s RBAC for referenced data is insufficient.',
    skill: 'referenced-data-triage',
  },
  {
    reason: 'SourceTooLarge',
    conditionTypes: ['ReferencedDataReady'],
    actionability: 'user',
    explanation: 'One or more referenced ConfigMaps or Secrets exceed the allowed size limit.',
    remediation: 'Shrink the referenced object, or split it into smaller ones.',
    skill: 'referenced-data-triage',
  },
  {
    reason: 'Resolving',
    conditionTypes: ['ReferencedDataReady'],
    actionability: 'transient',
    explanation:
      'The resolver is reading the source ConfigMaps/Secrets from the project control plane.',
    remediation: 'Wait.',
  },
  {
    reason: 'AwaitingPropagation',
    conditionTypes: ['ReferencedDataReady'],
    actionability: 'transient',
    explanation: 'The resolved data has not yet fully arrived on the cell.',
    remediation: 'Wait.',
  },

  // --------------------------------------------- placement / deployment
  {
    reason: 'NoMatchingLocation',
    conditionTypes: ['Available'],
    actionability: 'platform',
    explanation:
      'The cell has not been told which location it serves, so the deployment cannot be assigned one.',
    remediation: 'Escalate to Datum — cell location configuration is missing.',
    skill: 'placement-triage',
  },
  {
    reason: 'AmbiguousServingLocation',
    conditionTypes: ['Available'],
    actionability: 'platform',
    explanation:
      'More than one location was delivered to the cell. The cell will not guess which it serves, so the deployment waits for the platform to resolve the conflict.',
    remediation: 'Escalate to Datum.',
    skill: 'placement-triage',
  },
  {
    reason: 'CityCodeMismatch',
    conditionTypes: ['Available'],
    actionability: 'platform',
    explanation:
      'The deployment asked for one city and the cell serves another — it was placed on the wrong cell.',
    remediation: 'Escalate to Datum; this is a placement fault, not a spec error.',
    skill: 'placement-triage',
  },
  {
    reason: 'NetworkProvisioning',
    conditionTypes: ['Available'],
    actionability: 'transient',
    explanation: 'The network binding or subnet is still being provisioned.',
    remediation: 'Wait.',
  },
  {
    reason: 'InstancesProvisioning',
    conditionTypes: ['Available'],
    actionability: 'transient',
    explanation: 'Instances exist but none are ready yet.',
    remediation: 'Wait, then diagnose the individual instances if it does not settle.',
  },
  {
    reason: 'ReferencedDataNotReady',
    conditionTypes: ['Available'],
    actionability: 'user',
    explanation:
      'The worst-blocking sub-condition is a ReferencedData failure. The message carries that sub-condition verbatim — read the ReferencedDataReady condition for the real cause.',
    remediation: 'Diagnose the ReferencedDataReady condition on the deployment or instance.',
    skill: 'referenced-data-triage',
  },
  {
    reason: 'QuotaNotGranted',
    conditionTypes: ['Available'],
    actionability: 'user',
    explanation:
      'Quota is blocking one or more instances. The real cause is the instance\'s QuotaGranted condition.',
    remediation: 'Diagnose the QuotaGranted condition on the blocked instances.',
    skill: 'quota-triage',
  },
  {
    reason: 'NetworkNotFound',
    conditionTypes: ['Available'],
    actionability: 'user',
    explanation:
      'One or more networks referenced by the workload\'s network interfaces do not exist.',
    remediation:
      'Create the referenced Network, or correct the network name in the workload\'s network interfaces.',
  },
  {
    reason: 'NoAvailablePlacements',
    conditionTypes: ['Available'],
    actionability: 'user',
    explanation:
      'Every placement reports no available deployments. This is the last-resort default reason — the specific cause is on the placements below.',
    remediation: 'Diagnose the individual placements and their deployments.',
    skill: 'workload-not-available',
  },
  {
    reason: 'NoAvailableDeployments',
    conditionTypes: ['Available'],
    actionability: 'user',
    explanation: 'No deployment in this placement is available.',
    remediation: 'Diagnose the deployments in this placement.',
    skill: 'workload-not-available',
  },
];

const BY_REASON = new Map(CATALOG.map((r) => [r.reason, r]));

export function explainReason(reason: string): ReasonInfo | undefined {
  return BY_REASON.get(reason);
}

export function allReasons(): ReasonInfo[] {
  return CATALOG;
}
