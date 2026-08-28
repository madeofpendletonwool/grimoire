-- +goose Up
-- The simulation tick (MAD-367, stage 5.2 of MAD-315): advance the world by
-- N days, deterministically. One row per tick — the preview's identity. The
-- computed outcomes are NOT stored: the tick is a pure function of the
-- campaign snapshot, the plans, the schedule, the day count and the seed
-- (internal/sim), so a staged outcome set is re-derived, never cached. What
-- the row carries is the bookkeeping that makes the questions answerable:
-- the window, the seed, the snapshot digest, and where the outcomes went.
--
-- status is preview | staged | applied | discarded:
--   preview   computed, nothing staged, nothing written to the graph
--   staged    the outcomes live in one proposal_batch behind the review gate
--   applied   the batch was accepted; the clock moved by exactly the window
--   discarded the batch was dismissed; time did not pass
--
-- This issue's note on migration numbers: it claimed 0019 at branch time and
-- main's next genuinely free number is 0019 — no rebase needed.

CREATE TABLE sim_ticks (
    id              TEXT PRIMARY KEY,
    campaign_id     TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    from_day        INTEGER NOT NULL,
    to_day          INTEGER NOT NULL CHECK (to_day > from_day),
    seed            INTEGER NOT NULL,
    snapshot_digest TEXT NOT NULL,
    batch_id        TEXT REFERENCES proposal_batches(id) ON DELETE SET NULL,
    status          TEXT NOT NULL DEFAULT 'preview' CHECK (status IN ('preview','staged','applied','discarded')),
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX sim_ticks_campaign ON sim_ticks(campaign_id, created_at);
CREATE INDEX sim_ticks_batch ON sim_ticks(batch_id);

-- The tick's one new review kind: proposed_plan_transition — a faction
-- plan's whole advance (state, carried progress, the moves and the
-- arithmetic) as one batch item, applied through the faction store. The
-- canon_reviews kind list is a CHECK, and SQLite cannot widen a CHECK in
-- place, so this is the same rebuild 0007, 0009, 0011 and 0012 each did:
-- rename, recreate with the widened list, copy back, drop. Every existing
-- row — decided or open — keeps its ids, dedup keys and decisions.
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
      FROM canon_reviews_old;
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);

-- +goose Down
-- Dropping the ticks deletes the preview ledger; the batches they staged are
-- proposal_batches rows and survive (rolling back is an operator-owned
-- decision, the same rule 0012 and 0016 set). The canon_reviews rebuild is
-- reversed with the same rebuild, back to the 0012 kind list.
DROP TABLE sim_ticks;
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
     WHERE kind <> 'proposed_plan_transition';
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);
