# syntax=docker/dockerfile:1

# Build stage runs on the build host's native platform ($BUILDPLATFORM) and
# cross-compiles for the target ($TARGETOS/$TARGETARCH): no QEMU emulation of
# the Go toolchain, correct output for any --platform.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/helm-render-service .

# Runtime: distroless static, nonroot. No shell, no libc, no package manager --
# the service execs nothing by design (helm SDK in-process only), so the image
# needs exactly one file. Probes are HTTP (/healthz); no HEALTHCHECK binary
# exists or is needed.
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/braghettos/helm-render-service" \
      org.opencontainers.image.description="Stateless helm-template render/dry-run HTTP service (client-only helm SDK, no cluster access)" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/helm-render-service /helm-render-service
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/helm-render-service"]
