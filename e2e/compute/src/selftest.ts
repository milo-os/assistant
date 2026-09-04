// Selftest for the compute provider's diagnosis engine.
//
// Asserts the behaviour that actually matters: that the walk follows compute's
// pointer reasons down to the leaf cause instead of reporting the pointer, and
// that each fixture lands in the right actionability bucket.
//
// Run: node src/selftest.ts

import { diagnoseWorkload, fleetSummary } from './diagnose.ts';
import { explainReason } from './catalog.ts';

let failures = 0;

function check(label: string, actual: unknown, expected: unknown): void {
  const ok = actual === expected;
  if (!ok) failures++;
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${label}${ok ? '' : ` — got ${JSON.stringify(actual)}, want ${JSON.stringify(expected)}`}`);
}

// --- the pointer-following property, per failure class -------------------

const quota = diagnoseWorkload('api-backend')!;
check('api-backend root cause is the leaf quota reason', quota.rootCause?.reason, 'QuotaExceeded');
check('api-backend root cause is on the instance', quota.rootCause?.level, 'instance');
check('api-backend is user-actionable', quota.rootCause?.actionability, 'user');
check('api-backend is partially serving', quota.instances.ready, 2);
check('api-backend has 4 blocked instances', quota.instances.blocked.length, 4);
check('api-backend suggests quota-triage', quota.suggestedSkill, 'quota-triage');

const image = diagnoseWorkload('batch-processor')!;
check('batch-processor root cause', image.rootCause?.reason, 'ImageUnavailable');
check('batch-processor is user-actionable', image.rootCause?.actionability, 'user');
check('batch-processor has no ready instances', image.instances.ready, 0);

const placement = diagnoseWorkload('edge-cache')!;
check('edge-cache root cause', placement.rootCause?.reason, 'NoMatchingLocation');
check('edge-cache is a platform fault', placement.rootCause?.actionability, 'platform');
check('edge-cache root cause is on the deployment', placement.rootCause?.level, 'deployment');

const refdata = diagnoseWorkload('config-consumer')!;
check('config-consumer root cause', refdata.rootCause?.reason, 'SourceNotFound');
check('config-consumer is user-actionable', refdata.rootCause?.actionability, 'user');
check('config-consumer suggests referenced-data-triage', refdata.suggestedSkill, 'referenced-data-triage');

const crash = diagnoseWorkload('telemetry-agent')!;
check('telemetry-agent root cause', crash.rootCause?.reason, 'InstanceCrashing');
check('telemetry-agent is user-actionable', crash.rootCause?.actionability, 'user');

const healthy = diagnoseWorkload('web-frontend')!;
check('web-frontend is available', healthy.available, true);
check('web-frontend has no root cause', healthy.rootCause, null);
check('web-frontend has 3 ready instances', healthy.instances.ready, 3);

// --- no pointer reason is ever reported as the root cause ----------------

const POINTERS = new Set([
  'NoAvailablePlacements',
  'NoAvailableDeployments',
  'InstancesProvisioning',
  'QuotaNotGranted',
  'ReferencedDataNotReady',
  'PendingQuota',
  'SchedulingGatesPresent',
]);
for (const row of fleetSummary()) {
  if (row.rootCauseReason === null) continue;
  check(
    `${row.workload}: root cause is not a pointer reason`,
    POINTERS.has(row.rootCauseReason),
    false,
  );
}

// --- fleet view ordering and catalog coverage ----------------------------

const fleet = fleetSummary();
check('fleet lists every workload', fleet.length, 6);
check('fleet is worst-first', fleet[0]?.available, false);
check('fleet puts the healthy workload last', fleet[fleet.length - 1]?.workload, 'web-frontend');

// Every reason the fixtures produce must be explainable.
for (const row of fleet) {
  if (!row.rootCauseReason) continue;
  check(`catalog explains ${row.rootCauseReason}`, Boolean(explainReason(row.rootCauseReason)), true);
}

check('unknown reason returns undefined', explainReason('NotARealReason'), undefined);

console.log(failures === 0 ? '\nall checks passed' : `\n${failures} check(s) FAILED`);
process.exit(failures === 0 ? 0 : 1);
