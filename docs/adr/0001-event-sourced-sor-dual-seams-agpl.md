# ADR-0001: Event-sourced entity system-of-record, two swappable seams, AGPL

- **Status:** **Accepted** (retroactive record of the M0 foundation, written 2026-08-27)
- **Date:** 2026-08-27
- **Deciders:** jumpdrive-index maintainers
- **Milestone:** M0 (the foundation every later milestone builds on)

> This ADR is written after the fact: the decisions below were made and implemented
> across M0 (13 PRs) and are load-bearing for everything since. It is recorded now so
> the log states *why* the shape is what it is, not merely *that* it is
> (heyarr's "an ADR that merely describes the code is not worth having").

## Context

`jumpdrive-index` is a general-purpose, schema.org-based **knowledge-graph index**: typed
entities + typed first-class edges + embeddings, populated and queried by AI over **MCP**. It
is a *linking layer* over systems of record (heyarr media, notes, events), not a re-store of
them — but it is itself the **authoritative** store for the entities and edges it mints.

It must ship two ways from one codebase: a self-hosted homelab distribution (**Starchart**,
single-tenant, SQLite, CGO-off) and a hosted multi-tenant service (**Postgres**, pgvector),
and it must be embeddable in **Jumpdrive** (deferring to Jumpdrive's permission model) as well
as standalone. Three questions had to be answered before any code: how it stores truth, how it
stays swappable across those two axes, and under what licence.

## Decision

### 1. The index is its own event-sourced system of record

Entities and edges are recorded as an **append-only fact log** (`facts`, monotonic `seq`), and
the queryable graph/vector/FTS state is a **synchronously-folded, rebuildable projection** of
that log — not the source of truth. Invariants:

- Every projection row is derivable by folding facts in `seq` order. The projection is a
  disposable cache; `RebuildProjection` re-folds the whole log (so an embedding-model change
  re-embeds on fold rather than mixing vector spaces).
- Each mutation folds its fact **and** advances the projection in **one transaction**, so a
  reader can never see an entity the log does not record, or vice-versa.
- A boot check refuses to serve if `ProjectionHead != max(facts.seq)` — loud, not silent.

*Why:* the dangerous logic (dedup/merge/resolve) becomes replayable and auditable; a corrupted
projection is recoverable from the log; and "the index owns its entities" is enforced
structurally rather than by convention. The accepted cost is two durable stores by domain (the
source-of-record files in jumpdrive-web vs. the index's entities) — deliberate, not accidental.

### 2. Two seams, each a pure interface with adapters selected at build/config time

- **Storage seam — `store.Store`**: one interface behind a **Postgres** adapter (hosted,
  pgvector, `pg_advisory_xact_lock` to serialise the resolve→append→project window) and a
  **SQLite** adapter (homelab, pure-Go/modernc, `_txlock=immediate` single writer, float32-blob
  embeddings + Go KNN). Broker discipline: exactly one atomic method per mutation, no exposed
  transaction handle, `var _ store.Store = (*Store)(nil)` per adapter. The two dialects are kept
  from diverging by **one cross-adapter conformance matrix** run against BOTH (M0→M1: every
  behaviour that passes on one adapter must pass the other, or it is a bug caught in CI).
- **Identity seam — `access.Model`**: `Authenticate(bearer) → Decision{Principal, Lenses}`
  (deny-by-default) producing an `access.Filter` pushed into store SQL as a WHERE clause on
  **nodes AND edges**. A self-contained `starchart` impl (principals + spaces + lenses,
  token-digest auth) and a `jumpdrive` impl (delegates to Jumpdrive's authorizer). Selected by
  `IDENTITY_MODE` (default-deny enum).

*Why:* the two product axes (homelab↔hosted, standalone↔embedded) are orthogonal and each is a
single interface boundary, so a change on one axis cannot leak into the other. `domain`,
`access` (iface) and `embed` (iface) import nothing from DB/HTTP — the pure core (`resolve.go`)
is exhaustively unit-testable and is what the SQL must match under the conformance suite.

### 3. The MCP contract (+ its REST twin) is the stable product across both builds

Reads take an `access.Filter` and filter in SQL; writes call `CanWrite`/`CanPropose` before the
store. The MCP server is **hand-rolled** JSON-RPC (`2025-06-18`), not an SDK, so the authz seam
stays auditable in one file — matching the in-org precedent (heyarr, jumpdrive-web are two
hand-rolled servers).

### 4. Licence: AGPL-3.0 (+ DCO, no CLA)

One repo, no engine/distro split, set before the first public commit — heyarr's ADR-0016
posture. The moat is product/data/ops, not the licence.

## Consequences

- The fact log is permanent and authoritative; the projection is free to change shape (new
  index kinds, re-embeds) without data migration of truth.
- Adding a store dialect or an identity backend is a new adapter behind an existing interface,
  proven by the same conformance suite / access contract — never a fork of the engine.
- Two durable stores by domain is accepted; a single merged store across systems of record was
  rejected (it would make the index a re-store, which it explicitly is not).
- **Revisit if** the projection fold ever becomes too slow to rebuild synchronously at homelab
  scale (then: incremental/checkpointed rebuild — but not a second source of truth).
