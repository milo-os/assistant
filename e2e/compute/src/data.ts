// Canned Datum Compute resources for the prototype.
//
// These mirror the real shape of compute.datumapis.com/v1alpha objects
// (Workload -> WorkloadDeployment -> Instance, each with metav1.Condition
// status) closely enough that the diagnosis engine written against them ports
// unchanged to real client-go reads. Only this file is throwaway; catalog.ts
// and diagnose.ts are the artifacts meant to land in the compute repo.
//
// The fixture deliberately spans the actionability classes: a healthy
// workload, three user-actionable failures (quota, image, referenced data),
// one platform fault (placement), and one crash loop.

export interface Condition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason: string;
  message: string;
  lastTransitionTime: string;
  observedGeneration?: number;
}

export interface Instance {
  name: string;
  namespace: string;
  deployment: string;
  workload: string;
  location: string;
  creationTimestamp: string;
  conditions: Condition[];
}

export interface WorkloadDeployment {
  name: string;
  namespace: string;
  workload: string;
  placement: string;
  location: string;
  cityCode: string;
  creationTimestamp: string;
  conditions: Condition[];
}

export interface PlacementStatus {
  name: string;
  replicas: number;
  readyReplicas: number;
  conditions: Condition[];
}

export interface Workload {
  name: string;
  namespace: string;
  creationTimestamp: string;
  image: string;
  desiredReplicas: number;
  replicas: number;
  readyReplicas: number;
  placements: PlacementStatus[];
  conditions: Condition[];
}

const T = (iso: string) => iso;

// --------------------------------------------------------------- workloads

export const WORKLOADS: Workload[] = [
  {
    name: 'web-frontend',
    namespace: 'demo-project',
    creationTimestamp: T('2026-08-20T09:12:04Z'),
    image: 'ghcr.io/demo/web-frontend:1.8.2',
    desiredReplicas: 3,
    replicas: 3,
    readyReplicas: 3,
    placements: [
      {
        name: 'us-central',
        replicas: 3,
        readyReplicas: 3,
        conditions: [
          {
            type: 'Available',
            status: 'True',
            reason: 'StableInstanceFound',
            message: 'Placement has 3 ready instances.',
            lastTransitionTime: T('2026-08-20T09:14:31Z'),
          },
        ],
      },
    ],
    conditions: [
      {
        type: 'Available',
        status: 'True',
        reason: 'StableInstanceFound',
        message: 'At least one instance is ready and serving.',
        lastTransitionTime: T('2026-08-20T09:14:31Z'),
        observedGeneration: 4,
      },
    ],
  },

  {
    name: 'api-backend',
    namespace: 'demo-project',
    creationTimestamp: T('2026-08-29T16:40:11Z'),
    image: 'ghcr.io/demo/api-backend:3.1.0',
    desiredReplicas: 6,
    replicas: 6,
    readyReplicas: 2,
    placements: [
      {
        name: 'us-central',
        replicas: 6,
        readyReplicas: 2,
        conditions: [
          {
            type: 'Available',
            status: 'False',
            reason: 'NoAvailableDeployments',
            message: 'No deployment in this placement is available.',
            lastTransitionTime: T('2026-08-29T16:42:02Z'),
          },
        ],
      },
    ],
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'QuotaNotGranted',
        message: 'Quota is blocking 4 of 6 instances in placement us-central.',
        lastTransitionTime: T('2026-08-29T16:42:02Z'),
        observedGeneration: 2,
      },
    ],
  },

  {
    name: 'batch-processor',
    namespace: 'demo-project',
    creationTimestamp: T('2026-08-31T21:03:55Z'),
    image: 'ghcr.io/demo/batch-processor:2026.08.31',
    desiredReplicas: 2,
    replicas: 2,
    readyReplicas: 0,
    placements: [
      {
        name: 'eu-west',
        replicas: 2,
        readyReplicas: 0,
        conditions: [
          {
            type: 'Available',
            status: 'False',
            reason: 'NoAvailableDeployments',
            message: 'No deployment in this placement is available.',
            lastTransitionTime: T('2026-08-31T21:05:12Z'),
          },
        ],
      },
    ],
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'NoAvailablePlacements',
        message: 'All placements report no available deployments.',
        lastTransitionTime: T('2026-08-31T21:05:12Z'),
        observedGeneration: 1,
      },
    ],
  },

  {
    name: 'edge-cache',
    namespace: 'demo-project',
    creationTimestamp: T('2026-08-30T11:22:09Z'),
    image: 'ghcr.io/demo/edge-cache:0.9.4',
    desiredReplicas: 4,
    replicas: 0,
    readyReplicas: 0,
    placements: [
      {
        name: 'ams-edge',
        replicas: 0,
        readyReplicas: 0,
        conditions: [
          {
            type: 'Available',
            status: 'False',
            reason: 'NoAvailableDeployments',
            message: 'No deployment in this placement is available.',
            lastTransitionTime: T('2026-08-30T11:23:40Z'),
          },
        ],
      },
    ],
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'NoAvailablePlacements',
        message: 'All placements report no available deployments.',
        lastTransitionTime: T('2026-08-30T11:23:40Z'),
        observedGeneration: 1,
      },
    ],
  },

  {
    name: 'config-consumer',
    namespace: 'demo-project',
    creationTimestamp: T('2026-09-01T08:15:00Z'),
    image: 'ghcr.io/demo/config-consumer:1.0.1',
    desiredReplicas: 1,
    replicas: 1,
    readyReplicas: 0,
    placements: [
      {
        name: 'us-central',
        replicas: 1,
        readyReplicas: 0,
        conditions: [
          {
            type: 'Available',
            status: 'False',
            reason: 'NoAvailableDeployments',
            message: 'No deployment in this placement is available.',
            lastTransitionTime: T('2026-09-01T08:16:20Z'),
          },
        ],
      },
    ],
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'ReferencedDataNotReady',
        message: 'ConfigMap "app-config" not found in namespace "demo-project".',
        lastTransitionTime: T('2026-09-01T08:16:20Z'),
        observedGeneration: 1,
      },
    ],
  },

  {
    name: 'telemetry-agent',
    namespace: 'demo-project',
    creationTimestamp: T('2026-08-28T13:44:18Z'),
    image: 'ghcr.io/demo/telemetry-agent:4.2.0',
    desiredReplicas: 2,
    replicas: 2,
    readyReplicas: 0,
    placements: [
      {
        name: 'us-central',
        replicas: 2,
        readyReplicas: 0,
        conditions: [
          {
            type: 'Available',
            status: 'False',
            reason: 'NoAvailableDeployments',
            message: 'No deployment in this placement is available.',
            lastTransitionTime: T('2026-08-28T13:47:55Z'),
          },
        ],
      },
    ],
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'NoAvailablePlacements',
        message: 'All placements report no available deployments.',
        lastTransitionTime: T('2026-08-28T13:47:55Z'),
        observedGeneration: 7,
      },
    ],
  },
];

