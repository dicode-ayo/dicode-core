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
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dicode ./cmd/dicode

# --- Runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="dicode-core" \
      org.opencontainers.image.description="dicode — GitOps task orchestrator" \
      org.opencontainers.image.url="https://github.com/dicode-ayo/dicode-core" \
      org.opencontainers.image.documentation="https://github.com/dicode-ayo/dicode-core#readme" \
      org.opencontainers.image.source="https://github.com/dicode-ayo/dicode-core" \
      org.opencontainers.image.vendor="dicode-ayo" \
      org.opencontainers.image.licenses="AGPL-3.0-only"

COPY --from=build /out/dicode /usr/local/bin/dicode

# The daemon honors DICODE_DATA_DIR (cmd/dicode/main.go) for SQLite,
# sources, and run logs. Setting it here aligns the VOLUME with the
# default state path so `-v vol:/data` works without further config.
ENV DICODE_DATA_DIR=/data
VOLUME ["/data"]

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/dicode"]
CMD ["daemon"]
