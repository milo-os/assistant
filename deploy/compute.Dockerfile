# Datum Compute MCP provider image for the playground. Mirrors
# deploy/streamco.Dockerfile so deploy/ stays self-contained. Build context is
# e2e/compute:
#
#   docker build -t patch-compute:local -f deploy/compute.Dockerfile e2e/compute
#   kind load docker-image patch-compute:local --name test-infra
#
# Node >=22.18 type-strips the .ts sources natively; node_modules
# (@modelcontextprotocol/sdk, zod) are pure JS, so copying them in is
# arch-independent. Served on 7830; the gateway MCPRoute fronts it and enforces
# the reviewed tool allow-list (workload_delete excluded).
FROM node:22-slim
WORKDIR /app
COPY . .
ENV COMPUTE_HOST=0.0.0.0 \
    COMPUTE_PORT=7830
EXPOSE 7830
CMD ["node", "src/server.ts"]
