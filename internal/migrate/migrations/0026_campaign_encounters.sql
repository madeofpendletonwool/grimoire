-- +goose Up
-- Campaign-scoped encounters (MAD-378, stage 5.1 of MAD-317): one encounter
-- record instead of two unrelated ones. Until now a planned encounter lived
-- either in `encounters` (owner-scoped, 0001_baseline.sql, what the builder
-- saves) or in an 'encounter' session event's payload (what session prep
-- writes and what the canon engine reads) — and neither knew about the other.
-- MAD-373 left dungeon_rooms.encounter_id without a foreign key for exactly
-- this reason, and named this migration as the owner.
--
-- This issue's note on migration numbers: it claimed 0020 when main was at
-- 0015, but 0016-0025 landed first — so this takes the next genuinely free
-- number, the rebase rule the issue set for itself.
--
-- The table is REBUILT rather than ALTERed, which is the one thing about this
-- file worth reading twice. `encounters` is one of the tables a package still
-- declares its own DDL for (internal/encounter's schema const), and the
-- schema-compat test builds a database in both orders: migration-then-packages
-- and packages-then-migration. A plain ADD COLUMN passes the first order and
-- fails the second with "duplicate column name" the moment the package
-- declares the same column. Copying only the 0001 columns into a table
-- declared with all of them is correct under both orders and preserves every
-- row either way. Nothing references encounters (dungeon_rooms.encounter_id
-- and journey_days.encounter_id are deliberately soft columns, and there are
-- no triggers on it), so the rebuild is a straight copy.
--
-- campaign_id is a soft add: empty keeps every existing owner-scoped
-- encounter working untouched, the same precedent conversations.campaign_id
-- set in 0001_baseline.sql. It carries no foreign key for that reason — a row
-- belonging to no campaign is the ordinary case, not a dangling one.
--
-- The session event stays the canonical marker that an encounter is planned
-- for a session (ADR 9: the event log is the only writer). session_event_id
-- says which event this record is the long form of, so the event can name the
-- record that holds the full design instead of duplicating a thin roster.
-- scene_id is the spine scene (MAD-360) the encounter belongs to — a combat
-- scene's roster, as internal/story already says internal/encounter owns.
--
-- objective and terrain are reserved here and filled by the objectives issue
-- (the next split of MAD-317), so that issue needs no migration of its own.
-- terrain is a JSON object; '{}' is "nothing declared".

ALTER TABLE encounters RENAME TO encounters_old;
DROP INDEX IF EXISTS encounters_owner;
DROP INDEX IF EXISTS encounters_campaign;

CREATE TABLE encounters (
	id               TEXT PRIMARY KEY,
	owner_id         TEXT NOT NULL,
	name             TEXT NOT NULL DEFAULT '',
	notes            TEXT NOT NULL DEFAULT '',
	party            TEXT NOT NULL DEFAULT '[]',
	monsters         TEXT NOT NULL DEFAULT '[]',
	campaign_id      TEXT NOT NULL DEFAULT '',
	session_event_id TEXT NOT NULL DEFAULT '',
	scene_id         TEXT NOT NULL DEFAULT '',
	objective        TEXT NOT NULL DEFAULT '',
	terrain          TEXT NOT NULL DEFAULT '{}',
	status           TEXT NOT NULL DEFAULT 'planned'
	                   CHECK (status IN ('planned','run','discarded')),
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL
);

CREATE INDEX encounters_owner ON encounters(owner_id, updated_at);
CREATE INDEX encounters_campaign ON encounters(campaign_id, updated_at);

INSERT INTO encounters (id, owner_id, name, notes, party, monsters, created_at, updated_at)
    SELECT id, owner_id, name, notes, party, monsters, created_at, updated_at FROM encounters_old;

DROP TABLE encounters_old;

-- +goose Down
-- Rolling back returns encounters to its 0001 shape. Campaign-scoped rows
-- survive as owner-scoped ones — owner_id was never dropped — which is the
-- same "a rollback loses the new layer, never the old rows" rule 0021 set.
ALTER TABLE encounters RENAME TO encounters_old;
DROP INDEX IF EXISTS encounters_owner;
DROP INDEX IF EXISTS encounters_campaign;

CREATE TABLE encounters (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	notes       TEXT NOT NULL DEFAULT '',
	party       TEXT NOT NULL DEFAULT '[]',
	monsters    TEXT NOT NULL DEFAULT '[]',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);

CREATE INDEX encounters_owner ON encounters(owner_id, updated_at);

INSERT INTO encounters (id, owner_id, name, notes, party, monsters, created_at, updated_at)
    SELECT id, owner_id, name, notes, party, monsters, created_at, updated_at FROM encounters_old;

DROP TABLE encounters_old;
