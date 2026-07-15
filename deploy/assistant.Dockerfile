# Assistant service image for the playground (CONTRACT-REAL-ENV.md component 1).
# Packages a static, CGO-free Go binary on a distroless/static base — no shell,
# no package manager, runs as nonroot; ca-certificates are baked in so an
# optional real-model (HTTPS) gateway leg still verifies.
#
# The binary is compiled ON THE HOST by playground-up.sh
# (CGO_ENABLED=0 GOOS=linux GOARCH=<cluster-arch> go build ./cmd/assistant) and
# dropped into the build context. We compile on the host deliberately: the
# test-infra kind cluster is SHARED (it and two sibling kind clusters plus the
# user's ipam/etcd/nats workloads contend for the Docker VM's CPU), so an
# in-image `go build` starves and stalls. The host toolchain is warm and native;
# the resulting image is byte-identical in what matters — a static binary on
# distroless. (For a CI runner with spare CPU, a multi-stage in-image compile is
# equivalent; see deploy/playground/README-PLAYGROUND.md.)
#
# Build context is deploy/.build (contains the freshly-compiled `assistant`):
#   docker build -t patch-assistant:local -f deploy/assistant.Dockerfile deploy/.build
FROM gcr.io/distroless/static:nonroot
COPY assistant /usr/local/bin/assistant
EXPOSE 7820
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/assistant"]
