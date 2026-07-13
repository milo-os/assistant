// Canned StreamCo data. This is the single source of truth for the demo
// provider's tool responses; the selftest and the e2e assertions key off
// these exact values (notably pipeline p-1's findings/recommendation).

export interface StreamSummary {
  id: string;
  name: string;
  status: 'healthy' | 'degraded';
  rps: number;
}

export interface StreamDetail extends StreamSummary {
  region: string;
  lagSeconds: number;
}

export interface Finding {
  severity: 'critical' | 'warning' | 'info';
  code: string;
  message: string;
}

export interface Diagnosis {
  id: string;
  pipeline: string;
  findings: Finding[];
  recommendation: string;
}

export const STREAMS: StreamDetail[] = [
  {
    id: 's-1',
    name: 'checkout-events',
    status: 'healthy',
    rps: 1240,
    region: 'us-east-1',
    lagSeconds: 2,
  },
  {
    id: 's-2',
    name: 'playback-telemetry',
    status: 'degraded',
    rps: 861,
    region: 'us-east-1',
    lagSeconds: 847,
  },
  {
    id: 's-3',
    name: 'ad-impressions',
    status: 'healthy',
    rps: 3322,
    region: 'eu-west-2',
    lagSeconds: 0,
  },
];

export function streamSummaries(): StreamSummary[] {
  return STREAMS.map(({ id, name, status, rps }) => ({ id, name, status, rps }));
}

export function getStream(id: string): StreamDetail | undefined {
  return STREAMS.find((s) => s.id === id);
}

export const DIAGNOSES: Record<string, Diagnosis> = {
  'p-1': {
    id: 'p-1',
    pipeline: 'vod-transcode',
    findings: [
      {
        severity: 'critical',
        code: 'CONSUMER_LAG',
        message:
          'Consumer group vod-transcode is 847s behind on stream s-2 (playback-telemetry), partition 3.',
      },
      {
        severity: 'warning',
        code: 'CHECKPOINT_INTERVAL',
        message:
          'Checkpoint interval is 300s; recommended <= 60s for lag-sensitive pipelines.',
      },
      {
        severity: 'info',
        code: 'THROUGHPUT_OK',
        message: 'Ingest throughput 861 rps is within provisioned capacity.',
      },
    ],
    recommendation:
      'Scale consumer group vod-transcode from 2 to 4 workers and lower the checkpoint interval to 60s. Runbook: http://127.0.0.1:7810/runbooks/lag.md',
  },
  'p-2': {
    id: 'p-2',
    pipeline: 'ad-rollup',
    findings: [
      {
        severity: 'info',
        code: 'HEALTHY',
        message: 'All consumer groups current; no lag detected on ad-impressions.',
      },
    ],
    recommendation: 'No action required.',
  },
};

export function diagnose(id: string): Diagnosis | undefined {
  return DIAGNOSES[id];
}

export interface DeleteResult {
  id: string;
  deleted: boolean;
  note: string;
}

/**
 * Canned "destructive" tool used only by the gateway slice's allow-list proof:
 * StreamCo exposes streams_delete, but the gateway MCPRoute toolSelector
 * EXCLUDES it, so it must be reachable directly yet absent/blocked through the
 * gateway. It is a demo no-op — it never mutates STREAMS.
 */
export function deleteStream(id: string): DeleteResult | undefined {
  if (!getStream(id)) return undefined;
  return {
    id,
    deleted: false,
    note: 'demo no-op: streams_delete does not actually remove the stream; it exists only to prove gateway MCPRoute allow-list exclusion.',
  };
}
