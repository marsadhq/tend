# tend: distroless container image
#
# Build:
#   docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t tend:latest .
#
# Run (minimal):
#   docker volume create tenddata
#   docker run -d --name tend \
#     -p 8080:8080 \
#     -v tenddata:/data \
#     -e TEND_MASTER_KEY="$(head -c 32 /dev/urandom | base64)" \
#     tend:latest
#
# Environment variables:
#   TEND_DB          SQLite path or postgres:// DSN (default: /data/tend.db)
#   TEND_MASTER_KEY  32-byte base64 key; required for dashboard + secrets.
#                    Generate: head -c 32 /dev/urandom | base64
#   TEND_ADDR        Listen address (default: :8080)
#   TEND_BASE_URL    Externally-reachable base URL (used in heartbeat ping URLs)
#
# Data volume:
#   /data is the default SQLite data directory; mount a named volume or bind
#   mount here to persist tend.db across container restarts.
#   The directory is owned by uid 65532 (nonroot) so the SQLite driver can
#   create/write the database file without privilege escalation.

# --------------------------------------------------------------------------
# Stage 1: build
# --------------------------------------------------------------------------
FROM golang:1.25 AS build

ARG VERSION=dev

WORKDIR /src

# Download dependencies before copying source so the layer is cached when
# only source files change.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source tree and build a fully-static binary.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X github.com/marsadhq/tend/internal/cli.Version=${VERSION}" \
      -o /tend \
      ./cmd/tend

# Create the data directory in the build stage so we can COPY it into the
# final image with the correct ownership (distroless has no shell or adduser).
RUN mkdir -p /data

# --------------------------------------------------------------------------
# Stage 2: final (distroless/static, nonroot)
#   Bundles: CA certificates, tzdata, no shell, no package manager.
#   Runs as uid 65532 (nonroot) by default.
# --------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

# Copy the static binary.
COPY --from=build /tend /tend

# Copy the data directory with nonroot ownership so the SQLite driver can
# create /data/tend.db without running as root.  A named Docker volume
# mounted at /data inherits this uid from the image directory on first use.
COPY --from=build --chown=nonroot:nonroot /data /data

# Default DB path inside the container.  Override with -e TEND_DB=... or
# point at a postgres:// URL for production deployments.
ENV TEND_DB=/data/tend.db

VOLUME /data

EXPOSE 8080

ENTRYPOINT ["/tend"]
CMD ["serve"]
