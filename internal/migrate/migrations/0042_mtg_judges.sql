-- +goose Up
-- Judges and spectators (MAD-338, stage 6 of MAD-321): a non-playing
-- participant who sees the public game and helps settle disputes. One
-- table: the role row — judge or spectator, one per account per game,
-- redeemed through the same join code a seat is, except observers may
-- join a live game, because a dispute is exactly when a judge arrives.
-- The ruling log itself (mtg_rulings) has existed since 0014, waiting
-- for this issue to write to it; a ruling is anchored to the event
-- ordinal it concerns and survives a rewind — a human record is never
-- clobbered by a truncate.

CREATE TABLE mtg_observers (
    id        TEXT PRIMARY KEY,
    game_id   TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL,
    role      TEXT NOT NULL CHECK (role IN ('judge', 'spectator')),
    name      TEXT NOT NULL,
    joined_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX idx_mtg_observers_member ON mtg_observers(game_id, user_id);

CREATE INDEX idx_mtg_observers_user ON mtg_observers(user_id);

-- +goose Down

DROP TABLE mtg_observers;
