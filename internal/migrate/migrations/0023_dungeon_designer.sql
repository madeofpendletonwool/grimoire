-- +goose Up
-- The dungeon designer (MAD-373, stage 5.2 of MAD-316): a dungeon lives
-- in its own tables and touches the campaign graph only on request, the
-- same separation the quest machine carries. The seeded room graph is
-- computed by internal/dungeon (pure: no DB, no network, no clock) and
-- persisted here; the dressing pass names rooms it was handed, and
-- placing a dungeon stages a proposal batch through MAD-359's gate.
--
-- This issue's note on migration numbers: it claimed 0017 when main was
-- at 0011, but MAD-359/360 (0012/0013), proposal batches' siblings and
-- every stage-5 designer since landed first — main is at 0022 as this
-- branch starts — so this takes the next genuinely free number, the
-- rebase rule the issue set for itself.

-- The dungeon record itself. params is the JSON internal/dungeon.Layout
-- computed the graph from — same seed and params, same dungeon, forever;
-- re-rolling means a new row with a new seed, never a rewrite.
-- location_entity is null until the dungeon is placed in the world
-- (POST .../dungeons/{did}/place through the review gate). key_item is
-- the dressing pass's name for the item the boss's locked door needs —
-- the id lands on the edge's key_item_entity when the placing batch's
-- item entity applies. secret is the dressing pass's statement of what
-- the dungeon hides — the facts row the placing batch stages.
CREATE TABLE dungeons (
    id                TEXT PRIMARY KEY,
    campaign_id       TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    theme             TEXT NOT NULL DEFAULT '',
    size              TEXT NOT NULL CHECK (size IN ('delve','lair','complex','megadungeon')),
    level             INTEGER NOT NULL CHECK (level BETWEEN 1 AND 20),
    expected_sessions INTEGER NOT NULL CHECK (expected_sessions BETWEEN 1 AND 10),
    seed              INTEGER NOT NULL,
    params            TEXT NOT NULL,
    key_item          TEXT NOT NULL DEFAULT '',
    secret            TEXT NOT NULL DEFAULT '',
    boss_name         TEXT NOT NULL DEFAULT '',
    location_entity   TEXT,
    status            TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','dressed','placed')),
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

CREATE INDEX dungeons_campaign ON dungeons(campaign_id);

-- The rooms: one row per room of the computed graph, grid cell and all —
-- the map is a rendering of these rows, and dragging a room writes x and
-- y back rather than re-rolling anything.
--
-- encounter_id deliberately carries no foreign key: encounters is
-- owner-scoped, not campaign-scoped (0001_baseline.sql), and making it
-- campaign-aware is MAD-317's. The precedent for a deliberately soft
-- column is the session_id columns in 0002 and 0003, which waited for
-- the layer that owned them. entity_id is null until the dungeon is
-- placed; a key is stable for the dungeon's life (r1, r2, ...).
CREATE TABLE dungeon_rooms (
    id           TEXT PRIMARY KEY,
    dungeon_id   TEXT NOT NULL REFERENCES dungeons(id) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    purpose      TEXT NOT NULL CHECK (purpose IN (
                     'entrance','guard','hazard','puzzle','treasure','shrine',
                     'prison','lair','boss','secret','connector')),
    detail       TEXT NOT NULL DEFAULT '',
    x            INTEGER NOT NULL,
    y            INTEGER NOT NULL,
    depth        INTEGER NOT NULL,
    entity_id    TEXT,
    encounter_id TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    UNIQUE (dungeon_id, key)
);

CREATE INDEX dungeon_rooms_dungeon ON dungeon_rooms(dungeon_id);
CREATE INDEX dungeon_rooms_encounter ON dungeon_rooms(encounter_id);

-- The connections. from_room and to_room name room keys of the same
-- dungeon. kind is how the rooms connect: a locked door may name the
-- key item that opens it (key_item_entity, null until the dressing pass
-- or the DM names one), a secret door hides a secret room, a collapse
-- is one-way. A DM's hand edit writes the same rows the generator did.
CREATE TABLE dungeon_edges (
    id              TEXT PRIMARY KEY,
    dungeon_id      TEXT NOT NULL REFERENCES dungeons(id) ON DELETE CASCADE,
    from_room       TEXT NOT NULL,
    to_room         TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN (
                        'door','locked_door','secret_door','stair','shaft',
                        'passage','collapse')),
    key_item_entity TEXT,
    one_way         INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    UNIQUE (dungeon_id, from_room, to_room)
);

CREATE INDEX dungeon_edges_dungeon ON dungeon_edges(dungeon_id);

-- +goose Down
-- Rolling back drops the designer's own tables only: nothing in the
-- campaign graph references them (placement goes through proposal
-- batches, whose reviews keep their own history), so a rollback is an
-- operator-owned decision the same rule 0012 and 0022 set.
DROP TABLE dungeon_edges;
DROP TABLE dungeon_rooms;
DROP TABLE dungeons;
