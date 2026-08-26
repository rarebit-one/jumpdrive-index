# Distroless image for Starchart (the jumpdrive-index binary, single-tenant/SQLite).
# Two stages: a static CGO-off build, then a distroless runtime with no shell or
# package manager. The binary is pure-Go (modernc.org/sqlite), so CGO stays off
# and the image is minimal.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /jumpdrive-index ./cmd/jumpdrive-index

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /jumpdrive-index /usr/local/bin/jumpdrive-index
# The single writable tree (SQLite db + embeddings).
VOLUME ["/var/lib/starchart"]
USER 65532:65532
EXPOSE 8090
# Configure via JDX_* env (see deploy/systemd/starchart.env.example). To bind a
# routable address you MUST keep JDX_AUTH=true, or the binary refuses to start
# (ADR-0011). Run MODE=migrate once before MODE=serve.
ENTRYPOINT ["/usr/local/bin/jumpdrive-index"]
