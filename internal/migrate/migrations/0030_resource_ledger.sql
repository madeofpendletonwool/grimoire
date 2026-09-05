-- +goose Up
-- The resource ledger (MAD-419, stage 2 of MAD-417): pools, transactions,
-- and rests. The sheet (MAD-418) is the definition; these tables are the
-- state. Every change is an append-only transaction linked to the session
-- event and the actor that caused it; current values are derived by folding
-- the log (internal/ledger), never stored — the same rule that makes a sim
-- tick re-runnable makes "why is the wizard out of 3rd levels?" answerable.

-- A rest needs its own reason on the clock ledger. clock_advances.reason
-- is a CHECK, and SQLite cannot widen a CHECK in place, so this is the same
-- rebuild 0019 did for canon_reviews: rename, recreate with the widened
-- list, copy back, drop. Every existing advance keeps its row. The rebuild
-- runs BEFORE anything here references clock_advances: a rename rewrites
-- the REFERENCES clauses of pointing tables, and the new rests table below
-- must point at the rebuilt one, not follow the rename into the grave.
ALTER TABLE clock_advances RENAME TO clock_advances_old;
CREATE TABLE clock_advances (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    from_day    INTEGER NOT NULL,
    to_day      INTEGER NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN ('travel','downtime','session','tick','rest','manual')),
    note        TEXT NOT NULL DEFAULT '',
    session_id  TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
INSERT INTO clock_advances (id, campaign_id, from_day, to_day, reason, note, session_id, created_by, created_at)
    SELECT id, campaign_id, from_day, to_day, reason, note, session_id, created_by, created_at
      FROM clock_advances_old;
DROP TABLE clock_advances_old;
CREATE INDEX clock_advances_campaign ON clock_advances(campaign_id, created_at);
CREATE INDEX clock_advances_session ON clock_advances(session_id);

-- Pool definitions. Rows with source 'sheet' are derived from the pc's
-- typed sheet and re-synced on every sheet write (derivation, not a second
-- source of truth); rows with source 'dm' are the DM's own registrations —
-- ki points, rage, ammunition — the grammar covers them unchanged and the
-- sheet holds no numbers for them. recovery is the one grammar that decides
-- what a rest resets: short | long | dawn | manual. No feature gets a
-- special case; pact magic is simply a slot pool with recovery short.
CREATE TABLE resource_pools (
    id           TEXT PRIMARY KEY,
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    entity_id    TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('slot','hit_dice','feature','item','currency')),
    name         TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL CHECK (size >= 0),
    recovery     TEXT NOT NULL CHECK (recovery IN ('short','long','dawn','manual')),
    granularity  INTEGER NOT NULL DEFAULT 1 CHECK (granularity >= 1),
    source       TEXT NOT NULL DEFAULT 'sheet' CHECK (source IN ('sheet','dm')),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (campaign_id, entity_id, kind, name)
);

CREATE INDEX resource_pools_entity ON resource_pools(entity_id);

-- One row per rest. source 'dm' is the live button — executed immediately,
-- no gate; source 'model' is a machine proposal staged behind the review
-- gate (canon batch source 'rest'), decided like every other batch. The
-- long rest's clock advance is recorded here (advance_id / clock_from /
-- clock_to): the enemies move while the party sleeps. status is
-- applied | staged | discarded; plan carries a staged rest's computed
-- transactions so the finalizer applies exactly what was reviewed.
CREATE TABLE rests (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('short','long')),
    source      TEXT NOT NULL DEFAULT 'dm' CHECK (source IN ('dm','model')),
    status      TEXT NOT NULL DEFAULT 'applied' CHECK (status IN ('applied','staged','discarded')),
    plan        TEXT NOT NULL DEFAULT '[]',
    batch_id    TEXT REFERENCES proposal_batches(id) ON DELETE SET NULL,
    actor       TEXT NOT NULL DEFAULT '',
    session_id  TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    advance_id  TEXT REFERENCES clock_advances(id) ON DELETE SET NULL,
    clock_from  INTEGER,
    clock_to    INTEGER,
    note        TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

CREATE INDEX rests_campaign ON rests(campaign_id, created_at);
CREATE INDEX rests_batch ON rests(batch_id);

-- The transaction log. kind is spend | regain | set | reset: set is a DM
-- correction to an absolute value; reset is a rest's grammar-driven refill
-- (amount records the size it reset to). rest_id and session_event_id link
-- the change to what caused it; actor is the user id that wrote it. The
-- derived balance is this log folded in rowid order — never a column. pool
-- (kind:name) is the pool's identity independent of its row id, so history
-- stays readable even after the definition row goes; pool_id follows it for
-- joins while the definition lives.
CREATE TABLE resource_transactions (
    id               TEXT PRIMARY KEY,
    campaign_id      TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    entity_id        TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    pool_id          TEXT REFERENCES resource_pools(id) ON DELETE SET NULL,
    pool             TEXT NOT NULL,
    kind             TEXT NOT NULL CHECK (kind IN ('spend','regain','set','reset')),
    amount           INTEGER NOT NULL,
    rest_id          TEXT REFERENCES rests(id) ON DELETE CASCADE,
    session_event_id TEXT REFERENCES session_events(id) ON DELETE SET NULL,
    session_id       TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    actor            TEXT NOT NULL DEFAULT '',
    note             TEXT NOT NULL DEFAULT '',
    clock_day        INTEGER NOT NULL,
    created_at       INTEGER NOT NULL
);

CREATE INDEX resource_transactions_pool ON resource_transactions(pool_id);
CREATE INDEX resource_transactions_rest ON resource_transactions(rest_id);
CREATE INDEX resource_transactions_entity ON resource_transactions(entity_id);

-- +goose Down
-- Rolling back is an operator-owned decision (`grimoire migrate down`), the
-- same rule 0016 and 0019 set: the tables go, the sheets and the campaign
-- rows they hang from stay. The clock_advances rebuild is reversed with the
-- same rebuild, back to the 0016 reason list.
DROP TABLE rests;
DROP TABLE resource_transactions;
DROP TABLE resource_pools;
ALTER TABLE clock_advances RENAME TO clock_advances_old;
CREATE TABLE clock_advances (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    from_day    INTEGER NOT NULL,
    to_day      INTEGER NOT NULL,
    reason      TEXT NOT NULL CHECK (reason IN ('travel','downtime','session','tick','manual')),
    note        TEXT NOT NULL DEFAULT '',
    session_id  TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);
INSERT INTO clock_advances (id, campaign_id, from_day, to_day, reason, note, session_id, created_by, created_at)
    SELECT id, campaign_id, from_day, to_day,
           CASE reason WHEN 'rest' THEN 'manual' ELSE reason END,
           note, session_id, created_by, created_at
      FROM clock_advances_old;
DROP TABLE clock_advances_old;
CREATE INDEX clock_advances_campaign ON clock_advances(campaign_id, created_at);
CREATE INDEX clock_advances_session ON clock_advances(session_id);
