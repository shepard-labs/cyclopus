# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates

# Copy go.mod and go.sum first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
# CGO_ENABLED=0 for static linking
# -ldflags="-s -w" strips debug symbols for smaller binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -ldflags="-s -w" -o cyclopus ./cmd/cyclopus

# Runtime stage
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/shepard-labs/cyclopus"
LABEL org.opencontainers.image.description="Cyclopus - Self-hosted PaaS on Kubernetes"
LABEL org.opencontainers.image.licenses="AGPL-3.0"

# Copy the binary from builder
COPY --from=builder /build/cyclopus /usr/local/bin/cyclopus

# Use non-root user (65532:65532 = nonroot in distroless)
USER 65532:65532

# Note: Kubernetes uses liveness/readiness probes instead of HEALTHCHECK.
# The binary has no subcommands — it reads config from CYCLOPUS_* env vars / flags
# and serves directly, so there is intentionally no CMD.
ENTRYPOINT ["/usr/local/bin/cyclopus"]
