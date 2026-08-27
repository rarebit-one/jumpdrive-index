# ADR-0003: External-ID uniqueness scope — global vs per-space

- **Status:** **Accepted** (2026-08-27) — **Option B, per-space `UNIQUE(space, key)`**
- **Date:** 2026-08-27
- **Deciders:** jumpdrive-index maintainers
- **Milestone:** M2 (multi-tenant + the deferred P1); gates the hosted multi-tenant `serve` flavor. Not required for single-tenant Starchart.
- **Supersedes / relates to:** the deferred **P1** review finding — "external-id resolve is unscoped across spaces" (recorded on PR #7 / the access-model write-scoping work).

> ADR-0001 (event-sourced entity SoR + dual store/identity seams + AGPL) and ADR-0002
> (resolve-before-create two-band + child-safety-as-principal) are reserved and pending;
> this is the first ADR committed to the log because M2's decision must be made before its
> code is written.

## Context

An entity carries `ExternalIDs` (`scheme:value`, e.g. `tmdb:603`, `wikidata:Q83495`,
`heyarr-work:…`). These are the **dedup + cross-system join key**: resolve-before-create
uses an *exact external-id match* as its authoritative first band (ADR-0002 / `domain.Resolve`
precedence 1), and `ResolveByExternalID` is a first-class read.

**Today the index enforces GLOBAL uniqueness.** Both dialects declare:

```sql
CREATE UNIQUE INDEX entity_external_ids_key ON entity_external_ids (key);
```

and resolve-before-create looks up hits with **no space or access scoping** —
`externalHitsTx` runs `SELECT … FROM entity_external_ids WHERE key IN (…)` across the entire
database (`internal/store/postgres/entities.go`, and the SQLite mirror). One `tmdb:603` maps
to exactly one entity, index-wide.

This is correct and cheap for **single-tenant Starchart** (one tenant → "everything" is one
scope). It is **unsafe for hosted multi-tenant `serve`**, and that gap is the deferred P1:

- **Reads are already hard-filtered.** Every read takes an `access.Filter` compiled to a
  WHERE clause on nodes and edges; a caller cannot *read* another space's private entity.
- **The resolve WRITE path is NOT filtered.** `externalHitsTx` ignores access. So when tenant
  B asserts an entity carrying `tmdb:603` that tenant A already holds privately, resolve
  **attaches B's write to A's node** (precedence 1, `ActionAttach`). That both *leaks the
  existence* of A's entity to B and lets B mutate its projection (prop-union) — a
  cross-tenant write-path leak that the read filter cannot catch, because it happens before
  any read filter applies.

So the question M2 must answer is a **schema + topology** decision, not a bug fix in
isolation: **is an external id globally unique across the whole index, or unique per space?**
The answer determines the schema, the resolve scoping, and whether hosted `serve` can be
multi-tenant with hard per-tenant isolation.

## Options

### Option A — Global uniqueness (keep `UNIQUE(key)`)

One external id → one entity, index-wide. Cross-space joins are implicit and free.

- **Pro:** simplest; semantically true for *authoritative* catalogues — a `tmdb:603` is
  objectively one film no matter who references it. Zero schema change. Single-tenant
  Starchart is unaffected.
- **Con:** cannot host mutually-distrusting tenants. To make the resolve *write* safe you
  would have to scope `externalHitsTx` to the caller's writable spaces — but a global unique
  index makes that self-contradictory: two tenants literally cannot both hold `tmdb:603`.
  Global uniqueness therefore means **shared identity across tenants**, viable only for
  single-tenant, or for a deliberately shared/trusted catalogue.

### Option B — Per-space uniqueness (`UNIQUE(space, key)`) — *recommended target*

The same external id may exist **once per space**; each tenant/space gets its own node for
`tmdb:603`.

Requires three coordinated changes:
1. **Schema (both dialects + a migration):** replace `UNIQUE(key)` with `UNIQUE(space, key)`
   on `entity_external_ids` (carry `space` onto the child row, or join to the parent). PG and
   SQLite migrations, kept honest by the conformance suite.
2. **Scope resolve to the caller's WRITABLE spaces:** `externalHitsTx` (and
   `ResolveByExternalID`'s write-side use) filter by the writer's space(s), so a write can
   only attach to / merge within spaces the caller may write. This is exactly the **P1 fix**.
3. **Space-scope the merge/bridge path** (`external_collision_merges`): a bridging candidate
   merges only nodes inside the writable scope.

- **Pro:** correct isolation for hosted multi-tenant — no cross-tenant leak via the write
  path; fixes the P1 *at the schema level* rather than as a filter bolted onto a
  globally-unique index. **Single-tenant Starchart behavior is unchanged** (with one space —
  today entities default to `space=""` — `UNIQUE(space,key)` is identical to `UNIQUE(key)`;
  the existing conformance tests, which keep all entities in one implicit space, pass
  unchanged).
- **Con:** the same real-world title is duplicated across spaces (accepted — each tenant's
  graph is its own). Any *cross-space* reconciliation becomes an explicit, opt-in feature
  rather than an implicit join. The resolve path is more complex (scope must be threaded from
  the caller through `AppendEntityFact`).

### Option C — Hybrid (global for authoritative schemes, per-space for local) — *considered, rejected for now*

Keep `wikidata`/`tmdb`/`imdb` globally unique (objective catalogues) while `heyarr-*` and
locally-minted schemes are per-space. Most semantically precise, but it splits the resolve
path down two code paths, complicates the merge logic, and bakes a scheme-classification
policy into the schema. Over-engineered before there is a concrete need; revisit if a shared
authoritative catalogue is ever wanted *alongside* tenant isolation.

## Decision

**Accepted: Option B — per-space uniqueness (`UNIQUE(space, key)`)** (maintainer decision,
2026-08-27).

Rationale: it is the only option that makes resolve-before-create safe as a *write* under
multi-tenancy and it fixes the deferred P1 at the schema layer, while leaving single-tenant
Starchart behavior identical (with all entities in one space, `UNIQUE(space,key)` is
equivalent to `UNIQUE(key)`). The cost — a schema change in both adapters plus resolve
write-scoping — is accepted as deliberate M2 work.

**Sequencing:** M2 implementation is scheduled **after M1 (Postgres full parity) merges.**
Both touch the Postgres resolve path (`entities.go` / `externalHitsTx`) and add migrations;
running them concurrently would conflict heavily. M1 lands first (it brings the PG adapter to
parity on the *existing* global-uniqueness semantics); M2 then changes the uniqueness rule
across both adapters on top of a settled resolve path.

## Consequences

- **If B:** M2 lands the `UNIQUE(space, key)` migration (PG + SQLite), threads the caller's
  writable-space scope into `externalHitsTx` / the resolve path / the merge path, and adds a
  conformance subtest proving a write in space X cannot attach to or reveal a private entity
  in space Y. The P1 is closed by this work.
- **If A:** the `entity_external_ids_key` index stays; M2's multi-tenant scope is limited to
  read-side space grants (already implemented); the P1 is downgraded to "resolve is global by
  design" with an explicit note that hosted `serve` must not mix distrusting tenants.
- Either way the **conformance suite** is the guard: whatever uniqueness rule is chosen, both
  adapters must enforce it identically, so the decision is pinned by one behavioral matrix.
