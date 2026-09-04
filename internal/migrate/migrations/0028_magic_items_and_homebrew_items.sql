-- +goose Up
-- The magic-item catalog and the item designer (MAD-383, stage 5.2 of
-- MAD-317): two tables, two ownerships.
--
-- `magic_items` is the SRD mirror — the item catalog's cache of an Open5e
-- sync, the exact precedent `bestiary` set in the baseline. It is replaced
-- wholesale on refresh and never holds anything but upstream SRD rows, so
-- it keeps the cache pattern: the package declares its own DDL (the
-- compat test in internal/migrate pins the two together), and this
-- migration exists so a fresh database matches an upgraded one exactly.
--
-- `homebrew_items` is the item designer's table (MAD-383), the exact
-- precedent `homebrew_monsters` set in 0027. It is deliberately NOT the
-- mirror: homebrew written into `magic_items` would be destroyed on the
-- next sync — and worse, would silently become "SRD" to every surface
-- that trusts the mirror. Homebrew lives here and reaches the catalog's
-- reads as an explicit overlay, tagged as homebrew at every hop.
--
-- campaign_id is a soft add, the same precedent conversations.campaign_id
-- and homebrew_monsters.campaign_id set: empty is the ordinary
-- owner-scoped homebrew (a DM designing for their own table), and carries
-- no foreign key for that reason. A row with a campaign belongs to that
-- campaign's material.
--
-- `design` is the full structured design as JSON (internal/items' Design
-- shape): type, base item, bonus, attunement, charges and recharge, and
-- every mechanical effect in the game's own vocabulary. There is no
-- computed_rarity column and must never be one — the DMG gives rarity
-- guidance, not a formula, and the designer's honesty is that it never
-- computes a verdict, only places a design against the SRD distribution
-- at read time.
--
-- requested_rarity is the DM's own label, not the server's. It may
-- disagree with what the corpus bands suggest; the pair of it and the
-- bands is the labelling, shown wherever the item appears.
--
-- source is designed | hand: the save endpoint accepts a hand-entered
-- design under the same structural contract. The vocabulary is a CHECK,
-- the same rule 0027 set.
--
-- (owner_id, slug) is unique: slug is the squashed name the catalog's
-- lookups key on, and one owner cannot carry two items that resolve
-- ambiguously.

CREATE TABLE IF NOT EXISTS magic_items (
	key         TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	rarity      TEXT NOT NULL DEFAULT '',
	rarity_rank INTEGER NOT NULL DEFAULT 0,
	category    TEXT NOT NULL DEFAULT '',
	data        TEXT NOT NULL,
	synced_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS magic_items_rarity ON magic_items(rarity_rank);
CREATE TABLE IF NOT EXISTS magic_items_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE homebrew_items (
	id               TEXT PRIMARY KEY,
	owner_id         TEXT NOT NULL,
	campaign_id      TEXT NOT NULL DEFAULT '',
	name             TEXT NOT NULL,
	slug             TEXT NOT NULL,
	design           TEXT NOT NULL,
	requested_rarity TEXT NOT NULL DEFAULT '',
	tags             TEXT NOT NULL DEFAULT '',
	notes            TEXT NOT NULL DEFAULT '',
	source           TEXT NOT NULL DEFAULT 'designed'
	                     CHECK (source IN ('designed','hand')),
	created_at       INTEGER NOT NULL,
	updated_at       INTEGER NOT NULL,
	UNIQUE (owner_id, slug)
);

CREATE INDEX homebrew_items_campaign ON homebrew_items(campaign_id, updated_at);

-- +goose Down
-- Rolling back drops both tables whole: the mirror is a cache of an
-- upstream sync (the next cold start re-fetches it behind the app), and a
-- staged placement batch's entity rows live in the campaign graph, not
-- here.
DROP TABLE homebrew_items;
DROP TABLE magic_items_meta;
DROP TABLE magic_items;
