-- +goose Up
-- The canon engine's review queue (MAD-310): the human gate. Nothing the AI
-- proposes becomes canon until a person says so. canon_reviews is the queue
-- the post-session review screen reads; a decision here is the only path that
-- writes accepted material into the campaign graph.
--
-- Item kinds:
--   proposed_fact / proposed_event / proposed_discovery /
--   proposed_relationship / proposed_entity
--       staged candidates that survived the adversarial pass (an applied
--       'agree' or 'downgrade' verdict);
--   low_agreement  the adversarial checker could not decide (flag_review or
--                  an unparseable validator response);
--   contradiction  two credible sources disagree;
--   engine_flag    a review-severity deterministic finding from the flag
--                  ledger (canon_flags).
--
-- Queue semantics (ported from Arda's factcheck_reviews, kept exactly):
--   * a decided item keeps its decision forever;
--   * a decided item never resurrects;
--   * if the same finding reappears later it opens as a NEW item.
-- The UNIQUE (campaign_id, dedup_key) index is what makes a re-run of the
-- queue builder a no-op for findings that already have an item (open OR
-- decided) and forces genuinely new findings to mint a new row.
--
-- fact_provenance grows accepted_by / accepted_at so "who accepted this
-- extracted fact, and when" is recorded where the fact's origin is recorded,
-- exactly as discoveries.already carries its accepted_by / accepted_at.
-- (SQLite 3.35+ supports ALTER TABLE ... ADD COLUMN; the modernc driver used
-- by this repo is 3.56.)

ALTER TABLE fact_provenance ADD COLUMN accepted_by TEXT NOT NULL DEFAULT '';
ALTER TABLE fact_provenance ADD COLUMN accepted_at INTEGER;

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
    -- The rendered headline (subject) and body (detail) of the item, so the
    -- review screen needs no joins to draw a queue entry.
    subject       TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '',
    -- result_ref is the graph object id an accept/modify wrote (fact id,
    -- event id, discovery id, relationship id, entity id). It makes a
    -- discovery's local fact reference resolvable after its fact was accepted.
    result_ref    TEXT NOT NULL DEFAULT '',
    decision_note TEXT NOT NULL DEFAULT '',
    decided_by    TEXT NOT NULL DEFAULT '',
    decided_at    INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (campaign_id, dedup_key)
);

CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);

-- +goose Down
-- Rolling the queue back also drops the acceptance columns from
-- fact_provenance. SQLite cannot drop a column the way it adds one, so the
-- table is rebuilt without them — the same mechanics 0007 uses for the runs
-- table. fact_provenance is referenced by nothing (it references facts), so
-- the rebuild cascades nothing.
DROP TABLE canon_reviews;

ALTER TABLE fact_provenance RENAME TO fact_provenance_old;
CREATE TABLE fact_provenance (
    id         TEXT PRIMARY KEY,
    fact_id    TEXT NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    session_id TEXT,
    source_id  TEXT,
    span_start INTEGER,
    span_end   INTEGER,
    quote      TEXT NOT NULL DEFAULT '',
    method     TEXT NOT NULL CHECK (method IN ('dm_authored','ai_proposed','extracted','imported')),
    created_at INTEGER NOT NULL
);
INSERT INTO fact_provenance (id, fact_id, session_id, source_id, span_start, span_end, quote, method, created_at)
    SELECT id, fact_id, session_id, source_id, span_start, span_end, quote, method, created_at
      FROM fact_provenance_old;
DROP TABLE fact_provenance_old;
CREATE INDEX fact_provenance_fact ON fact_provenance(fact_id);
