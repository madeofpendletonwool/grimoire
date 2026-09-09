-- +goose Up
-- +goose NO TRANSACTION
-- Player journals (MAD-489, stage 3 of MAD-319): the belief loop into the
-- canon engine. The session_sources storage was built for this in 0004 —
-- player_journal is a first-class kind with an author column, and this
-- migration adds no table. But the canon engine's kind vocabularies are
-- CHECKs, and the loop stages a genuinely new kind of candidate: a belief —
-- a journal claim held by its author at an awareness stance, resolved
-- against what the campaign already records, never written as a fact. The
-- queue mirrors it with proposed_belief, the item an agreeing belief
-- becomes; a contradicting belief is claimed by the existing contradiction
-- kind, pairing journal claim against canon fact for the DM.
--
-- canon_candidates is a referenced table (canon_verdicts, canon_reviews and
-- the batch tables all carry candidate_id REFERENCES), so this is SQLite's
-- own rebuild discipline, the same one 0031 used for session_events — not
-- the rename dance 0022 and 0024 used for canon_reviews, which nothing
-- references: a rename there would rewrite every REFERENCES clause into the
-- grave and the old table's drop would cascade the verdicts away. Instead
-- foreign_keys comes OFF (safe here: the app holds ONE connection, and NO
-- TRANSACTION keeps every statement of this migration on it), the
-- replacements are built under new names, the originals drop, the new ones
-- take the names — every surviving REFERENCES clause still spells
-- "canon_candidates" and is valid again — and foreign_keys comes back ON.

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE canon_candidates_new (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES canon_runs(id) ON DELETE CASCADE,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL,
    source_id      TEXT NOT NULL,
    chunk_index    INTEGER NOT NULL DEFAULT 0,
    kind           TEXT NOT NULL CHECK (kind IN ('fact','event','discovery','relationship','entity','belief')),
    payload        TEXT NOT NULL,
    confidence     REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    span_start     INTEGER NOT NULL,
    span_end       INTEGER NOT NULL,
    quote          TEXT NOT NULL,
    checksum       TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);
INSERT INTO canon_candidates_new (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
SELECT id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at
  FROM canon_candidates;
DROP TABLE canon_candidates;
ALTER TABLE canon_candidates_new RENAME TO canon_candidates;
CREATE INDEX canon_candidates_campaign ON canon_candidates(campaign_id, kind, created_at);
CREATE INDEX canon_candidates_run ON canon_candidates(run_id);
CREATE INDEX canon_candidates_source ON canon_candidates(source_id);

CREATE TABLE canon_reviews_new (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN (
                      'proposed_fact','proposed_event','proposed_discovery',
                      'proposed_relationship','proposed_entity',
                      'proposed_rumor','proposed_belief',
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
INSERT INTO canon_reviews_new (id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
                               batch_id, depends_on, subject, summary, detail, result_ref,
                               decision_note, decided_by, decided_at, created_at, updated_at)
SELECT id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
       batch_id, depends_on, subject, summary, detail, result_ref,
       decision_note, decided_by, decided_at, created_at, updated_at
  FROM canon_reviews;
DROP TABLE canon_reviews;
ALTER TABLE canon_reviews_new RENAME TO canon_reviews;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
-- The rebuild reversed, back to the 0024 kind lists; belief candidates and
-- proposed_belief items go with it (rolling back is an operator-owned
-- decision, the same rule 0012, 0019 and 0022 set).

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE canon_candidates_old (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES canon_runs(id) ON DELETE CASCADE,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL,
    source_id      TEXT NOT NULL,
    chunk_index    INTEGER NOT NULL DEFAULT 0,
    kind           TEXT NOT NULL CHECK (kind IN ('fact','event','discovery','relationship','entity')),
    payload        TEXT NOT NULL,
    confidence     REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    span_start     INTEGER NOT NULL,
    span_end       INTEGER NOT NULL,
    quote          TEXT NOT NULL,
    checksum       TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);
INSERT INTO canon_candidates_old (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
SELECT id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at
  FROM canon_candidates WHERE kind <> 'belief';
DROP TABLE canon_candidates;
ALTER TABLE canon_candidates_old RENAME TO canon_candidates;
CREATE INDEX canon_candidates_campaign ON canon_candidates(campaign_id, kind, created_at);
CREATE INDEX canon_candidates_run ON canon_candidates(run_id);
CREATE INDEX canon_candidates_source ON canon_candidates(source_id);

CREATE TABLE canon_reviews_old (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN (
                      'proposed_fact','proposed_event','proposed_discovery',
                      'proposed_relationship','proposed_entity',
                      'proposed_rumor',
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
INSERT INTO canon_reviews_old (id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
                               batch_id, depends_on, subject, summary, detail, result_ref,
                               decision_note, decided_by, decided_at, created_at, updated_at)
SELECT id, campaign_id, kind, status, dedup_key, candidate_id, flag_id,
       batch_id, depends_on, subject, summary, detail, result_ref,
       decision_note, decided_by, decided_at, created_at, updated_at
  FROM canon_reviews WHERE kind <> 'proposed_belief';
DROP TABLE canon_reviews;
ALTER TABLE canon_reviews_old RENAME TO canon_reviews;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
