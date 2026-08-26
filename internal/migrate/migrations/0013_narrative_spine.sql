-- +goose Up
-- The narrative spine (MAD-360, stage 1 of MAD-314's generators): acts,
-- scenes, and the planned face of a session. The campaign graph (0002) models
-- what is true; these tables model what is planned. Everything the stage-5
-- generators write into lands here first, and is authorable by hand — with no
-- model key configured a DM still gets a working act-and-scene planner.
--
-- The Go layer is internal/story; it creates no tables, following the pattern
-- migrations 0002 and up set. Deterministic planning rules (pace, act shapes,
-- spine validation) live there as pure functions over a snapshot.

-- An act is one movement of the campaign: a level band, a premise, and a
-- place in the order. level_start..level_end is the band the party is meant
-- to cross inside the act; story.Validate checks that neighbours neither
-- overlap nor leave a gap.
CREATE TABLE acts (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    ordinal     INTEGER NOT NULL,
    name        TEXT NOT NULL,
    premise     TEXT NOT NULL DEFAULT '',
    level_start INTEGER NOT NULL,
    level_end   INTEGER NOT NULL,
    status      TEXT NOT NULL DEFAULT 'planned' CHECK (status IN ('planned','active','done')),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (campaign_id, ordinal),
    CHECK (level_start >= 1 AND level_end <= 20 AND level_start <= level_end)
);

CREATE INDEX acts_campaign ON acts(campaign_id, ordinal);

-- A scene is not an encounter: an encounter is a combat scene's roster, and
-- internal/encounter already owns rosters. kind says which of the six things
-- a scene is. session_id is nullable — a scene can be planned long before it
-- is seated in a session; setting_entity points at the place it happens.
CREATE TABLE scenes (
    id             TEXT PRIMARY KEY,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    act_id         TEXT NOT NULL REFERENCES acts(id) ON DELETE CASCADE,
    session_id     TEXT REFERENCES game_sessions(id) ON DELETE SET NULL,
    ordinal        INTEGER NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('social','exploration','combat','revelation','downtime','travel')),
    name           TEXT NOT NULL,
    purpose        TEXT NOT NULL DEFAULT '',
    setting_entity TEXT REFERENCES entities(id),
    status         TEXT NOT NULL DEFAULT 'planned' CHECK (status IN ('planned','active','done')),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE (act_id, ordinal)
);

CREATE INDEX scenes_campaign ON scenes(campaign_id, act_id, ordinal);
CREATE INDEX scenes_session ON scenes(session_id);

-- Who a scene is about. 'focus' is the one the scene exists for; 'present'
-- is on stage; 'offstage' acts through others; 'mentioned' is named only.
CREATE TABLE scene_cast (
    id         TEXT PRIMARY KEY,
    scene_id   TEXT NOT NULL REFERENCES scenes(id) ON DELETE CASCADE,
    entity_id  TEXT NOT NULL REFERENCES entities(id),
    role       TEXT NOT NULL CHECK (role IN ('focus','present','offstage','mentioned')),
    created_at INTEGER NOT NULL,
    UNIQUE (scene_id, entity_id)
);

CREATE INDEX scene_cast_entity ON scene_cast(entity_id);

-- Secrets in play are ordinary facts rows with visibility='secret' — the
-- same rows unreachable_secret scores. Planning a secret into a scene is
-- what makes it reachable, not a parallel notion of one. disposition:
-- 'in_play' the scene engages the secret; 'revealed_if' it comes out on a
-- condition; 'withheld' the scene deliberately sits on it.
CREATE TABLE scene_secrets (
    id          TEXT PRIMARY KEY,
    scene_id    TEXT NOT NULL REFERENCES scenes(id) ON DELETE CASCADE,
    fact_id     TEXT NOT NULL REFERENCES facts(id),
    disposition TEXT NOT NULL CHECK (disposition IN ('in_play','revealed_if','withheld')),
    created_at  INTEGER NOT NULL,
    UNIQUE (scene_id, fact_id)
);

CREATE INDEX scene_secrets_fact ON scene_secrets(fact_id);

-- Outcomes A-D: the branches a scene can resolve along. leads_to_scene names
-- the scene that branch continues into (story.Validate rejects one pointing
-- at a scene in another act); quest_transition is JSON
-- {"quest": "<id>", "from": "<state>", "to": "<state>"}
-- naming an edge of that quest's own state machine — the same machine
-- campaign.StateMachine.HasEdge checks — or empty when the outcome moves no
-- quest.
CREATE TABLE scene_outcomes (
    id               TEXT PRIMARY KEY,
    scene_id         TEXT NOT NULL REFERENCES scenes(id) ON DELETE CASCADE,
    label            TEXT NOT NULL,
    summary          TEXT NOT NULL DEFAULT '',
    leads_to_scene   TEXT REFERENCES scenes(id) ON DELETE SET NULL,
    quest_transition TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    UNIQUE (scene_id, label)
);

CREATE INDEX scene_outcomes_scene ON scene_outcomes(scene_id);

-- The planned face of a game_sessions row: what tonight is for, what needs
-- prepping, which act it belongs to, and whether the prep is done. One plan
-- per session (the session is the primary key).
CREATE TABLE session_plans (
    session_id  TEXT PRIMARY KEY REFERENCES game_sessions(id) ON DELETE CASCADE,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    act_id      TEXT REFERENCES acts(id) ON DELETE SET NULL,
    goal        TEXT NOT NULL DEFAULT '',
    prep_notes  TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'planned' CHECK (status IN ('planned','ready','done')),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE INDEX session_plans_campaign ON session_plans(campaign_id);
CREATE INDEX session_plans_act ON session_plans(act_id);

-- +goose Down
-- Dropping the spine drops a campaign's plan with it; rolling back is an
-- operator-owned decision (`grimoire migrate down`), the same rule 0002 and
-- 0004 set.
DROP TABLE session_plans;
DROP TABLE scene_outcomes;
DROP TABLE scene_secrets;
DROP TABLE scene_cast;
DROP TABLE scenes;
DROP TABLE acts;
