-- 0002_proposals: the governed-write holding table. A proposal stores the exact
-- serialized write intent (an AppendEntityInput / AppendEdgeInput) so an approver
-- can replay it through the normal resolve path — mirroring jumpdrive-web's
-- KnowledgePromotion. Nothing is projected until a proposal is promoted.

CREATE TABLE proposals (
    id             TEXT PRIMARY KEY,
    kind           TEXT NOT NULL CHECK (kind IN ('entity.asserted','edge.asserted')),
    proposer       TEXT NOT NULL,
    space          TEXT NOT NULL,
    payload        TEXT NOT NULL CHECK (json_valid(payload)),
    status         TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','promoted','discarded')),
    created_at     TEXT NOT NULL,
    decided_at     TEXT,
    decided_by     TEXT,
    result_subject TEXT               -- the entity/edge id the approved write resolved onto
) STRICT;

CREATE INDEX proposals_by_space_status ON proposals (space, status);
