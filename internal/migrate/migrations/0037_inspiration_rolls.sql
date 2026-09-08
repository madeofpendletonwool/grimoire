-- +goose Up
-- Table meta (MAD-428, stage 11 of MAD-417): inspiration joins the dice.
--
-- Inspiration is a first-class 5e pool (2014 rules: the DM awards it, a
-- character holds at most one, and spending it grants advantage on one
-- attack roll, saving throw or ability check). The pool itself is ledger
-- grammar — kind feature, name inspiration, size 1, recovery manual —
-- registered the moment the DM first awards it, resting and tracking with
-- zero special cases. What the ledger cannot carry is the mark on the roll
-- itself: a roll thrown with inspiration spent is a fact with provenance,
-- so the row says so. The feed renders it, the export prints it, and the
-- campaign stats count it — one truth, read three ways.
--
-- A plain ADD COLUMN passes the table's first-declared order (0009's
-- precedent): dice_rolls owns no inbound references, so nothing else in
-- the schema has to move.

ALTER TABLE dice_rolls ADD COLUMN inspiration INTEGER NOT NULL DEFAULT 0 CHECK (inspiration IN (0,1));

-- +goose Down
-- Rolling back is an operator-owned decision: the mark goes, the rolls
-- stay. dice_rolls has no inbound references, so the rebuild is a plain
-- copy minus the column.
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE dice_rolls_old (
    id                TEXT PRIMARY KEY,
    campaign_id       TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    seq               INTEGER NOT NULL,
    session_id        TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    session_event_id  TEXT REFERENCES session_events(id) ON DELETE SET NULL,
    actor             TEXT NOT NULL DEFAULT '',
    character_id      TEXT REFERENCES entities(id) ON DELETE SET NULL,
    character_name    TEXT NOT NULL DEFAULT '',
    context_kind      TEXT NOT NULL DEFAULT 'other' CHECK (context_kind IN ('attack','save','check','damage','initiative','table','other')),
    detail            TEXT NOT NULL DEFAULT '',
    target_id         TEXT,
    target_name       TEXT NOT NULL DEFAULT '',
    formula           TEXT NOT NULL,
    mode              TEXT NOT NULL DEFAULT '' CHECK (mode IN ('','advantage','disadvantage')),
    dice              TEXT NOT NULL DEFAULT '[]',
    modifier          INTEGER NOT NULL DEFAULT 0,
    total             INTEGER NOT NULL DEFAULT 0,
    seed              INTEGER NOT NULL DEFAULT 0,
    nonce             INTEGER NOT NULL DEFAULT 0,
    visibility        TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','secret')),
    created_at        INTEGER NOT NULL,
    UNIQUE (campaign_id, seq)
);

INSERT INTO dice_rolls_old (id, campaign_id, seq, session_id, session_event_id, actor,
    character_id, character_name, context_kind, detail, target_id, target_name,
    formula, mode, dice, modifier, total, seed, nonce, visibility, created_at)
    SELECT id, campaign_id, seq, session_id, session_event_id, actor,
           character_id, character_name, context_kind, detail, target_id, target_name,
           formula, mode, dice, modifier, total, seed, nonce, visibility, created_at
      FROM dice_rolls;

DROP TABLE dice_rolls;
ALTER TABLE dice_rolls_old RENAME TO dice_rolls;

CREATE INDEX dice_rolls_feed ON dice_rolls(campaign_id, seq);

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
