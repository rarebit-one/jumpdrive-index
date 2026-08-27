-- 0003_per_space_external_ids (Postgres): external ids are unique PER SPACE, not
-- globally (ADR-0003, accepted) — the parallel of the SQLite 0004 migration.
-- Denormalise the owning entity's space onto the external-id row so a
-- UNIQUE(space, key) index can hold it (a unique index cannot span the entities
-- join), and swap the global unique index for the per-space one. The composite
-- PRIMARY KEY (entity_id, key) is unchanged. Resolve-before-create is scoped to
-- the write's target space in code (externalHitsTx), closing the deferred P1.
--
-- Single-tenant hosted use is unaffected: with one space, UNIQUE(space, key) is
-- identical to the old UNIQUE(key).

ALTER TABLE entity_external_ids ADD COLUMN space TEXT NOT NULL DEFAULT '';

-- Backfill existing rows from their parent entity (no-op on a fresh db).
UPDATE entity_external_ids x SET space = e.space
  FROM entities e WHERE e.id = x.entity_id;

DROP INDEX entity_external_ids_key;
CREATE UNIQUE INDEX entity_external_ids_space_key ON entity_external_ids (space, key);
