# ADR-0002: Resolve-before-create is a two-band decision; child-safety is a principal, never a lens

- **Status:** **Accepted** (retroactive record of the M0 resolve + access design, written 2026-08-27)
- **Date:** 2026-08-27
- **Deciders:** jumpdrive-index maintainers
- **Milestone:** M0 (resolve core + access model); refined by M2 (per-space scoping, ADR-0003)

## Context

Two decisions in the M0 core are subtle enough, and expensive enough to get wrong, to deserve
their own record: **how a new entity is reconciled against what already exists** (dedup on
write), and **how a hard safety boundary is expressed** so a query cannot switch it off.

## Decision

### 1. Resolve-before-create is a pure, two-band decision executed inside the write transaction

`domain.Resolve` (the analogue of the broker's `placement.Decide`) is **pure** — it performs no
I/O. The store does the lookups, feeds them in, and executes the returned verdict inside the
append transaction, so the dedup decision and the write cannot race. Precedence:

1. **Idempotency short-circuit** on `(writer, dedupe_key)` — a replay returns the existing node.
2. **Exact external-ID match is authoritative** (any policy but `force_new`): one hit → attach;
   several → merge them, then attach. An external id (`tmdb`, `imdb`, `heyarr-work`, …) is the
   objective join key. (M2/ADR-0003 scopes this match to the write's space.)
3. **Policy gate** (`external_only` / `force_new`) stops before the vector stage.
4. **Vector two-band** (only when no external id resolved):
   - `≥ AutoMerge` floor (cosine ~0.94) → **attach** to the near-duplicate.
   - `≥ Review` band (~0.86, `< AutoMerge`) → **insert new PLUS an inferred `sameAs?` edge** —
     never auto-merge in the band.
   - below Review → a plain new node.

The two-band split exists because **the two error directions are not symmetric**. A missed
duplicate is a cheap, later-fixable annoyance (two nodes that should be one). A **false merge
corrupts two real things into one and is dear to undo** — so auto-merge demands strong evidence
(a high floor), and the ambiguous middle only *flags* a candidate (an inferred, low-confidence
`sameAs?` edge a human or later signal can confirm or cut) rather than destroying information.
Thresholds are config, validated at boot (`AutoMerge > Review`, both in `(0,1]`), never silently
defaulted.

`DedupeKey`, when absent, is derived `blake3(type ‖ sorted(externalIDs) ‖ normalized(name))` so
external-ID-bearing entities get stable keys (re-assert = no-op) while nameless ones get random
keys (avoiding false collision). The verdict carries a **stable, human-readable reason**, which
a drift test asserts alongside the action over a perturbation corpus.

### 2. Child-safety (and any hard boundary) is a restricted principal enforced in SQL — never a lens

The access model is two layers: a **hard ACL** = the `access.Filter` compiled to a WHERE clause
on nodes and edges (default-deny as SQL *shape*), and a **soft lens** = a query-time Go filter
applied *after* the ACL that can hide but never reveal.

A restricted principal (e.g. a child-safety boundary) is filtered by
`access.Filter.Restricted` / `DenyTypes` **inside the WHERE clause** — a boundary a query cannot
switch off. It is **never** modelled as a lens, because a lens is droppable by construction: the
whole point of a lens is that it can be removed, which is exactly the wrong property for a
safety boundary. "The boundary lives where it cannot be turned off" is the invariant.

## Consequences

- The dangerous merge/attach logic is a pure function with an exhaustive unit corpus and a
  SQL-vs-Go drift test — the single place a reviewer reads to trust dedup.
- A false merge is structurally hard to trigger (needs external-ID evidence or above-floor
  vector similarity); the review band preserves both nodes plus an auditable `sameAs?` hint.
- Safety boundaries are expressed once, in the ACL WHERE clause, and are unaffected by lens
  changes; adding a lens can only ever *further* restrict what a principal sees.
- **Revisit if** measured false-merge/miss rates argue for different thresholds (they are
  config, so this is a tuning change, not a design change), or if a boundary needs cross-field
  logic the flat `Restricted`/`DenyTypes` filter cannot express.
