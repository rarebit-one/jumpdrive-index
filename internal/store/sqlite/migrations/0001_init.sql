-- 0001_init: the append-only fact log (system of record) + the derived
-- projection (entities, their external ids / labels / embeddings, and edges).
-- The projection is rebuildable purely by folding facts in seq order.
--
-- SQLite dialect notes (vs the Postgres adapter): TEXT + CHECK(x IN ...) stands
-- in for PG ENUMs; TEXT CHECK(json_valid(...)) for jsonb; a child table
-- (entity_labels) for text[]; timestamps are RFC3339 TEXT. Vectors are stored as
-- float32 BLOBs and searched by brute-force Go KNN (CGO stays off), not sqlite-vec.

-- NOTE: schema_migrations is created and owned by store.Migrate, not here, so
-- this file is tracked as an ordinary migration.

-- The durable truth. (writer, dedupe_key) is unique so a replay is a no-op.
CREATE TABLE facts (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN (
                   'entity.asserted','entity.retracted',
                   'edge.asserted','edge.retracted','entity.merged')),
    subject    TEXT NOT NULL,            -- the entity/edge id this fact resolved onto
    writer     TEXT NOT NULL,
    dedupe_key TEXT NOT NULL,
    payload    TEXT NOT NULL CHECK (json_valid(payload)),
    actor      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (writer, dedupe_key)
) STRICT;

-- Projection: entities.
CREATE TABLE entities (
    id               TEXT PRIMARY KEY,
    type             TEXT NOT NULL,
    props            TEXT NOT NULL CHECK (json_valid(props)),
    space            TEXT NOT NULL,
    owner            TEXT NOT NULL,
    visibility       TEXT NOT NULL CHECK (visibility IN ('private','space','public')),
    prov_asserter    TEXT NOT NULL,
    prov_method      TEXT NOT NULL CHECK (prov_method IN ('asserted','inferred')),
    prov_source      TEXT NOT NULL,
    prov_confidence  REAL NOT NULL,
    prov_asserted_at TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
) STRICT;
CREATE INDEX entities_by_type ON entities(type);

-- One entity per external key (scheme:value). A candidate whose external ids
-- match >1 distinct entity is what triggers resolve's merge branch.
CREATE TABLE entity_external_ids (
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    scheme    TEXT NOT NULL,
    value     TEXT NOT NULL,
    key       TEXT NOT NULL,
    PRIMARY KEY (entity_id, key)
) STRICT;
CREATE UNIQUE INDEX entity_external_ids_key ON entity_external_ids(key);

CREATE TABLE entity_labels (
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    label     TEXT NOT NULL,
    PRIMARY KEY (entity_id, label)
) STRICT;

CREATE TABLE embeddings (
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    model     TEXT NOT NULL,
    field     TEXT NOT NULL,
    dim       INTEGER NOT NULL,
    vector    BLOB NOT NULL,            -- little-endian float32, dim*4 bytes
    PRIMARY KEY (entity_id, model, field)
) STRICT;

-- Projection: edges. Edges carry their OWN visibility, independent of endpoints.
CREATE TABLE edges (
    id               TEXT PRIMARY KEY,
    predicate        TEXT NOT NULL,
    from_id          TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_id            TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    props            TEXT CHECK (props IS NULL OR json_valid(props)),
    space            TEXT NOT NULL,
    owner            TEXT NOT NULL,
    visibility       TEXT NOT NULL CHECK (visibility IN ('private','space','public')),
    prov_asserter    TEXT NOT NULL,
    prov_method      TEXT NOT NULL CHECK (prov_method IN ('asserted','inferred')),
    prov_source      TEXT NOT NULL,
    prov_confidence  REAL NOT NULL,
    prov_asserted_at TEXT NOT NULL,
    created_at       TEXT NOT NULL
) STRICT;
CREATE INDEX edges_from ON edges(from_id);
CREATE INDEX edges_to ON edges(to_id);