// ------------------------------------------------------------- deployments

export const DEPLOYMENTS: WorkloadDeployment[] = [
  {
    name: 'web-frontend-us-central-a',
    namespace: 'demo-project',
    workload: 'web-frontend',
    placement: 'us-central',
    location: 'us-central-1a',
    cityCode: 'DFW',
    creationTimestamp: T('2026-08-20T09:12:10Z'),
    conditions: [
      {
        type: 'Available',
        status: 'True',
        reason: 'StableInstanceFound',
        message: '3 ready instances.',
        lastTransitionTime: T('2026-08-20T09:14:28Z'),
      },
      {
        type: 'ReferencedDataReady',
        status: 'True',
        reason: 'Ready',
        message: 'All referenced data present on the cell.',
        lastTransitionTime: T('2026-08-20T09:12:44Z'),
      },
    ],
  },
  {
    name: 'api-backend-us-central-a',
    namespace: 'demo-project',
    workload: 'api-backend',
    placement: 'us-central',
    location: 'us-central-1a',
    cityCode: 'DFW',
    creationTimestamp: T('2026-08-29T16:40:18Z'),
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'QuotaNotGranted',
        message: 'Quota is blocking 4 instances.',
        lastTransitionTime: T('2026-08-29T16:41:59Z'),
      },
      {
        type: 'ReferencedDataReady',
        status: 'True',
        reason: 'Ready',
        message: 'All referenced data present on the cell.',
        lastTransitionTime: T('2026-08-29T16:40:51Z'),
      },
    ],
  },
  {
    name: 'batch-processor-eu-west-b',
    namespace: 'demo-project',
    workload: 'batch-processor',
    placement: 'eu-west',
    location: 'eu-west-2b',
    cityCode: 'LHR',
    creationTimestamp: T('2026-08-31T21:04:02Z'),
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'InstancesProvisioning',
        message: 'Instances exist but none are ready.',
        lastTransitionTime: T('2026-08-31T21:05:09Z'),
      },
      {
        type: 'ReferencedDataReady',
        status: 'True',
        reason: 'Ready',
        message: 'All referenced data present on the cell.',
        lastTransitionTime: T('2026-08-31T21:04:30Z'),
      },
    ],
  },
  {
    name: 'edge-cache-ams-edge-a',
    namespace: 'demo-project',
    workload: 'edge-cache',
    placement: 'ams-edge',
    location: '',
    cityCode: 'AMS',
    creationTimestamp: T('2026-08-30T11:22:15Z'),
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'NoMatchingLocation',
        message:
          'The cell serving this deployment has not been told which location it serves; no location could be assigned.',
        lastTransitionTime: T('2026-08-30T11:23:37Z'),
      },
    ],
  },
  {
    name: 'config-consumer-us-central-a',
    namespace: 'demo-project',
    workload: 'config-consumer',
    placement: 'us-central',
    location: 'us-central-1a',
    cityCode: 'DFW',
    creationTimestamp: T('2026-09-01T08:15:06Z'),
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'ReferencedDataNotReady',
        message: 'ConfigMap "app-config" not found in namespace "demo-project".',
        lastTransitionTime: T('2026-09-01T08:16:18Z'),
      },
      {
        type: 'ReferencedDataReady',
        status: 'False',
        reason: 'SourceNotFound',
        message:
          'ConfigMap "app-config" not found in namespace "demo-project"; referenced by container "app" as a volume mount.',
        lastTransitionTime: T('2026-09-01T08:16:18Z'),
      },
    ],
  },
  {
    name: 'telemetry-agent-us-central-a',
    namespace: 'demo-project',
    workload: 'telemetry-agent',
    placement: 'us-central',
    location: 'us-central-1a',
    cityCode: 'DFW',
    creationTimestamp: T('2026-08-28T13:44:25Z'),
    conditions: [
      {
        type: 'Available',
        status: 'False',
        reason: 'InstancesProvisioning',
        message: 'Instances exist but none are ready.',
        lastTransitionTime: T('2026-08-28T13:47:51Z'),
      },
      {
        type: 'ReferencedDataReady',
        status: 'True',
        reason: 'Ready',
        message: 'All referenced data present on the cell.',
        lastTransitionTime: T('2026-08-28T13:44:58Z'),
      },
    ],
  },
];

