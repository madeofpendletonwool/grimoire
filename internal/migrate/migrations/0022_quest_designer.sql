-- +goose Up
-- The quest designer (MAD-371, stage 5.2 of MAD-316): a hook becomes a
-- branching quest. Every quest-side row this issue writes goes through
-- MAD-359's batch tables and the quest tables MAD-369 adds — no new tables.
-- But the quest itself is a new kind of graph object a batch can propose, so
-- canon_reviews gains the one review kind proposed_quest: a whole quest
-- (machine, cast links, state facts) — or, from the "branch this quest"
-- operation, an edit of an existing quest's machine — as one batch item,
-- applied through the campaign store.
--
-- The kind list is a CHECK, and SQLite cannot widen a CHECK in place, so
-- this is the same rebuild 0007, 0009, 0011, 0012 and 0019 each did: rename,
-- recreate with the widened list, copy back, drop. Every existing row —
-- decided or open — keeps its ids, dedup keys and decisions.

ALTER TABLE canon_reviews RENAME TO canon_reviews_old;
CREATE TABLE canon_reviews (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN (
                      'proposed_fact','proposed_event','proposed_discovery',
                      'proposed_relationship','proposed_entity',
                      'proposed_plan_transition','proposed_quest',
                      'low_agreement','contradiction','engine_flag',
                      'npc_reveal')),
    status        TEXT NOT NULL DEFAULT 'open'
                      CHECK (status IN ('open','accepted','modified','dismissed')),
    dedup_key     TEXT NOT NULL,
    candidate_id  TEXT REFERENCES canon_candidates(id) ON DELETE CASCADE,
    flag_id       TEXT REFERENCES canon_flags(id) ON DELETE CASCADE,
    batch_id      TEXT REFERENCES proposal_batches(id) ON DELETE CASCADE,
    depends_on    TEXT NOT NULL DEFAULT '[]',
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
                           batch_id, depends_on, subject, summary, detail, result_ref,
                           decision_note, decided_by, decided_at, created_at, updated_at)
    SELECT id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
           batch_id, depends_on, subject, summary, detail, result_ref,
           decision_note, decided_by, decided_at, created_at, updated_at
      FROM canon_reviews_old;
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);

-- +goose Down
-- The rebuild reversed, back to the 0019 kind list; proposed_quest items go
-- with it (rolling back is an operator-owned decision, the same rule 0012,
-- 0016 and 0019 set).
ALTER TABLE canon_reviews RENAME TO canon_reviews_old;
CREATE TABLE canon_reviews (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN (
                      'proposed_fact','proposed_event','proposed_discovery',
                      'proposed_relationship','proposed_entity',
                      'proposed_plan_transition',
                      'low_agreement','contradiction','engine_flag',
                      'npc_reveal')),
    status        TEXT NOT NULL DEFAULT 'open'
                      CHECK (status IN ('open','accepted','modified','dismissed')),
    dedup_key     TEXT NOT NULL,
    candidate_id  TEXT REFERENCES canon_candidates(id) ON DELETE CASCADE,
    flag_id       TEXT REFERENCES canon_flags(id) ON DELETE CASCADE,
    batch_id      TEXT REFERENCES proposal_batches(id) ON DELETE CASCADE,
    depends_on    TEXT NOT NULL DEFAULT '[]',
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
                           batch_id, depends_on, subject, summary, detail, result_ref,
                           decision_note, decided_by, decided_at, created_at, updated_at)
    SELECT id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
           batch_id, depends_on, subject, summary, detail, result_ref,
           decision_note, decided_by, decided_at, created_at, updated_at
      FROM canon_reviews_old
     WHERE kind <> 'proposed_quest';
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);
