-- +goose Up
-- +goose NO TRANSACTION
-- Dice with provenance (MAD-420, stage 3 of MAD-417): every roll is a row.
-- The formula, the natural dice, the modifiers, the total, who rolled and
-- what for — plus a visibility. A public roll lands in the shared party
-- feed; a secret roll is DM-only, and its absence from player-scoped reads
-- is enforced in the query, not in the UI.
--
-- The RNG is counter-based: each campaign owns a seed (dice_seeds), each
-- roll's per-campaign seq doubles as the nonce, so (seed, nonce, formula)
-- reproduces the dice exactly — the property the golden files pin and
-- Stage 9's replay will stand on. The rolled values are stored too: the
-- feed must render without re-deriving, and the stored bytes ARE the
-- replay's oracle.
--
-- A roll is a new kind of session event, and session_events.kind is a
-- CHECK — SQLite cannot widen a CHECK in place, so this is SQLite's own
-- twelve-step rebuild, not the rename dance 0019 and 0030 used: a rename
-- would rewrite resource_transactions' REFERENCES clause into the grave
-- and the old table's drop would fire the ruling_fts triggers. Instead
-- foreign_keys comes OFF (safe here: the whole app holds ONE connection,
-- and NO TRANSACTION keeps every statement of this migration on it), the
-- replacement is built under a new name, the original drops with its
-- triggers and index, the new one takes the name — every surviving
-- REFERENCES clause in the schema still spells "session_events" and is
-- valid again — and foreign_keys comes back ON.

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE session_events_new (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES game_sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('qa','ruling','note','discovery','encounter','roll')),
    summary    TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    UNIQUE (session_id, seq)
);
INSERT INTO session_events_new (id, session_id, seq, kind, summary, detail, payload, created_at)
    SELECT id, session_id, seq, kind, summary, detail, payload, created_at
      FROM session_events;
DROP TABLE session_events;
ALTER TABLE session_events_new RENAME TO session_events;
CREATE INDEX session_events_session ON session_events(session_id, seq);

-- +goose StatementBegin
CREATE TRIGGER ruling_fts_ins AFTER INSERT ON session_events BEGIN
    INSERT INTO ruling_fts (body, campaign_id, session_id, event_id)
    SELECT NEW.summary || ' ' || NEW.detail, gs.campaign_id, NEW.session_id, NEW.id
      FROM game_sessions gs
     WHERE gs.id = NEW.session_id
       AND NEW.kind IN ('ruling', 'qa');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER ruling_fts_upd AFTER UPDATE ON session_events BEGIN
    DELETE FROM ruling_fts WHERE event_id = NEW.id;
    INSERT INTO ruling_fts (body, campaign_id, session_id, event_id)
    SELECT NEW.summary || ' ' || NEW.detail, gs.campaign_id, NEW.session_id, NEW.id
      FROM game_sessions gs
     WHERE gs.id = NEW.session_id
       AND NEW.kind IN ('ruling', 'qa');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER ruling_fts_del AFTER DELETE ON session_events BEGIN
    DELETE FROM ruling_fts WHERE event_id = OLD.id;
END;
-- +goose StatementEnd

CREATE TABLE IF NOT EXISTS dice_seeds (
    campaign_id TEXT PRIMARY KEY REFERENCES campaigns(id) ON DELETE CASCADE,
    seed        INTEGER NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS dice_rolls (
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

CREATE INDEX IF NOT EXISTS dice_rolls_feed ON dice_rolls(campaign_id, seq);

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS dice_rolls;
DROP TABLE IF EXISTS dice_seeds;
