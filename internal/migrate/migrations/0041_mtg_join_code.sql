-- +goose Up
-- The multiplayer pod's join code (MAD-337, stage 6 of MAD-321): a game
-- becomes shareable by code — POST /api/games/join redeems it and binds
-- the joining account to a seat. NULL on every legacy row is deliberate:
-- games created before per-seat scoping carried deck composition inside
-- public GAME_STARTED payloads, so they stay owner-only forever; only
-- games written under the split (DECK_KNOWN rows) may be joined. New
-- games mint a code at creation.

ALTER TABLE mtg_games ADD COLUMN join_code TEXT;

CREATE UNIQUE INDEX idx_mtg_games_join_code ON mtg_games(join_code) WHERE join_code IS NOT NULL;

-- +goose Down

DROP INDEX idx_mtg_games_join_code;

ALTER TABLE mtg_games DROP COLUMN join_code;
