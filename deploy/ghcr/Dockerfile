# GHCR image for self-hosted deploys, first of all the STAGING index on hyperion-2
# (thesim-family/homelab-ops roles/jumpdrive-index-staging.nix, jumpdrive-web#748 Phase 3).
#
# A third Dockerfile, because neither existing one can build in CI:
#   - the root Dockerfile runs `go mod download` in-image, and void-which-binds-go is a
#     PRIVATE module, so an unauthenticated image build cannot fetch it;
#   - deploy/docker/Dockerfile is the Starchart (SQLite, distroless) image.
# This one takes the static binary that .github/workflows/publish-image.yml builds on the
# runner (with the same private-module auth as ci.yml) and gives it the SAME runtime as
# the root Dockerfile: scratch, CA bundle, uid 65532, :8090, no shell. The build context
# is dist/, so nothing else from the repo reaches the image.

# Only for the CA bundle; same pinned base as the root Dockerfile.
FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS certs

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY jumpdrive-index /jumpdrive-index
USER 65532:65532
# Serving on a routable address REQUIRES JDX_AUTH=true (ADR-0011). Migrations are a
# separate run of this image with JDX_MODE=migrate, never at serve start.
EXPOSE 8090
ENTRYPOINT ["/jumpdrive-index"]
