-- +goose Up
-- Journeys (MAD-375, stage 3 of MAD-316): the road between two places, at
-- the density the DM asked for. The route graph, the distances, the
-- calendar and the weather are the campaign clock's (MAD-365, migration
-- 0016); this is the encounter-density layer on top of them — which days
-- carry something, what that something is, and how the journey lands back
-- in the campaign.
--
-- This issue's note on migration numbers: it claimed 0019 when main was at
-- 0011, but 0012–0024 landed first — main is at 0024 as this branch starts
-- — so this takes the next genuinely free number, the rebase rule the
-- issue set for itself.
--
-- A journey is planned, not lived. Plan writes these rows and nothing
-- else: no graph rows, no clock movement. Resolving stages one proposal
-- batch (source 'journey'); accepting that batch is what writes the
-- events and moves the clock by exactly the journey's days, reason
-- 'travel', through the clock_advances ledger — ADR 3 applied to a week
-- on the road.
--
-- journeys.route is the leg list the clock's shortest path computed (JSON:
-- [{"to": <entity id>, "days": N, "terrain": "..."}]), stored so a journey
-- stays reproducible when the route graph later changes. A DM-declared day
-- override (no route on the map) stores its single synthetic leg the same
-- way.
--
-- status is planned | underway | done | abandoned:
--   planned   the day table exists; nothing resolved, nothing staged
--   underway  days are resolving, and/or the batch is staged behind the
--             review gate
--   done      the batch was accepted; the clock moved by the journey
--   abandoned the DM's own verdict (PATCH); time did not pass
--
-- density=none writes no journey_days rows at all: "you travel for five
-- days" is one line and a day count, zero model calls, zero day rows —
-- the journey row itself is still the record.

CREATE TABLE journeys (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    from_entity TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    to_entity   TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    route       TEXT NOT NULL DEFAULT '[]',
    start_day   INTEGER NOT NULL,
    days        INTEGER NOT NULL CHECK (days >= 1),
    density     TEXT NOT NULL CHECK (density IN ('none','light','standard','dense')),
    pace        TEXT NOT NULL DEFAULT 'normal' CHECK (pace IN ('fast','normal','slow')),
    seed        INTEGER NOT NULL,
    status      TEXT NOT NULL DEFAULT 'planned'
                  CHECK (status IN ('planned','underway','done','abandoned')),
    session_id  TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    batch_id    TEXT REFERENCES proposal_batches(id) ON DELETE SET NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE INDEX journeys_campaign ON journeys(campaign_id, created_at);
CREATE INDEX journeys_batch ON journeys(batch_id);

-- The day table: one row per day of a density != none journey, written at
-- plan time by the seeded roll and never re-derived. weather is
-- clock.Weather's answer for that day, recorded here because a journey is
-- a record of what happened, even though weather is otherwise computed and
-- unstored. leg is the entity id of the leg's destination (empty for a
-- synthetic override leg).
--
-- event_kind is uneventful | encounter | discovery | hazard | social |
-- rumour | landmark.
--
-- entity_id carries no foreign key on purpose: it names whatever the day
-- is about — an entity id for most kinds, a rumour id for rumour days
-- (rumours are not entities; the mill owns them, migration 0024). The Go
-- layer validates it. encounter_id is the encounter the DM actually ran,
-- attached at day-resolve time; encounters are owner-scoped (0001), not
-- campaign-scoped, so no foreign key crosses that line either. resolved is
-- the DM's "this day happened at the table" mark.
CREATE TABLE journey_days (
    journey_id   TEXT NOT NULL REFERENCES journeys(id) ON DELETE CASCADE,
    day_index    INTEGER NOT NULL CHECK (day_index >= 0),
    clock_day    INTEGER NOT NULL,
    leg          TEXT NOT NULL DEFAULT '',
    weather      TEXT NOT NULL DEFAULT '',
    event_kind   TEXT NOT NULL DEFAULT 'uneventful'
                   CHECK (event_kind IN ('uneventful','encounter','discovery','hazard','social','rumour','landmark')),
    detail       TEXT NOT NULL DEFAULT '',
    entity_id    TEXT NOT NULL DEFAULT '',
    encounter_id TEXT NOT NULL DEFAULT '',
    resolved     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (journey_id, day_index)
);

CREATE INDEX journey_days_entity ON journey_days(entity_id);

-- +goose Down
-- Dropping the journeys deletes the day tables; the batches they staged
-- are proposal_batches rows and survive (the same rule 0012, 0016, 0019
-- and 0020 set).
DROP TABLE journey_days;
DROP TABLE journeys;
