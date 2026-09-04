# Assistant service image (CONTRACT-REAL-ENV.md component 1).
# Packages a static, CGO-free Go binary on a distroless/static base — no shell,
# no package manager, runs as nonroot; ca-certificates are baked in so an
# optional real-model (HTTPS) gateway leg still verifies.
#
# TWO WAYS TO GET THE BINARY IN, one image contract out.
#
#   DEFAULT (last stage) — compile IN-IMAGE from source. This is what
#   .github/workflows/build.yaml publishes to ghcr.io/milo-os/assistant; a
#   hosted runner has the spare CPU to do it:
#     docker build -t assistant:ci -f deploy/assistant.Dockerfile .
#
#   `--target prebuilt` — copy a binary compiled ON THE HOST by `task
#   dev:build`. The local playground uses this deliberately: the test-infra kind
#   cluster is SHARED (it and two sibling kind clusters plus the user's
#   ipam/etcd/nats workloads contend for the Docker VM's CPU), so an in-image
#   `go build` starves and stalls there:
#     docker build -t patch-assistant:local --target prebuilt \
#       -f deploy/assistant.Dockerfile .
#
# Both land the same thing: a static binary on distroless/static:nonroot.
# NOTE the build context is the REPOSITORY ROOT for both targets — the in-image
# compile needs the source tree, and `prebuilt` reads deploy/.build/assistant.

# ── Runtime (playground): a host-compiled binary, no in-image go build ──
FROM gcr.io/distroless/static:nonroot AS prebuilt
COPY deploy/.build/assistant /usr/local/bin/assistant
COPY deploy/.build/assistant-apiserver /usr/local/bin/assistant-apiserver
EXPOSE 7820
EXPOSE 8443
# Numeric UID, not the `nonroot` name: kubelet resolves image users only
# numerically, so with runAsNonRoot=true and no runAsUser it cannot prove a
# named user is non-root and refuses to start the container. 65532 is what
# distroless's `nonroot` resolves to.
USER 65532:65532
# The service is the default; the apiserver Deployment overrides `command`.
# One image because the two are one module released together: they share the
# conversation-store schema and the assistant.miloapis.com types, so a version
# skew between them is a bug that separate images would make possible and a
# single image makes unrepresentable.
ENTRYPOINT ["/usr/local/bin/assistant"]

# ── Builder: compile ./cmd/assistant statically ────────────────────────
# Pinned to the `go` directive in go.mod — bump both together.
FROM golang:1.26 AS builder
WORKDIR /src

# Module graph first, so a source-only edit reuses the cached download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/assistant ./cmd/assistant
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
      -o /out/assistant-apiserver ./cmd/assistant-apiserver

# ── Runtime (default): the freshly compiled binary ─────────────────────
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/assistant /usr/local/bin/assistant
COPY --from=builder /out/assistant-apiserver /usr/local/bin/assistant-apiserver
EXPOSE 7820
EXPOSE 8443
# Numeric UID, not the `nonroot` name: kubelet resolves image users only
# numerically, so with runAsNonRoot=true and no runAsUser it cannot prove a
# named user is non-root and refuses to start the container. 65532 is what
# distroless's `nonroot` resolves to.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/assistant"]
