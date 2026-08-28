-- +goose Up
-- The campaign clock (MAD-365, stage 1 of MAD-315): the calendar, the clock
-- ledger, and the schedule. The Go layer is internal/clock (pure arithmetic:
-- no DB, no wall clock, no network) plus internal/campaign/clock.go for the
-- rows. This issue's note on migration numbers: it claimed 0014 when main was
-- at 0011, but MAD-359/360 (0012/0013), MAD-322 (0014) and MAD-363 (0015)
-- landed first — so this takes the next genuinely free number, exactly the
-- rebase rule the issue set for itself.

-- One calendar per campaign, stored as the JSON definition internal/clock
-- owns. Its own table rather than a key in campaigns.settings because the
-- schedule references it semantically and settings is deliberately untyped.
-- seed is the campaign's weather seed: weather is derived, never stored, and
-- re-rolling it means changing this value — a recorded decision, not a
-- refresh button.
CREATE TABLE campaign_calendars (
    campaign_id TEXT PRIMARY KEY REFERENCES campaigns(id) ON DELETE CASCADE,
    definition  TEXT NOT NULL,
    epoch_label TEXT NOT NULL DEFAULT '',
    seed        TEXT NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL
);

-- The clock is a ledger, not a settable integer: campaigns.clock is the
-- cached head of these rows. Faction plan progress, schedule firing and
-- downtime all mean "what happened between day A and day B", and a clock
-- that can jump silently makes every one of those unanswerable. A backwards
-- move is legal (a DM fixing a typo) and recorded like any other.
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

CREATE INDEX clock_advances_campaign ON clock_advances(campaign_id, created_at);
CREATE INDEX clock_advances_session ON clock_advances(session_id);

-- The schedule: festivals, rituals, NPC routines, caravan arrivals. An NPC's
-- routine is a row here with entity_id set — not a second table with its own
-- recurrence, its own firing and its own "did it happen". recurrence is
-- 'none' | 'yearly' | 'monthly' | 'every_n_days' (every_n_days pairs with
-- every_n_days > 0); status is pending until the world passes it (fired,
-- cancelled) or the clock leaves it behind (missed, surfaced by integrity).
-- visibility mirrors facts: secret entries are DM-only.
CREATE TABLE scheduled_events (
    id              TEXT PRIMARY KEY,
    campaign_id     TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    detail          TEXT NOT NULL DEFAULT '',
    day             INTEGER NOT NULL,
    recurrence      TEXT NOT NULL DEFAULT 'none' CHECK (recurrence IN ('none','yearly','monthly','every_n_days')),
    every_n_days    INTEGER NOT NULL DEFAULT 0 CHECK (every_n_days >= 0),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','fired','cancelled','missed')),
    entity_id       TEXT REFERENCES entities(id) ON DELETE CASCADE,
    location_entity TEXT REFERENCES entities(id) ON DELETE SET NULL,
    visibility      TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','secret')),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    CHECK (recurrence != 'every_n_days' OR every_n_days > 0)
);

CREATE INDEX scheduled_events_campaign ON scheduled_events(campaign_id, day);
CREATE INDEX scheduled_events_entity ON scheduled_events(entity_id);

-- +goose Down
-- Dropping the calendar drops the schedule's reckoning and the clock's
-- ledger; rolling back is an operator-owned decision (`grimoire migrate
-- down`), the same rule 0002 and 0004 set. campaigns.clock survives as the
-- plain integer it started as.
DROP TABLE scheduled_events;
DROP TABLE clock_advances;
DROP TABLE campaign_calendars;
