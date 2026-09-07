-- +goose Up
-- The table screen (MAD-425, stage 8 of MAD-417): one URL, cast to the
-- TV or projected — the public surface of the same state the party
-- board serves, with nothing a player could not already see.
--
-- Three moves. combatants gains a reveal column: the DM's per-monster
-- choice of what the room may read — nothing (the default), exact hit
-- points, or the table's health word. It is per combatant, so it dies
-- with the battle it belonged to, and it is presentation, not
-- mechanics: it changes no number the fight keeps.
--
-- combat_log's kind is a CHECK and SQLite cannot widen a CHECK in
-- place, so the journal gets the rebuild 0033 performed on
-- session_events and 0034 on resource_pools — nothing references
-- combat_log and it carries no triggers, so the swap is the plain
-- shape: build under a new name, copy, drop, rename, re-index. The
-- reveal lands in the journal because the journal is where every DM
-- act on the battle already lands; the session mirror needs no
-- change (its kind is 'combat').
--
-- table_screens is the share-page model applied to a live surface: a
-- 128-bit token from crypto/rand is the whole access model, minted and
-- revoked by the DM, never carrying a sequential id. Unlike a share
-- the screen is a reference, not a snapshot — it resolves to the
-- campaign and re-derives state on every read, which is what makes it
-- live. last_seen_at is the projector's heartbeat, best-effort.

ALTER TABLE combatants ADD COLUMN reveal TEXT NOT NULL DEFAULT '' CHECK (reveal IN ('','hp','word'));

CREATE TABLE combat_log_new (
    id               TEXT PRIMARY KEY,
    combat_id        TEXT NOT NULL REFERENCES combats(id) ON DELETE CASCADE,
    seq              INTEGER NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('start','turn','round','damage','heal','temp_hp','death_save','condition','condition_end','legendary','reaction','recharge','reveal','end')),
    combatant_id     TEXT REFERENCES combatants(id) ON DELETE SET NULL,
    amount           INTEGER NOT NULL DEFAULT 0,
    note             TEXT NOT NULL DEFAULT '',
    payload          TEXT NOT NULL DEFAULT '{}',
    actor            TEXT NOT NULL DEFAULT '',
    session_event_id TEXT REFERENCES session_events(id) ON DELETE SET NULL,
    created_at       INTEGER NOT NULL,
    UNIQUE (combat_id, seq)
);

INSERT INTO combat_log_new (id, combat_id, seq, kind, combatant_id, amount, note, payload, actor, session_event_id, created_at)
    SELECT id, combat_id, seq, kind, combatant_id, amount, note, payload, actor, session_event_id, created_at
      FROM combat_log;

DROP TABLE combat_log;
ALTER TABLE combat_log_new RENAME TO combat_log;

CREATE INDEX combat_log_combat ON combat_log(combat_id, seq);

CREATE TABLE table_screens (
    token        TEXT PRIMARY KEY,
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    created_by   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    revoked_at   INTEGER,
    last_seen_at INTEGER
);

CREATE INDEX table_screens_campaign ON table_screens(campaign_id, created_at DESC);

-- +goose Down
-- Rolling back is an operator-owned decision: the screens go, the
-- reveal choices go, the battles and their journals stay (minus their
-- reveal rows, which the narrowed CHECK drops).
DROP TABLE table_screens;

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE combat_log_old (
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

INSERT INTO combat_log_old (id, combat_id, seq, kind, combatant_id, amount, note, payload, actor, session_event_id, created_at)
    SELECT id, combat_id, seq, kind, combatant_id, amount, note, payload, actor, session_event_id, created_at
      FROM combat_log
     WHERE kind != 'reveal';

DROP TABLE combat_log;
ALTER TABLE combat_log_old RENAME TO combat_log;

CREATE INDEX combat_log_combat ON combat_log(combat_id, seq);

-- SQLite cannot drop a column before 3.35, and the combatants table
-- predates that guarantee nowhere — so the down path rebuilds it
-- without the reveal column, the same swap as above.
CREATE TABLE combatants_old (
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

INSERT INTO combatants_old (id, combat_id, entity_id, name, side, kind, statblock,
    initiative, init_bonus, init_formula, ac, max_hp, hp_reduction, hp, temp_hp,
    downed, stable, dead, death_successes, death_failures, reaction_spent,
    legendary_used, conditions, position, created_at, updated_at)
    SELECT id, combat_id, entity_id, name, side, kind, statblock,
           initiative, init_bonus, init_formula, ac, max_hp, hp_reduction, hp, temp_hp,
           downed, stable, dead, death_successes, death_failures, reaction_spent,
           legendary_used, conditions, position, created_at, updated_at
      FROM combatants;

DROP TABLE combatants;
ALTER TABLE combatants_old RENAME TO combatants;

CREATE INDEX combatants_combat ON combatants(combat_id, position);
CREATE INDEX combatants_entity ON combatants(entity_id) WHERE entity_id IS NOT NULL;

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
