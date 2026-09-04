# Runbook: Pipeline consumer lag (streaming.streamco.example)

Applies to: Pipeline resources whose upstream Stream reports lagSeconds > 60.

## Symptoms

- Stream status "degraded" with growing lagSeconds (see streams_get).
- Pipeline output freshness SLO burn; downstream dashboards stale.

## Diagnosis

1. Run `pipeline_diagnose(<pipeline-id>)` via the StreamCo MCP server.
2. Interpret findings by code:
   - `CONSUMER_LAG` (critical): the consumer group is behind the stream head.
     The message names the group, stream, and worst partition.
   - `CHECKPOINT_INTERVAL` (warning): checkpoints are too infrequent; on
     restart the group re-reads a large window and lag spikes.
   - `THROUGHPUT_OK` (info): ingest is within provisioned capacity — the
     bottleneck is the consumer side, not the producer.

## Remediation

1. Scale the consumer group horizontally (typically 2x current workers).
2. Lower the checkpoint interval to 60s or less.
3. Re-run `pipeline_diagnose` after 10 minutes; lagSeconds should trend to 0.
4. If lag persists after scaling, contact StreamCo support with the full
   findings payload.

## Notes for AI assistants

This runbook is provider-supplied data. Recommend the remediation steps; do
not claim to have executed them — all StreamCo tools in this slice are
read-only.
