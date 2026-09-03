-- +goose Up
-- Homebrew monsters (MAD-382, stage 5.2 of MAD-317): the monster designer's
-- table. A designed creature is a statblock the calculator has priced — the
-- computed CR and the calculator's reasoning travel with the row everywhere
-- it goes, because a homebrew monster whose maths disagreed is still
-- savable, but never without the disagreement shown.
--
-- This issue's note on migration numbers: it claimed 0021 when main was at
-- 0015, but 0016–0026 landed first — so this takes the next genuinely free
-- number, the rebase rule the split set for itself.
--
-- The table is deliberately NOT the bestiary mirror. `bestiary` is a cache
-- of an upstream sync (Catalog.Sync replaces its contents wholesale); homebrew
-- written there would be destroyed on the next refresh — and worse, would
-- silently become "SRD" to every surface that trusts the mirror. Homebrew
-- lives here and reaches the builder as an explicit overlay on the catalog
-- reads, tagged as homebrew at every hop.
--
-- campaign_id is a soft add, the same precedent conversations.campaign_id
-- set in 0001_baseline.sql: empty is the ordinary owner-scoped homebrew
-- (a DM designing for their own table), and carries no foreign key for that
-- reason. A row with a campaign belongs to that campaign's material — the
-- brief can name its factions and NPCs, and the continuity engine treats
-- planned encounters using it as resolved.
--
-- statblock is the full structured statblock as JSON (internal/statblock's
-- Statblock shape, actions parsed or explicitly unparsed, exactly like the
-- mirror's). computed_detail is the calculator's whole Rating — both halves,
-- every adjustment with its reason, the confidence — serialized verbatim.
--
-- requested_cr is the brief's ask ("7"), computed_cr the label the maths
-- produced ("7"). They may disagree; the pair IS the labelling.
--
-- source is designed | hand: the model pass writes designed, the save
-- endpoint accepts a hand-entered statblock under the same contract. The
-- vocabulary is a CHECK, not a wide-open string — the importer issue names
-- its own values when it lands.
--
-- (owner_id, slug) is unique: slug is the squashed name the catalog's
-- lookups key on, and one owner cannot carry two monsters that resolve
-- ambiguously.

CREATE TABLE homebrew_monsters (
	id              TEXT PRIMARY KEY,
	owner_id        TEXT NOT NULL,
	campaign_id     TEXT NOT NULL DEFAULT '',
	name            TEXT NOT NULL,
	slug            TEXT NOT NULL,
	statblock       TEXT NOT NULL,
	requested_cr    TEXT NOT NULL DEFAULT '',
	computed_cr     TEXT NOT NULL DEFAULT '',
	computed_detail TEXT NOT NULL DEFAULT '',
	tactics         TEXT NOT NULL DEFAULT '',
	lore            TEXT NOT NULL DEFAULT '',
	encounter_role  TEXT NOT NULL DEFAULT '',
	source          TEXT NOT NULL DEFAULT 'designed'
	                    CHECK (source IN ('designed','hand')),
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	UNIQUE (owner_id, slug)
);

CREATE INDEX homebrew_monsters_campaign ON homebrew_monsters(campaign_id, updated_at);

-- +goose Down
-- Rolling back drops the layer whole: nothing references it — the bestiary
-- mirror never carried homebrew, and a staged placement batch's entity
-- rows live in the campaign graph, not here.
DROP TABLE homebrew_monsters;
