# Backends image for the AI Gateway e2e. Built from the StreamCo demo server
# (e2e/streamco, assistant-qa's asset — consumed here, not modified). Provides:
#   * StreamCo itself (default CMD: node src/server.ts, port 7810), and
#   * a Node 22 runtime the stub-llm Deployment reuses (it runs its own
#     ConfigMap-mounted stub-llm.mjs, so it needs only `node`).
#
# Build context is e2e/streamco. Its node_modules (@modelcontextprotocol/sdk,
# zod) are pure JS, so copying them straight in is arch-independent. Node
# >=22.18 gives native TS type-stripping for StreamCo's .ts sources.
#
# Build + load into kind (done by up.sh):
#   docker build -t patch-e2e-backends:local -f e2e/gateway/streamco/Dockerfile e2e/streamco
#   kind load docker-image patch-e2e-backends:local --name test-infra
FROM node:22-slim

WORKDIR /app
COPY . .

ENV STREAMCO_HOST=0.0.0.0 \
    STREAMCO_PORT=7810
EXPOSE 7810

CMD ["node", "src/server.ts"]
