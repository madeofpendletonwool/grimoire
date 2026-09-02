-- +goose Up
-- Rumours with truth values, and who is repeating them (MAD-374, stage 5.2
-- of MAD-316). A rumour is a statement in circulation plus a truth value
-- the DM holds and nobody else ever reads: facts.confidence has no slot for
-- "untrue" (0002), and a false rumour has no fact to hang awareness on —
-- so rumours live in their own tables and reach into the knowledge layer
-- only through the heard path, which writes stances through the same
-- transition table every other stance write runs through.
--
-- This issue's note on migration numbers: it claimed 0018 when main was at
-- 0011, but 0012–0023 landed first — main is at 0023 as this branch starts
-- — so this takes the next genuinely free number, the rebase rule the
-- issue set for itself.
--
-- truth is DM-only, always. A player-scope read returns the statement, the
-- variant and who said it, and never the column: internal/knowledge
-- enforces it in the SQL itself (ADR 2), and the reflection leak test
-- asserts it (ADR 6 layer 2). dm_only exists for the rumour the DM plants
-- for their own eyes — a rumour marked DM-only never reaches a player
-- scope at all, truth column or no.
--
-- fact_id is the canon fact the rumour attests (when true) or the one it
-- distorts (when distorted), and is null for a rumour invented whole. A
-- rumour attached to a secret fact is a clue path — the cheapest fix for
-- an unfindable secret, which is what checkUnreachableSecret scores.

CREATE TABLE rumors (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    statement   TEXT NOT NULL,
    truth       TEXT NOT NULL CHECK (truth IN ('true','false','distorted')),
    about_entity TEXT,
    fact_id     TEXT REFERENCES facts(id) ON DELETE SET NULL,
    origin      TEXT NOT NULL DEFAULT '',
    spread      TEXT NOT NULL DEFAULT 'local' CHECK (spread IN ('local','regional','widespread')),
    status      TEXT NOT NULL DEFAULT 'circulating'
                  CHECK (status IN ('circulating','debunked','confirmed','dormant')),
    dm_only     INTEGER NOT NULL DEFAULT 0,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE INDEX rumors_campaign ON rumors(campaign_id, status);
CREATE INDEX rumors_about ON rumors(about_entity);
CREATE INDEX rumors_fact ON rumors(fact_id);

-- Who is repeating it, in their own words. variant is the drifted wording
-- this NPC repeats — the whole charm of a rumour, for one column.
-- since_event is the timeline event that first put the words in their
-- mouth, the same optional provenance awareness.since_event carries.
--
-- entity_id is an entity id or the literal 'party' (a fact-less false
-- rumour a knower hears renders as a holding here, because awareness's
-- primary key is (campaign_id, knower, fact_id) with a real foreign key to
-- facts — making the target polymorphic would touch every scoped query and
-- both leak tests to buy one column, a limit documented rather than
-- engineered around). A foreign key cannot express "entity or this one
-- magic string", so the Go layer validates it — the exact precedent
-- awareness.knower set in 0003.
CREATE TABLE rumor_holders (
    rumor_id    TEXT NOT NULL REFERENCES rumors(id) ON DELETE CASCADE,
    entity_id   TEXT NOT NULL,
    variant     TEXT NOT NULL DEFAULT '',
    since_event TEXT REFERENCES events(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (rumor_id, entity_id)
);

CREATE INDEX rumor_holders_entity ON rumor_holders(entity_id);

-- Generated rumour batches stage one proposed_rumor item per rumour
-- (canon.BatchSourceRumor). The kinds CHECK cannot widen in place — the
-- same SQLite rule 0012 documented — so canon_reviews is rebuilt once
-- here, the way 0007, 0009, 0011 and 0012 did it: rename, recreate with
-- the widened kind list, copy back, drop. Every existing row, decided or
-- open, keeps its id, dedup key and decision.
ALTER TABLE canon_reviews RENAME TO canon_reviews_old;
CREATE TABLE canon_reviews (
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
-- Rebuild canon_reviews with the pre-rumour kind list, then drop the
-- rumour tables. Decided rumour items lose their applied rumours with the
-- tables — a rollback is an operator-owned decision the same rule 0012,
-- 0022 and 0023 set.
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
      FROM canon_reviews_old
     WHERE kind <> 'proposed_rumor';
DROP TABLE canon_reviews_old;
CREATE INDEX canon_reviews_campaign ON canon_reviews(campaign_id, status);
CREATE INDEX canon_reviews_candidate ON canon_reviews(candidate_id);
CREATE INDEX canon_reviews_batch ON canon_reviews(batch_id);
DROP TABLE rumor_holders;
DROP TABLE rumors;
