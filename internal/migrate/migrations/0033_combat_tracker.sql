-- +goose Up
-- The combat tracker (MAD-422, stage 5 of MAD-417): the whole battle as
-- one state machine. Every combatant is the same kind of thing — pcs from
-- their sheets, monsters and companions from the statblock machinery —
-- over a persistent encounter state: round, turn, whose action it is.
--
-- Three tables. combats is the battle itself: its status, the round and
-- turn the table sits on, and the links out (the planned encounter it
-- came from, the session whose log it streams into). One active combat
-- per campaign — the partial unique index enforces it at the write, the
-- same one-live-session shape the dice store resolves against.
-- combatants is the merged order: a snapshot of the numbers the fight
-- needs (AC, max HP, resistances, legendary budget, recharge abilities)
-- frozen at combat start — the sheet and the statblock are definitions,
-- tonight's numbers are these rows — plus the mid-play state (hp, temp
-- hp, max-hp reduction, death saves, the downed/stable/dead flags,
-- reaction, legendary uses) that every change flows through.
-- combat_log is the append-only journal under it all: one row per
-- mechanical event, per-combat seq assigned atomically in the INSERT (the
-- session_events pattern), each carrying the payload that mirrors into
-- the session log. Combat is the densest stream of mechanical events in
-- the game; the journal is what makes the state re-runnable and the
-- session export a replay rather than a summary.
--
-- A combat event is a new kind of session event, and session_events.kind
-- is a CHECK — SQLite cannot widen a CHECK in place, so this is the same
-- twelve-step rebuild 0031 used: foreign_keys comes OFF, the replacement
-- is built under a new name, the original drops with its triggers and
-- index, the new one takes the name — every surviving REFERENCES clause
-- in the schema still spells "session_events" and is valid again — and
-- foreign_keys comes back ON.

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE session_events_new (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES game_sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('qa','ruling','note','discovery','encounter','roll','combat')),
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

CREATE TABLE combats (
    id           TEXT PRIMARY KEY,
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    encounter_id TEXT REFERENCES encounters(id) ON DELETE SET NULL,
    session_id   TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    name         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','ended')),
    end_reason   TEXT NOT NULL DEFAULT '',
    round        INTEGER NOT NULL DEFAULT 1,
    turn_index   INTEGER NOT NULL DEFAULT 0,
    actor        TEXT NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    ended_at     INTEGER,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- One battle at a time: the tracker's whole model is "the current fight",
-- and a second active combat in one campaign is a mistake, not a feature.
CREATE UNIQUE INDEX combats_one_active ON combats(campaign_id) WHERE status = 'active';

CREATE TABLE combatants (
    id              TEXT PRIMARY KEY,
    combat_id       TEXT NOT NULL REFERENCES combats(id) ON DELETE CASCADE,
    entity_id       TEXT REFERENCES entities(id) ON DELETE SET NULL,
    name            TEXT NOT NULL,
    side            TEXT NOT NULL CHECK (side IN ('party','foe')),
    kind            TEXT NOT NULL CHECK (kind IN ('pc','monster','companion')),
    statblock       TEXT NOT NULL DEFAULT '{}',
    initiative      INTEGER NOT NULL DEFAULT 0,
    init_bonus      INTEGER NOT NULL DEFAULT 0,
    init_formula    TEXT NOT NULL DEFAULT '',
    ac              INTEGER NOT NULL DEFAULT 0,
    max_hp          INTEGER NOT NULL DEFAULT 0,
    hp_reduction    INTEGER NOT NULL DEFAULT 0,
    hp              INTEGER NOT NULL DEFAULT 0,
    temp_hp         INTEGER NOT NULL DEFAULT 0,
    downed          INTEGER NOT NULL DEFAULT 0 CHECK (downed IN (0,1)),
    stable          INTEGER NOT NULL DEFAULT 0 CHECK (stable IN (0,1)),
    dead            INTEGER NOT NULL DEFAULT 0 CHECK (dead IN (0,1)),
    death_successes INTEGER NOT NULL DEFAULT 0,
    death_failures  INTEGER NOT NULL DEFAULT 0,
    reaction_spent  INTEGER NOT NULL DEFAULT 0 CHECK (reaction_spent IN (0,1)),
    legendary_used  INTEGER NOT NULL DEFAULT 0,
    conditions      TEXT NOT NULL DEFAULT '[]',
    position        INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX combatants_combat ON combatants(combat_id, position);
CREATE INDEX combatants_entity ON combatants(entity_id) WHERE entity_id IS NOT NULL;

CREATE TABLE combat_log (
    id               TEXT PRIMARY KEY,
    combat_id        TEXT NOT NULL REFERENCES combats(id) ON DELETE CASCADE,
    seq              INTEGER NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('start','turn','round','damage','heal','temp_hp','death_save','condition','condition_end','legendary','reaction','recharge','end')),
    combatant_id     TEXT REFERENCES combatants(id) ON DELETE SET NULL,
    amount           INTEGER NOT NULL DEFAULT 0,
    note             TEXT NOT NULL DEFAULT '',
    payload          TEXT NOT NULL DEFAULT '{}',
    actor            TEXT NOT NULL DEFAULT '',
    session_event_id TEXT REFERENCES session_events(id) ON DELETE SET NULL,
    created_at       INTEGER NOT NULL,
    UNIQUE (combat_id, seq)
);

CREATE INDEX combat_log_combat ON combat_log(combat_id, seq);

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
-- The rule 0030 set: rolling back is an operator-owned decision. The
-- battles go; the sheets, statblocks, effects and session logs they hung
-- from stay. The session_events CHECK narrows back the same twelve-step
-- way it widened.
DROP TABLE combat_log;
DROP TABLE combatants;
DROP TABLE combats;

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE session_events_old (
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
INSERT INTO session_events_old (id, session_id, seq, kind, summary, detail, payload, created_at)
    SELECT id, session_id, seq, kind, summary, detail, payload, created_at
      FROM session_events
     WHERE kind <> 'combat';
DROP TABLE session_events;
ALTER TABLE session_events_old RENAME TO session_events;
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

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
