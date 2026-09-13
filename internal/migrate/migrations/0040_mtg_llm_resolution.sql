-- +goose Up
-- The LLM identification method (MAD-331, stage 4 of MAD-321): the model
-- fallback's name identifications join the per-game identity cache, so the
-- same mumble is never re-inferred and never re-billed. The cache's method
-- vocabulary gains 'llm' — an audit trail that says how a name was
-- identified is only honest if the model tier is distinguishable from the
-- deck tiers, the global index, and a human correction.
--
-- SQLite cannot ALTER a CHECK constraint, so this is the standard
-- rebuild: new table with the widened CHECK, rows copied, old table
-- dropped, name reclaimed, index recreated. Per-game cache rows only —
-- nothing else references this table, and the copy preserves history.

CREATE TABLE mtg_name_resolutions_new (
    game_id     TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    spoken      TEXT NOT NULL,
    card_name   TEXT NOT NULL,
    method      TEXT NOT NULL CHECK (method IN ('deck_exact','deck_fuzzy','global','manual','llm')),
    confidence  REAL NOT NULL DEFAULT 0,
    resolved_at INTEGER NOT NULL,
    PRIMARY KEY (game_id, spoken)
);

INSERT INTO mtg_name_resolutions_new (game_id, spoken, card_name, method, confidence, resolved_at)
    SELECT game_id, spoken, card_name, method, confidence, resolved_at FROM mtg_name_resolutions;

DROP TABLE mtg_name_resolutions;
ALTER TABLE mtg_name_resolutions_new RENAME TO mtg_name_resolutions;

CREATE INDEX mtg_name_resolutions_card ON mtg_name_resolutions(card_name);

-- +goose Down
-- Restoring the narrower CHECK cannot keep model-identified rows: they are
-- deleted first, deliberately — rolling back removes the feature's data
-- with the feature, the way every destructive Down in this tree does.
CREATE TABLE mtg_name_resolutions_old (
    game_id     TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    spoken      TEXT NOT NULL,
    card_name   TEXT NOT NULL,
    method      TEXT NOT NULL CHECK (method IN ('deck_exact','deck_fuzzy','global','manual')),
    confidence  REAL NOT NULL DEFAULT 0,
    resolved_at INTEGER NOT NULL,
    PRIMARY KEY (game_id, spoken)
);

INSERT INTO mtg_name_resolutions_old (game_id, spoken, card_name, method, confidence, resolved_at)
    SELECT game_id, spoken, card_name, method, confidence, resolved_at
      FROM mtg_name_resolutions WHERE method <> 'llm';

DROP TABLE mtg_name_resolutions;
ALTER TABLE mtg_name_resolutions_old RENAME TO mtg_name_resolutions;

CREATE INDEX mtg_name_resolutions_card ON mtg_name_resolutions(card_name);
