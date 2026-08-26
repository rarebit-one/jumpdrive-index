-- 0003_fts: a standalone FTS5 index over each entity's searchable text (its
-- string property values). One row per entity, kept in sync at fold time by
-- upsertEntitySnapshot, so it rebuilds deterministically with the projection.
-- Adding this to an already-populated database requires a RebuildProjection to
-- backfill; a fresh migrate + fold populates it as entities are asserted.

CREATE VIRTUAL TABLE entities_fts USING fts5(entity_id UNINDEXED, text);
