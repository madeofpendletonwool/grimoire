-- +goose Up
-- Proposal batches (MAD-359): multi-object AI proposals through the one
-- review gate. Every stage-5 generator produces many linked graph objects at
-- once (a campaign skeleton is factions + a secret + hooks + the edges
-- between them), and ADR 3 says none of it becomes canon without a human
-- decision. A batch is that decision's unit: one generator run, many
-- proposed_* items, one DecideBatch call.
--
-- proposal_batches carries the run's identity (which generator, from what
-- prompt, by whom) and its aggregate status. source is a plain string
-- column, deliberately NOT a widened CHECK: SQLite cannot widen a CHECK in
-- place, and 0011 already had to rebuild canon_reviews once to add one kind.
-- New generators name themselves without a migration.
--
-- canon_reviews gains batch_id and depends_on — one rebuild here, covering
-- all of stage 5. The existing proposed_* kinds are reused verbatim: a
-- proposed NPC from a designer is the same kind of thing as a proposed NPC
-- from extraction and renders on the same screen. depends_on is a JSON
-- array of sibling review ids, and its graph is validated in the store
-- (cycle-free, references resolvable) before anything is written.

CREATE TABLE proposal_batches (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    source      TEXT NOT NULL,
    prompt      TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','accepted','partially_accepted','dismissed')),
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    decided_at  INTEGER
);
CREATE INDEX proposal_batches_campaign ON proposal_batches(campaign_id, status);

-- Rebuild canon_reviews the way 0007, 0009 and 0011 do: rename, recreate
-- with the new columns, copy back, drop. Nothing references canon_reviews,
-- so the rebuild cascades nothing. All existing rows, decided or open, keep
-- their ids, dedup keys and decisions.
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
           NULL, '[]', subject, summary, detail, result_ref,
           decision_note, decided_by, decided_at, created_at, updated_at
      FROM canon_reviews_old;
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);

-- +goose Down
-- Rebuild canon_reviews without the batch columns (batch items detach from
-- their batches and keep their decisions), then drop the batches. The
-- kind list is unchanged from 0011: batches added columns, not kinds.
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
DROP TABLE proposal_batches;
