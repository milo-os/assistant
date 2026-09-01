# StreamCo demo MCP provider image for the playground (CONTRACT-REAL-ENV.md
# component 3). Mirrors the proven e2e/gateway/streamco/Dockerfile so deploy/ is
# self-contained. Build context is e2e/streamco:
#
#   docker build -t patch-streamco:local -f deploy/streamco.Dockerfile e2e/streamco
#   kind load docker-image patch-streamco:local --name test-infra
#
# Node >=22.18 type-strips StreamCo's .ts sources natively; its node_modules
# (@modelcontextprotocol/sdk, zod) are pure JS, so copying them in is
# arch-independent. Served on 7810; the gateway MCPRoute fronts it and enforces
# the reviewed tool allow-list (streams_delete excluded).
FROM node:22-slim
WORKDIR /app
COPY . .
ENV STREAMCO_HOST=0.0.0.0 \
    STREAMCO_PORT=7810
EXPOSE 7810
CMD ["node", "src/server.ts"]
