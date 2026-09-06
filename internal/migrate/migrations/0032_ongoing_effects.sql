-- +goose Up
-- The duration and condition engine (MAD-421, stage 4 of MAD-417): every
-- ongoing effect is a row. Target entity, effect reference (a spell, a
-- condition, a feature), source, a concentration flag, and a remaining
-- amount in rounds, minutes, hours, days, until-dispelled or until-rest.
--
-- Two clocks, one engine. In combat durations decrement on turn advance
-- (Stage 5 wires the round counter; the engine's tick and this table's
-- remaining_seconds are that half's model). Out of combat they decrement
-- against the campaign clock: every read derives what a day advance (or a
-- rest) has worn away from applied_day — the remaining column is the
-- effect's state as of its anchor day, never a live total, the same
-- derivation-not-storage rule the ledger folds balances under.
--
-- amount/unit keep the declared duration verbatim ("1 minute", "8 hours")
-- for provenance; remaining_seconds is the canonical engine state (a round
-- is 6 seconds, so every unit converts exactly). until_dispelled and
-- until_rest carry no clock amount — they end by their event, and until_rest
-- rows expire when an applied rest postdates the effect.
CREATE TABLE ongoing_effects (
    id                TEXT PRIMARY KEY,
    campaign_id       TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    target_id         TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL CHECK (kind IN ('spell','condition','feature','other')),
    name              TEXT NOT NULL,
    ref               TEXT NOT NULL DEFAULT '',
    source_id         TEXT REFERENCES entities(id) ON DELETE SET NULL,
    source_name       TEXT NOT NULL DEFAULT '',
    concentration     INTEGER NOT NULL DEFAULT 0 CHECK (concentration IN (0,1)),
    amount            INTEGER NOT NULL DEFAULT 0,
    unit              TEXT NOT NULL CHECK (unit IN ('round','minute','hour','day','until_dispelled','until_rest')),
    remaining_seconds INTEGER NOT NULL DEFAULT 0,
    applied_day       INTEGER NOT NULL,
    status            TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','ended')),
    end_reason        TEXT CHECK (end_reason IN ('expired','dispelled','concentration_broken','rest','superseded','manual')),
    ended_by          TEXT NOT NULL DEFAULT '',
    actor             TEXT NOT NULL DEFAULT '',
    session_id        TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    session_event_id  TEXT REFERENCES session_events(id) ON DELETE SET NULL,
    note              TEXT NOT NULL DEFAULT '',
    applied_at        INTEGER NOT NULL,
    ended_at          INTEGER,
    updated_at        INTEGER NOT NULL
);

CREATE INDEX ongoing_effects_campaign ON ongoing_effects(campaign_id, applied_at);
CREATE INDEX ongoing_effects_target ON ongoing_effects(target_id, status);
CREATE INDEX ongoing_effects_concentration ON ongoing_effects(campaign_id, source_id) WHERE concentration = 1 AND status = 'active';

-- +goose Down
-- The rows go, the entities and rests they hang from stay (the rule 0030
-- set: rolling back is an operator-owned decision).
DROP TABLE ongoing_effects;
