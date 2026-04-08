# ── Stage 1: Build ────────────────────────────────────────────────────────────
# Use the full Alpine-based Go image so we can install the Docker CLI.
FROM golang:1.22-alpine AS builder

# ca-certificates  — TLS for API server calls
# docker-cli       — used at runtime to manage Kind node containers
RUN apk add --no-cache ca-certificates git

WORKDIR /src

# Layer-cache dependency downloads independently from source changes.
COPY go.mod go.sum ./
RUN go mod download

# Build a statically linked binary.
# -trimpath   removes local path prefixes from stack traces
# -ldflags -s -w  strips debug symbols → smaller binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -trimpath \
    -ldflags="-s -w -X main.version=0.1.0" \
    -o /autoscaler \
    ./cmd/main.go

# Verify the binary is statically linked (will fail the build if not).
RUN file /autoscaler | grep -q "statically linked" || \
    (echo "ERROR: binary is not statically linked" && exit 1)

# ── Stage 2: Docker CLI layer ─────────────────────────────────────────────────
# Extract only the docker binary from the official Docker image so Stage 3
# can copy it without pulling in any other Docker components.
FROM docker:25-cli AS docker-cli

# ── Stage 3: Runtime ──────────────────────────────────────────────────────────
# gcr.io/distroless/static-debian12 ships with:
#   • CA bundle (for TLS)
#   • timezone data
#   • no shell, no package manager → minimal attack surface
FROM gcr.io/distroless/static-debian12:nonroot

# Controller binary built in Stage 1.
COPY --from=builder /autoscaler /autoscaler

# Docker CLI copied from Stage 2.
# The controller uses `docker exec`, `docker run`, and `docker rm`
# to manage Kind node containers via the mounted Docker socket.
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker

# Run as the built-in nonroot user (uid 65532).
# Note: the Deployment manifest sets runAsNonRoot=false because the container
# needs access to the Docker socket; adjust if your socket permissions allow
# non-root access (e.g. by adding the user to the docker group).
USER nonroot:nonroot

ENTRYPOINT ["/autoscaler"]
