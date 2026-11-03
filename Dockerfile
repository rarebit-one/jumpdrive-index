# Self-contained source build of the hosted (Postgres) image: compiles the static
# binary IN-image. It needs access to the PRIVATE void-which-binds-go module, so an
# unauthenticated build (App Platform's, or a plain `docker build`) cannot fetch it;
# hosted deploys run the GHCR image instead, built on the runner and packaged by
# deploy/ghcr/Dockerfile with the same runtime (docs/deploy/hosted-app-platform.md).
# Distinct from deploy/docker/Dockerfile, the Starchart homelab image.
#
# Build stage. Pinned by digest, not tag: a moving base image means the thing that
# built yesterday is not the thing that builds today, and CI would be comparing
# against a target that shifted underneath it.
FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph. go.sum is copied with it or the download cannot be verified.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off produces a static binary that runs on a scratch base. The store backends
# are pure Go (modernc.org/sqlite, jackc/pgx), so nothing here needs a C toolchain.
# -trimpath keeps build paths out of the binary; the ldflags strip debug info we
# would not read from a container anyway.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/jumpdrive-index ./cmd/jumpdrive-index

# Runtime stage.
#
# scratch, not alpine: this binary needs no shell, no package manager and no libc,
# and every one of those is attack surface on an internet-facing service. It also
# means there is no shell to exec into if something goes wrong — a deliberate
# trade, and the reason logs are structured JSON.
FROM scratch

# CA certificates, for outbound TLS: the hosted build talks to a Jumpdrive
# authorizer (JDX_JUMPDRIVE_URL) and optionally a heyarr MCP endpoint
# (JDX_HEYARR_URL) over HTTPS. Without these those calls fail with an opaque x509
# error that reads like a network problem.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/jumpdrive-index /jumpdrive-index

# Non-root by uid, because scratch has no /etc/passwd to name a user in.
USER 65532:65532

# Match the default JDX_HTTP_ADDR port. Serving on a routable address REQUIRES
# JDX_AUTH=true or the process refuses to boot (ADR-0011). MODE=migrate is a
# separate run of this image (a PRE_DEPLOY job on App Platform), never at container start.
EXPOSE 8090
ENTRYPOINT ["/jumpdrive-index"]
