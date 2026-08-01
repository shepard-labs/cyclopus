# syntax=docker/dockerfile:1

# ── build stage ───────────────────────────────────────────────────────────────
FROM golang:1.26-bookworm AS builder
WORKDIR /src

# Download dependencies first so this layer is cached independently of source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: fully static binary, no libc dependency.
# -trimpath: strip local paths from the binary.
# -s -w: strip debug info and DWARF (smaller image).
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /cyclopus \
    ./cmd/cyclopus

# ── final stage ───────────────────────────────────────────────────────────────
# distroless/static: no shell, no package manager, minimal attack surface.
# nonroot variant: runs as UID/GID 65532 (matches the USER directive).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /cyclopus /usr/local/bin/cyclopus
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cyclopus"]
