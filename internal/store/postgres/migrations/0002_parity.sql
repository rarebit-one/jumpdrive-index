-- 0002_parity (Postgres): brings the adapter to full parity with SQLite —
-- pgvector semantic search, a tsvector full-text index, and the governed-write
-- holding table. The dialect divergences (kept honest by the conformance suite):
--
--   * SemanticSearch: SQLite brute-forces cosine over float32 blobs in Go; here a
--     real pgvector `embedding` column carries the search index server-side. The
--     existing little-endian `vector` BYTEA stays the round-trip / RebuildProjection
--     source of truth (loadEntityChildren reads it, byte-parallel with SQLite);
--     `embedding` is populated ALONGSIDE it in upsertEntitySnapshot and is only ever
--     read by the KNN. A model space is one fixed dimension, so an unmodified
--     `vector` column (any dim) is safe: every KNN filters by model first.
--   * FullTextSearch: SQLite uses a standalone FTS5 virtual table; here a
--     `search_text` tsvector column on entities (kept in sync at fold time, exactly
--     like SQLite's entities_fts) with a GIN index. Because it is a column, DELETE
--     of an entity removes it — no separate cascade, unlike SQLite's FTS table.
--   * proposals: the byte-for-byte analogue of SQLite's 0002_proposals holding
--     table (JSONB payload; the json_valid CHECK is implicit in JSONB).

CREATE EXTENSION IF NOT EXISTS vector;

ALTER TABLE embeddings ADD COLUMN embedding vector;   -- nullable; pgvector search index, mirrors the BYTEA blob

ALTER TABLE entities ADD COLUMN search_text tsvector;
CREATE INDEX entities_search_text ON entities USING GIN (search_text);

CREATE TABLE proposals (
    id             TEXT PRIMARY KEY,
    kind           TEXT NOT NULL CHECK (kind IN ('entity.asserted','edge.asserted')),
    proposer       TEXT NOT NULL,
    space          TEXT NOT NULL,
    payload        JSONB NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','promoted','discarded')),
    created_at     TEXT NOT NULL,
    decided_at     TEXT,
    decided_by     TEXT,
    result_subject TEXT
);
CREATE INDEX proposals_by_space_status ON proposals (space, status);
