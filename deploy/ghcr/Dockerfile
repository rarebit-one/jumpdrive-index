# GHCR image for self-hosted deploys, first of all the STAGING index on hyperion-2
# (thesim-family/homelab-ops roles/jumpdrive-index-staging.nix, jumpdrive-web#748 Phase 3).
#
# deploy/docker/Dockerfile is the Starchart (SQLite, distroless) image. This one
# takes the static binary that .github/workflows/publish-image.yml builds on the
# runner (with the same private-module auth as ci.yml) and gives it the SAME runtime:
# scratch, CA bundle, uid 65532, :8090, no shell. The build context is dist/, so
# nothing else from the repo reaches the image.

# Only for the CA bundle; pinned by digest.
FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS certs

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY jumpdrive-index /jumpdrive-index
USER 65532:65532
# Serving on a routable address REQUIRES JDX_AUTH=true (ADR-0011). Migrations are a
# separate run of this image with JDX_MODE=migrate, never at serve start.
EXPOSE 8090
ENTRYPOINT ["/jumpdrive-index"]
