# heyarr ADR-0050 (DRAFT): external identifiers are readable for knowledge-graph reconciliation

> **⚠️ This is a cross-repo PROPOSAL staged in `jumpdrive-index`, not an accepted heyarr ADR.**
> It targets `heyarr-core/docs/adr/` and is **pending heyarr-maintainer sign-off and ADR-number
> coordination**. heyarr-core's ADR log currently reaches **0049**, so **0050** is the next free
> number — but confirm it has not been claimed by an in-flight worktree before landing this in
> heyarr-core. It touches the public `/api/v1` surface, so it must not be merged cross-repo
> unilaterally. It is staged here so the `jumpdrive-index` ↔ heyarr integration (M3) records the
> dependency it is built against.

- **Status:** Proposed (draft)
- **Date:** 2026-08-27
- **Deciders:** heyarr maintainers (sign-off required)
- **Relates to:** heyarr ADR-0005 (external-id model), ADR-0019 (MCP tool/scope conventions),
  ADR-0032 (read scopes), ADR-0020 (content-addressed / byte-identity ids), ADR-0025 (graceful
  degradation). Consumer: `rarebit-one/jumpdrive-index` reference-by-id linking (its M3).

## Context

heyarr already **stores** external identifiers for a work — `tmdb`, `imdb`, and similar upstream
catalogue ids — as part of its schema (ADR-0005). They are how heyarr itself reconciles a scanned
file against an upstream catalogue.

But those ids are on **no `/api/v1` route today.** `search_content` returns
`{work_id, content_type, title, year}` and nothing else; there is no way for an API/MCP client to
read a work's `tmdb`/`imdb` id, nor to go the other way — from an upstream `(source, value)` to the
heyarr work that carries it.

`jumpdrive-index` is a schema.org knowledge graph that **references heyarr works by id and never
copies heyarr's catalogue** (its entities carry `heyarr-work` / `heyarr-edition` / `heyarr-asset` /
`heyarr-blake3` external ids; title/year/type are read back through heyarr MCP at query time). To
reconcile an entity that arrived with an upstream id (e.g. a `tmdb:603` mentioned in a YouTube
video's metadata) against the right heyarr work, it needs to resolve `tmdb:603 → work_id`. Without a
readable external-id surface it cannot — it would have to guess by title, which risks binding to the
wrong work or minting a duplicate. This is a **read gap**, not a new capability: the data exists;
only the door is missing.

## Decision

heyarr will expose its external identifiers as a first-class, read-only, additive API — so a
knowledge-graph consumer (`jumpdrive-index`) can reconcile a heyarr work against upstream catalogues
(tmdb/imdb/wikidata) without heyarr ever learning about, or storing an edge back to, that consumer.

Three additions, all read-only and all behind `ScopeRead`:

1. **`GET /api/v1/works/{id}/external-ids`** — returns the external identifiers heyarr already holds
   for a work, as `{"external_ids":[{"source":"tmdb","value":"603"},{"source":"imdb","value":"tt0090605"}]}`.
   A work with none returns an empty list (200), never a 404 on the ids resource.
2. **`GET /api/v1/external-ids?source=tmdb&value=603`** — the reverse lookup: given an upstream
   `(source, value)`, return the heyarr work id(s) that carry it, as `{"works":[{"work_id":"…"}]}`.
   No match returns an empty list (200) — a "no match" is a normal answer, not an error (ADR-0025).
3. **MCP tool `get_external_ids`** (`ScopeRead`) — the same forward/reverse capability over the
   existing `POST /api/v1/mcp` JSON-RPC surface, arguments `{"work_id":string}` OR
   `{"source":string,"value":string}`, returning the same shapes in `structuredContent`. This keeps
   an MCP-only client (the reconciliation path jumpdrive-index actually uses) from needing the REST
   surface.

Constraints this Decision commits to:

- **Additive and read-only.** No existing endpoint, tool, scope, or content type changes; no new
  content type is introduced. `search_content`'s shape is untouched.
- **Degrades to "no match", never to an error.** Absence of an id, of a work, or of a reverse hit is
  an empty result with a 200 / non-error MCP reply (ADR-0025).
- **No back-edge.** heyarr stores nothing about jumpdrive-index; the join lives entirely on the
  consumer side, keyed on the shared upstream id. heyarr remains the source of truth for its own
  catalogue; jumpdrive-index only ever *references* heyarr ids, never copies catalogue fields.
- Consistent with **ADR-0005** (external-id model), **ADR-0019** (MCP tool/scope conventions),
  **ADR-0032** (read scopes), **ADR-0020** (content-addressed / byte-identity ids), and **ADR-0025**
  (graceful degradation).

## Consequences

- **jumpdrive-index gains a deterministic reconciliation path:** `tmdb:603 → work_id` via the reverse
  lookup, and `work_id → {tmdb,imdb}` to enrich an entity it already links — both by id, never by a
  fuzzy title match, so it cannot bind to the wrong work or mint a duplicate.
- **heyarr's attack/trust surface is unchanged in kind:** the additions are read-only and require the
  same `read` scope `search_content` already requires; an MCP session is not a new trust domain
  (heyarr ADR-0011). No write path, no new content type, no state about the consumer.
- **The contract becomes part of the drift gate.** Per the plan, jumpdrive-index vendors heyarr's MCP
  contract with a `SOURCE.md` + drift test; `get_external_ids` joins `search_content` in that
  vendored contract, so a future change to either shape is caught by the gate rather than silently
  breaking reconciliation.
- **If rejected / deferred:** jumpdrive-index falls back to title-based reconciliation for
  upstream-id-only entities (lossy, can duplicate) or defers that reconciliation to M4/M6; the
  reference-by-id linking that is already shipped (M3a) is unaffected, since it stores the heyarr id
  directly rather than deriving it.
