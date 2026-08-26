-- +goose Up
-- NPC simulation (MAD-313): the npc_reveal review kind. The "ask as this NPC"
-- simulation's reveals — campaign assertions the model invented that are not
-- in the record — stage into the same review queue every other machine
-- proposal uses, behind the same "Make it canon" gate.
--
-- SQLite cannot widen a CHECK constraint in place, so canon_reviews is
-- rebuilt the way 0007 and 0009 rebuild their tables: rename, recreate with
-- the extended kind list, copy back, drop. Nothing references canon_reviews
-- (it references campaigns / canon_candidates / canon_flags), so the rebuild
-- cascades nothing. All existing rows, decided or open, keep their ids,
-- dedup keys and decisions.

ALTER TABLE canon_reviews RENAME TO canon_reviews_old;
CREATE TABLE canon_reviews (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN (
                      'proposed_fact','proposed_event','proposed_discovery',
                      'proposed_relationship','proposed_entity',
                      'low_agreement','contradiction','engine_flag',
                      'npc_reveal')),
    status        TEXT NOT NULL DEFAULT 'open'
                      CHECK (status IN ('open','accepted','modified','dismissed')),
    dedup_key     TEXT NOT NULL,
    candidate_id  TEXT REFERENCES canon_candidates(id) ON DELETE CASCADE,
    flag_id       TEXT REFERENCES canon_flags(id) ON DELETE CASCADE,
    subject       TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '',
    result_ref    TEXT NOT NULL DEFAULT '',
    decision_note TEXT NOT NULL DEFAULT '',
    decided_by    TEXT NOT NULL DEFAULT '',
    decided_at    INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (campaign_id, dedup_key)
);

INSERT INTO canon_reviews (id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
                           subject, summary, detail, result_ref, decision_note, decided_by,
                           decided_at, created_at, updated_at)
    SELECT id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
           subject, summary, detail, result_ref, decision_note, decided_by,
           decided_at, created_at, updated_at
      FROM canon_reviews_old;
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);

-- +goose Down
-- Rebuild canon_reviews without the npc_reveal kind. Any npc_reveal rows are
-- dropped with the kind; every other row keeps its decision.
ALTER TABLE canon_reviews RENAME TO canon_reviews_old;
CREATE TABLE canon_reviews (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN (
                      'proposed_fact','proposed_event','proposed_discovery',
                      'proposed_relationship','proposed_entity',
                      'low_agreement','contradiction','engine_flag')),
    status        TEXT NOT NULL DEFAULT 'open'
                      CHECK (status IN ('open','accepted','modified','dismissed')),
    dedup_key     TEXT NOT NULL,
    candidate_id  TEXT REFERENCES canon_candidates(id) ON DELETE CASCADE,
    flag_id       TEXT REFERENCES canon_flags(id) ON DELETE CASCADE,
    subject       TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '',
    result_ref    TEXT NOT NULL DEFAULT '',
    decision_note TEXT NOT NULL DEFAULT '',
    decided_by    TEXT NOT NULL DEFAULT '',
    decided_at    INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (campaign_id, dedup_key)
);
INSERT INTO canon_reviews (id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
                           subject, summary, detail, result_ref, decision_note, decided_by,
                           decided_at, created_at, updated_at)
    SELECT id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
           subject, summary, detail, result_ref, decision_note, decided_by,
           decided_at, created_at, updated_at
      FROM canon_reviews_old
     WHERE kind <> 'npc_reveal';
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