// --------------------------------------------------------------- instances

const ready = (t: string): Condition[] => [
  {
    type: 'Ready',
    status: 'True',
    reason: 'Available',
    message: 'Instance is serving.',
    lastTransitionTime: t,
  },
  {
    type: 'Available',
    status: 'True',
    reason: 'Available',
    message: 'Instance is available.',
    lastTransitionTime: t,
  },
  {
    type: 'Programmed',
    status: 'True',
    reason: 'Programmed',
    message: 'Instance programmed by the infrastructure provider.',
    lastTransitionTime: t,
  },
  {
    type: 'QuotaGranted',
    status: 'True',
    reason: 'QuotaAvailable',
    message: 'Quota granted.',
    lastTransitionTime: t,
  },
  {
    type: 'ReferencedDataReady',
    status: 'True',
    reason: 'Ready',
    message: 'All referenced data present.',
    lastTransitionTime: t,
  },
];

export const INSTANCES: Instance[] = [
  // web-frontend — all healthy
  ...[0, 1, 2].map((i) => ({
    name: `web-frontend-us-central-a-${['7fx2', 'k91m', 'p4qd'][i]}`,
    namespace: 'demo-project',
    deployment: 'web-frontend-us-central-a',
    workload: 'web-frontend',
    location: 'us-central-1a',
    creationTimestamp: T('2026-08-20T09:12:20Z'),
    conditions: ready(T('2026-08-20T09:14:22Z')),
  })),

  // api-backend — 2 healthy, 4 blocked on quota
  ...[0, 1].map((i) => ({
    name: `api-backend-us-central-a-${['a1b2', 'c3d4'][i]}`,
    namespace: 'demo-project',
    deployment: 'api-backend-us-central-a',
    workload: 'api-backend',
    location: 'us-central-1a',
    creationTimestamp: T('2026-08-29T16:40:25Z'),
    conditions: ready(T('2026-08-29T16:41:44Z')),
  })),
  ...[0, 1, 2, 3].map((i) => ({
    name: `api-backend-us-central-a-${['e5f6', 'g7h8', 'j9k0', 'm1n2'][i]}`,
    namespace: 'demo-project',
    deployment: 'api-backend-us-central-a',
    workload: 'api-backend',
    location: 'us-central-1a',
    creationTimestamp: T('2026-08-29T16:40:25Z'),
    conditions: [
      {
        type: 'Ready',
        status: 'False',
        reason: 'SchedulingGatesPresent',
        message: 'Instance is gated pending quota.',
        lastTransitionTime: T('2026-08-29T16:41:50Z'),
      },
      {
        type: 'QuotaGranted',
        status: 'False',
        reason: 'QuotaExceeded',
        message:
          'Requested 4 vCPU / 16Gi; project allowance has 2 vCPU / 8Gi remaining of 24 vCPU / 96Gi.',
        lastTransitionTime: T('2026-08-29T16:41:50Z'),
      },
      {
        type: 'Programmed',
        status: 'False',
        reason: 'PendingQuota',
        message: 'Programming held until quota is granted.',
        lastTransitionTime: T('2026-08-29T16:41:50Z'),
      },
    ] as Condition[],
  })),

  // batch-processor — image cannot be pulled
  ...[0, 1].map((i) => ({
    name: `batch-processor-eu-west-b-${['q1r2', 's3t4'][i]}`,
    namespace: 'demo-project',
    deployment: 'batch-processor-eu-west-b',
    workload: 'batch-processor',
    location: 'eu-west-2b',
    creationTimestamp: T('2026-08-31T21:04:10Z'),
    conditions: [
      {
        type: 'Ready',
        status: 'False',
        reason: 'ImageUnavailable',
        message:
          'Failed to pull image "ghcr.io/demo/batch-processor:2026.08.31": manifest unknown.',
        lastTransitionTime: T('2026-08-31T21:05:05Z'),
      },
      {
        type: 'Programmed',
        status: 'False',
        reason: 'ImageUnavailable',
        message:
          'Failed to pull image "ghcr.io/demo/batch-processor:2026.08.31": manifest unknown.',
        lastTransitionTime: T('2026-08-31T21:05:05Z'),
      },
      {
        type: 'QuotaGranted',
        status: 'True',
        reason: 'QuotaAvailable',
        message: 'Quota granted.',
        lastTransitionTime: T('2026-08-31T21:04:20Z'),
      },
    ] as Condition[],
  })),

  // config-consumer — referenced ConfigMap missing
  {
    name: 'config-consumer-us-central-a-u5v6',
    namespace: 'demo-project',
    deployment: 'config-consumer-us-central-a',
    workload: 'config-consumer',
    location: 'us-central-1a',
    creationTimestamp: T('2026-09-01T08:15:12Z'),
    conditions: [
      {
        type: 'Ready',
        status: 'False',
        reason: 'SchedulingGatesPresent',
        message: 'Instance is gated pending referenced data.',
        lastTransitionTime: T('2026-09-01T08:16:15Z'),
      },
      {
        type: 'ReferencedDataReady',
        status: 'False',
        reason: 'SourceNotFound',
        message:
          'ConfigMap "app-config" not found in namespace "demo-project"; referenced by container "app" as a volume mount.',
        lastTransitionTime: T('2026-09-01T08:16:15Z'),
      },
      {
        type: 'QuotaGranted',
        status: 'True',
        reason: 'QuotaAvailable',
        message: 'Quota granted.',
        lastTransitionTime: T('2026-09-01T08:15:40Z'),
      },
    ],
  },

  // telemetry-agent — application crash loop
  ...[0, 1].map((i) => ({
    name: `telemetry-agent-us-central-a-${['w7x8', 'y9z0'][i]}`,
    namespace: 'demo-project',
    deployment: 'telemetry-agent-us-central-a',
    workload: 'telemetry-agent',
    location: 'us-central-1a',
    creationTimestamp: T('2026-08-28T13:44:32Z'),
    conditions: [
      {
        type: 'Ready',
        status: 'False',
        reason: 'InstanceCrashing',
        message:
          'Instance restarted 43 times; last exit code 1 after 2s. Runtime reports CrashLoopBackOff.',
        lastTransitionTime: T('2026-08-28T13:47:48Z'),
      },
      {
        type: 'Programmed',
        status: 'False',
        reason: 'InstanceCrashing',
        message: 'Instance keeps crashing on startup.',
        lastTransitionTime: T('2026-08-28T13:47:48Z'),
      },
      {
        type: 'QuotaGranted',
        status: 'True',
        reason: 'QuotaAvailable',
        message: 'Quota granted.',
        lastTransitionTime: T('2026-08-28T13:44:50Z'),
      },
    ] as Condition[],
  })),
];

// ----------------------------------------------------------------- lookups

export function listWorkloads(): Workload[] {
  return WORKLOADS;
}

export function getWorkload(name: string): Workload | undefined {
  return WORKLOADS.find((w) => w.name === name);
}

export function deploymentsFor(workload: string): WorkloadDeployment[] {
  return DEPLOYMENTS.filter((d) => d.workload === workload);
}

export function instancesFor(workload: string): Instance[] {
  return INSTANCES.filter((i) => i.workload === workload);
}

export function getInstance(name: string): Instance | undefined {
  return INSTANCES.find((i) => i.name === name);
}

export function workloadNames(): string[] {
  return WORKLOADS.map((w) => w.name);
}
