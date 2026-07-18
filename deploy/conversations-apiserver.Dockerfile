# Conversations aggregated-apiserver image. Same posture as assistant.Dockerfile:
# a static, CGO-free Go binary on distroless/static, nonroot, no shell.
#
# The binary is compiled ON THE HOST by `task dev:build`
# (CGO_ENABLED=0 GOOS=linux GOARCH=<cluster-arch> go build ./cmd/conversations-apiserver)
# and dropped into deploy/.build — we compile on the host deliberately because
# the shared test-infra kind cluster starves an in-image `go build` (see
# assistant.Dockerfile for the full rationale).
#
# Build context is deploy/.build (contains the freshly-compiled binary):
#   docker build -t conversations-apiserver:dev -f deploy/conversations-apiserver.Dockerfile deploy/.build
FROM gcr.io/distroless/static:nonroot
COPY conversations-apiserver /usr/local/bin/conversations-apiserver
EXPOSE 8443
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/conversations-apiserver"]
