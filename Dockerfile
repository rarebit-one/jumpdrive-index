# Hosted (Track B) image: builds the static binary IN-image, because DigitalOcean
# App Platform builds from source rather than taking a prebuilt artifact. This is
# distinct from deploy/docker/Dockerfile — that one is the Starchart homelab image
# and expects a binary handed to it by the release pipeline. Mirrors the
# jumpdrive-broker template.
#
# Build stage. Pinned by digest, not tag: a moving base image means the thing that
# built yesterday is not the thing that builds today, and CI would be comparing
# against a target that shifted underneath it.
FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build

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
# JDX_AUTH=true or the process refuses to boot (ADR-0011). MODE=migrate runs as a
# PRE_DEPLOY phase (see .do/app.yaml), never at container start.
EXPOSE 8090
ENTRYPOINT ["/jumpdrive-index"]
