# syntax=docker/dockerfile:1.7

# Build args (overridable from CI):
#   GO_VERSION     — keep in sync with go.mod's `go` directive
#   ALPINE_VERSION — alpine base for the builder stage
#   VERSION        — semver string stamped into the binary via -ldflags
ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.21

# --- Build stage ----------------------------------------------------------
# Run on the host's native arch ($BUILDPLATFORM); cross-compile via
# GOOS/GOARCH so the Go compiler is never QEMU-emulated.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

# Cache module downloads on a separate layer
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dicode ./cmd/dicode

# --- Runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/dicode-ayo/dicode-core"
LABEL org.opencontainers.image.description="dicode — GitOps task orchestrator"
LABEL org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/dicode /usr/local/bin/dicode

# /data is the conventional mount point for SQLite state; users override
# `--data-dir` or set DICODE_DATA_DIR to relocate.
VOLUME ["/data"]

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/dicode"]
CMD ["daemon"]
