// Workload diagnosis: turn a Workload/WorkloadDeployment/Instance condition
// tree into one root cause plus a next step.
//
// The engine exists because compute's top-level reasons are deliberately
// *pointers*, not causes. `Workload.Available=False/QuotaNotGranted` tells you
// which subsystem is blocking, and the real reason lives on an Instance's
// `QuotaGranted` condition below it. A human reads down the tree by hand; this
// walks it, skips the pointer reasons, and reports the deepest condition that
// actually names a cause.
//
// PORTABILITY: pure logic over condition data. Together with catalog.ts this
// is what moves into the compute repo as Go, reading real objects instead of
// the fixture.

import { explainReason, type Actionability } from './catalog.ts';
import {
  deploymentsFor,
  getWorkload,
  instancesFor,
  workloadNames,
  type Condition,
  type Workload,
} from './data.ts';

/**
 * Reasons that redirect to a deeper condition rather than naming a cause.
 * The walk descends past these to find the condition they point at.
 */
const POINTER_REASONS = new Set([
  'NoAvailablePlacements',
  'NoAvailableDeployments',
  'InstancesProvisioning',
  'QuotaNotGranted',
  'ReferencedDataNotReady',
  'PendingQuota',
  'SchedulingGatesPresent',
]);

/** Depth ordering — a cause found deeper in the tree beats a shallower one. */
const LEVEL_DEPTH = { workload: 0, deployment: 1, instance: 2 } as const;
export type Level = keyof typeof LEVEL_DEPTH;

export interface CauseRef {
  level: Level;
  object: string;
  conditionType: string;
  status: string;
  reason: string;
  message: string;
  lastTransitionTime: string;
  actionability: Actionability | 'unknown';
  explanation: string;
  remediation: string;
  skill?: string;
}

export interface Diagnosis {
  workload: string;
  namespace: string;
  available: boolean;
  summary: string;
  rootCause: CauseRef | null;
  /** Every failing condition found, deepest first — the evidence trail. */
  contributingConditions: CauseRef[];
  instances: {
    total: number;
    ready: number;
    blocked: { name: string; reason: string; message: string }[];
  };
  nextSteps: string[];
  /** Skill to load for the full procedure, when one covers this cause. */
  suggestedSkill?: string;
}

function toCause(level: Level, object: string, c: Condition): CauseRef {
  const info = explainReason(c.reason);
  return {
    level,
    object,
    conditionType: c.type,
    status: c.status,
    reason: c.reason,
    message: c.message,
    lastTransitionTime: c.lastTransitionTime,
    actionability: info?.actionability ?? 'unknown',
    explanation:
      info?.explanation ??
      `No catalog entry for reason "${c.reason}". Treat the condition message as the cause.`,
    remediation: info?.remediation ?? '',
    skill: info?.skill,
  };
}

/** A condition is failing when a positive-polarity condition is not True. */
function failing(conditions: Condition[]): Condition[] {
  return conditions.filter((c) => c.status !== 'True');
}

export function diagnoseWorkload(name: string): Diagnosis | undefined {
  const workload = getWorkload(name);
  if (!workload) return undefined;

  const causes: CauseRef[] = [];

  for (const c of failing(workload.conditions)) {
    causes.push(toCause('workload', workload.name, c));
  }
  for (const d of deploymentsFor(name)) {
    for (const c of failing(d.conditions)) {
      causes.push(toCause('deployment', d.name, c));
    }
  }

  const allInstances = instancesFor(name);
  for (const i of allInstances) {
    for (const c of failing(i.conditions)) {
      causes.push(toCause('instance', i.name, c));
    }
  }

  // Deepest-first, and within a level prefer a condition that names a real
  // cause over one that only points at another condition.
  const ranked = [...causes].sort((a, b) => {
    const pointerA = POINTER_REASONS.has(a.reason) ? 1 : 0;
    const pointerB = POINTER_REASONS.has(b.reason) ? 1 : 0;
    if (pointerA !== pointerB) return pointerA - pointerB;
    return LEVEL_DEPTH[b.level] - LEVEL_DEPTH[a.level];
  });

  const rootCause = ranked[0] ?? null;

  const readyInstances = allInstances.filter((i) =>
    i.conditions.some((c) => c.type === 'Ready' && c.status === 'True'),
  );
  const blocked = allInstances
    .filter((i) => !i.conditions.some((c) => c.type === 'Ready' && c.status === 'True'))
    .map((i) => {
      // Report the instance's own most specific failing condition.
      const specific =
        failing(i.conditions).find((c) => !POINTER_REASONS.has(c.reason)) ??
        failing(i.conditions)[0];
      return {
        name: i.name,
        reason: specific?.reason ?? 'Unknown',
        message: specific?.message ?? '',
      };
    });

  const available = workload.conditions.some(
    (c) => c.type === 'Available' && c.status === 'True',
  );

  return {
    workload: workload.name,
    namespace: workload.namespace,
    available,
    summary: summarize(workload, available, rootCause, blocked.length),
    rootCause,
    contributingConditions: ranked,
    instances: {
      total: allInstances.length,
      ready: readyInstances.length,
      blocked,
    },
    nextSteps: nextSteps(rootCause),
    suggestedSkill: rootCause?.skill,
  };
}

function summarize(
  w: Workload,
  available: boolean,
  root: CauseRef | null,
  blockedCount: number,
): string {
  if (available && blockedCount === 0) {
    return `Workload ${w.name} is available: ${w.readyReplicas}/${w.desiredReplicas} replicas ready.`;
  }
  if (!root) {
    return `Workload ${w.name} reports no failing conditions but only ${w.readyReplicas}/${w.desiredReplicas} replicas are ready.`;
  }
  const who =
    root.actionability === 'user'
      ? 'This is user-actionable'
      : root.actionability === 'platform'
        ? 'This is a platform fault — escalate to Datum'
        : 'This is a transient state';
  return (
    `Workload ${w.name} is ${available ? 'partially available' : 'not available'} ` +
    `(${w.readyReplicas}/${w.desiredReplicas} replicas ready). ` +
    `Root cause: ${root.reason} on ${root.level} ${root.object} (${root.conditionType}). ${who}.`
  );
}

function nextSteps(root: CauseRef | null): string[] {
  if (!root) return [];
  const steps: string[] = [];
  if (root.remediation) steps.push(root.remediation);
  if (root.actionability === 'platform') {
    steps.push(
      'Do not change the workload spec — this cause is not fixable from the customer side.',
    );
  }
  if (root.skill) {
    steps.push(`Load the "${root.skill}" skill for the full procedure.`);
  }
  return steps;
}

/** Availability summary across every workload, worst first. */
export function fleetSummary() {
  const rows = [];
  for (const w of workloadNames()) {
    const d = diagnoseWorkload(w);
    if (!d) continue;
    rows.push({
      workload: d.workload,
      available: d.available,
      readyReplicas: `${d.instances.ready}/${d.instances.total}`,
      rootCauseReason: d.rootCause?.reason ?? null,
      actionability: d.rootCause?.actionability ?? null,
    });
  }
  return rows.sort((a, b) => Number(a.available) - Number(b.available));
}

