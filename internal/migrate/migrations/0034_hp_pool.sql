-- +goose Up
-- Hit points join the pool grammar (MAD-423, stage 6 of MAD-417).
--
-- The ledger's one grammar already covers slots, hit dice, features,
-- items and currency — but not the number the party board leads with.
-- Current HP lived only on combat rows (MAD-422), so between fights
-- nothing could answer "how hurt is the wizard": the sheet holds max HP,
-- the tracker holds tonight's fight, and no row holds the truth in
-- between. One kind fixes it: an hp pool sized to the sheet's max HP,
-- recovery manual — the 2014 long rest does not heal, it returns hit
-- dice — where damage and healing are transactions like every other
-- change. The combat tracker keeps its in-fight rows as the fast state
-- and reconciles into this pool when a battle ends; the board reads
-- combat while one runs, the ledger otherwise.
--
-- resource_pools.kind is a CHECK and SQLite cannot widen a CHECK in
-- place, so this is the rebuild 0030 performed on clock_advances and
-- 0033 on session_events: foreign_keys OFF, the replacement built under
-- a new name, the original dropped, the new one renamed into place —
-- resource_transactions' REFERENCES still spells "resource_pools" and is
-- valid again — foreign_keys back ON. No data changes; existing pools
-- copy across byte for byte.

-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE resource_pools_new (
    id           TEXT PRIMARY KEY,
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    entity_id    TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('hp','slot','hit_dice','feature','item','currency')),
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

INSERT INTO resource_pools_new (id, campaign_id, entity_id, kind, name, label, size,
    recovery, granularity, source, created_at, updated_at)
    SELECT id, campaign_id, entity_id, kind, name, label, size,
           recovery, granularity, source, created_at, updated_at
      FROM resource_pools;

DROP TABLE resource_pools;
ALTER TABLE resource_pools_new RENAME TO resource_pools;

CREATE INDEX resource_pools_entity ON resource_pools(entity_id);

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
-- The same rebuild with the 0030 kind list. An hp pool's rows are
-- dropped (a down migration is an operator-owned decision; the pools
-- re-derive from the sheets at the next sync anyway).
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
-- +goose StatementEnd

CREATE TABLE resource_pools_old (
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

INSERT INTO resource_pools_old (id, campaign_id, entity_id, kind, name, label, size,
    recovery, granularity, source, created_at, updated_at)
    SELECT id, campaign_id, entity_id, kind, name, label, size,
           recovery, granularity, source, created_at, updated_at
      FROM resource_pools
     WHERE kind != 'hp';

DROP TABLE resource_pools;
ALTER TABLE resource_pools_old RENAME TO resource_pools;

CREATE INDEX resource_pools_entity ON resource_pools(entity_id);

-- +goose StatementBegin
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
