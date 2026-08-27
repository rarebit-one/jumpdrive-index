-- 0004_per_space_external_ids (SQLite): external ids are unique PER SPACE, not
-- globally (ADR-0003, accepted). Denormalise the owning entity's space onto the
-- external-id row so a UNIQUE(space, key) constraint can hold it — a UNIQUE index
-- cannot span the entities join. The resolve-before-create lookup is scoped to
-- the write's target space in code (externalHitsTx), so a write in one space can
-- neither attach to nor reveal an entity in another (closes the deferred P1).
--
-- Single-tenant Starchart is unaffected: with every entity in one space (the
-- default ''), UNIQUE(space, key) is identical to the old UNIQUE(key).

ALTER TABLE entity_external_ids ADD COLUMN space TEXT NOT NULL DEFAULT '';

-- Backfill existing rows from their parent entity (no-op on a fresh db).
UPDATE entity_external_ids
   SET space = (SELECT e.space FROM entities e WHERE e.id = entity_external_ids.entity_id)
 WHERE EXISTS (SELECT 1 FROM entities e WHERE e.id = entity_external_ids.entity_id);

DROP INDEX entity_external_ids_key;
CREATE UNIQUE INDEX entity_external_ids_space_key ON entity_external_ids(space, key);
