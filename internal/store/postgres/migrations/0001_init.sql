-- 0001_init (Postgres): the parallel translation of the SQLite core schema — the
-- append-only fact log (system of record) + the rebuildable projection.
--
-- Dialect choices vs SQLite: jsonb for props/payload (json_valid CHECK is gone —
-- jsonb enforces it); bytea for float32 embedding blobs (same little-endian
-- encoding as SQLite, so the Go codec is shared in spirit); bigserial for the
-- monotonic seq. Ids are TEXT (UUIDv7 minted in Go, identical to the SQLite
-- adapter) and timestamps are TEXT RFC3339Nano strings, so the fold logic stays
-- byte-parallel with SQLite. The enum-like columns are TEXT + CHECK rather than
-- native ENUM types: a bound string parameter binds to TEXT without a per-column
-- cast, which removes a class of pgx encoding edge cases; the behavioural
-- conformance suite is indifferent to ENUM vs CHECK.

CREATE TABLE facts (
    seq        BIGSERIAL PRIMARY KEY,
    id         TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN (
                   'entity.asserted','entity.retracted',
                   'edge.asserted','edge.retracted','entity.merged')),
    subject    TEXT NOT NULL,
    writer     TEXT NOT NULL,
    dedupe_key TEXT NOT NULL,
    payload    JSONB NOT NULL,
    actor      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (writer, dedupe_key)
);

CREATE TABLE entities (
    id               TEXT PRIMARY KEY,
    type             TEXT NOT NULL,
    props            JSONB NOT NULL,
    space            TEXT NOT NULL,
    owner            TEXT NOT NULL,
    visibility       TEXT NOT NULL CHECK (visibility IN ('private','space','public')),
    prov_asserter    TEXT NOT NULL,
    prov_method      TEXT NOT NULL CHECK (prov_method IN ('asserted','inferred')),
    prov_source      TEXT NOT NULL,
    prov_confidence  DOUBLE PRECISION NOT NULL,
    prov_asserted_at TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);
CREATE INDEX entities_by_type ON entities (type);

CREATE TABLE entity_external_ids (
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    scheme    TEXT NOT NULL,
    value     TEXT NOT NULL,
    key       TEXT NOT NULL,
    PRIMARY KEY (entity_id, key)
);
CREATE UNIQUE INDEX entity_external_ids_key ON entity_external_ids (key);

CREATE TABLE entity_labels (
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    label     TEXT NOT NULL,
    PRIMARY KEY (entity_id, label)
);

CREATE TABLE embeddings (
    entity_id TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    model     TEXT NOT NULL,
    field     TEXT NOT NULL,
    dim       INTEGER NOT NULL,
    vector    BYTEA NOT NULL,          -- little-endian float32, dim*4 bytes
    PRIMARY KEY (entity_id, model, field)
);

CREATE TABLE edges (
    id               TEXT PRIMARY KEY,
    predicate        TEXT NOT NULL,
    from_id          TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_id            TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    props            JSONB,
    space            TEXT NOT NULL,
    owner            TEXT NOT NULL,
    visibility       TEXT NOT NULL CHECK (visibility IN ('private','space','public')),
    prov_asserter    TEXT NOT NULL,
    prov_method      TEXT NOT NULL CHECK (prov_method IN ('asserted','inferred')),
    prov_source      TEXT NOT NULL,
    prov_confidence  DOUBLE PRECISION NOT NULL,
    prov_asserted_at TEXT NOT NULL,
    created_at       TEXT NOT NULL
);
CREATE INDEX edges_from ON edges (from_id);
CREATE INDEX edges_to ON edges (to_id);
