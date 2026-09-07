-- +goose Up
-- Leveling and post-session mechanical reconciliation (MAD-424, stage 7
-- of MAD-417).
--
-- XP is not a pool: it is the sheet's running total, and what this table
-- records is provenance — every encounter award, the encounter it came
-- from, the share each character took, and who wrote it. xp_total is the
-- sheet's total AS THIS AWARD LEFT IT: the reconciliation pass compares
-- the sheet against the newest award's recorded total, so a hand-lowered
-- XP is caught however many awards ago it happened. "Why does the wizard
-- have 3,000 XP" resolves to these rows the same way "why is the wizard
-- out of 3rd levels" resolves to the ledger's transaction log.

CREATE TABLE xp_awards (
    id           TEXT PRIMARY KEY,
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    encounter_id TEXT NOT NULL REFERENCES encounters(id) ON DELETE CASCADE,
    entity_id    TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    amount       INTEGER NOT NULL CHECK (amount > 0),
    total_xp     INTEGER NOT NULL CHECK (total_xp >= 0),
    xp_total     INTEGER NOT NULL CHECK (xp_total >= 0),
    actor        TEXT NOT NULL DEFAULT '',
    session_id   TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    note         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);

CREATE INDEX xp_awards_campaign ON xp_awards(campaign_id, created_at);
CREATE INDEX xp_awards_encounter ON xp_awards(encounter_id);
CREATE INDEX xp_awards_entity ON xp_awards(entity_id);

-- One row per level-up proposal. There is no ungated level-up path —
-- every level-up is staged here, reviewed as a canon batch (source
-- 'level_up'), and applied by the batch finalizer exactly once: status
-- staged -> applied (an accepted item) or staged -> discarded (dismissed,
-- or nobody accepted). diff carries the computed diff the DM reviewed —
-- evidence of what was proposed; the finalizer recomputes against the
-- current sheet, the same contract the staged rest holds.
CREATE TABLE level_ups (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    entity_id   TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    class       TEXT NOT NULL,
    subclass    TEXT NOT NULL DEFAULT '',
    from_level  INTEGER NOT NULL CHECK (from_level >= 0),
    to_level    INTEGER NOT NULL CHECK (to_level >= 1 AND to_level <= 20),
    status      TEXT NOT NULL DEFAULT 'staged' CHECK (status IN ('staged','applied','discarded')),
    diff        TEXT NOT NULL DEFAULT '{}',
    batch_id    TEXT REFERENCES proposal_batches(id) ON DELETE SET NULL,
    actor       TEXT NOT NULL DEFAULT '',
    session_id  TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    note        TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

CREATE INDEX level_ups_campaign ON level_ups(campaign_id, created_at);
CREATE INDEX level_ups_batch ON level_ups(batch_id);
CREATE INDEX level_ups_entity ON level_ups(entity_id);

-- +goose Down
-- The tables go; the sheets, encounters and batches they hang from stay.
DROP TABLE level_ups;
DROP TABLE xp_awards;
