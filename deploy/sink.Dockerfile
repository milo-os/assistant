# Usage capture sink image for the playground (CONTRACT-REAL-ENV.md component 6
# proof P6). Zero-dependency Node ESM server (e2e/sink/sink.mjs) — the local
# stand-in for the platform's usage collector. Build context is e2e/sink:
#
#   docker build -t patch-sink:local -f deploy/sink.Dockerfile e2e/sink
#   kind load docker-image patch-sink:local --name test-infra
#
# CAPTURE_FILE points at an in-container writable path (an emptyDir is mounted
# there by the Deployment); GET /events reads it back for the P6 assertion.
FROM node:22-slim
WORKDIR /app
COPY sink.mjs ./
ENV SINK_HOST=0.0.0.0 \
    SINK_PORT=7811 \
    CAPTURE_FILE=/data/captured-events.jsonl
EXPOSE 7811
CMD ["node", "sink.mjs"]
