# jumpdrive-index

A general-purpose, schema.org-based **knowledge-graph index**: typed entities +
typed edges + embeddings, populated and queried by AI over **MCP**. It is a
*linking layer* over systems of record (heyarr media, notes, events), not a
re-store — and it is its own event-sourced system of record for the graph
itself.

It ships two ways from one codebase, behind two seams:

- **Storage seam** (`internal/store`) — a `Store` interface with a **Postgres**
  adapter (hosted, multi-tenant, pgvector) and a **SQLite** adapter (homelab,
  pure-Go, CGO-off; float32-blob embeddings + brute-force Go KNN).
- **Identity seam** (`internal/access`) — an `AccessModel` interface: a
  self-contained principals + spaces + lenses model (**Starchart**, the
  self-hosted homelab distribution) or delegation to **Jumpdrive**'s permission
  model (embedded/hosted).

The **MCP contract (+ REST twin)** is the stable product across both.

> Status: **scaffold**. The pure domain core (entities, edges, facts, the
> resolve-before-create decision), the storage/identity seam interfaces, and
> boot config are in and unit-tested. The storage adapters, the HTTP/MCP server,
> and the access-model implementations are being built milestone by milestone.

## Design

The durable truth is an append-only **fact log**; the queryable graph/vector/FTS
**projection** is a disposable, synchronously-folded cache derived from it.
Writes go through **resolve-before-create** (`internal/domain/resolve.go`, a pure
decision: exact external-id match → conservative two-band vector similarity) so
the graph stays well-linked instead of accumulating duplicate nodes. Every read
is filtered by an `AccessFilter` compiled into SQL — a hard ACL on nodes **and**
edges — with soft, droppable **lenses** layered on top; child-safety is a
restricted principal, never a lens.

Package layout mirrors `jumpdrive-broker`; the homelab deploy mirrors
`heyarr-core`. See [`docs/adr/`](docs/adr/) for the decision records.

## Develop

```bash
go build ./...
go test -race ./...
gofmt -l .        # must be empty
go vet ./...
```

## License

AGPL-3.0-or-later. Contributions by DCO sign-off (`git commit -s`); no CLA.
© Rarebit One Pte. Ltd.
