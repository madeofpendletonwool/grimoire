-- +goose Up
-- Faction plans (MAD-366, stage 1 of the faction engine): a plan is the
-- campaign's existing quest state machine plus four things a quest does not
-- have — an owner, a rate, a progress counter and a reaction rule. The pure
-- progression arithmetic (Advance) is internal/faction; the rows live here
-- and the integrity checks read them through internal/campaign's snapshot,
-- exactly like quests. This issue's note on migration numbers: it claimed
-- 0015 when main was at 0011, but MAD-359/360 (0012/0013), MAD-322 (0014),
-- MAD-363 (0015), MAD-365 (0016) and the tiling update's ui_state (0017)
-- landed first — so this takes the next genuinely free number, the rebase
-- rule the issue set for itself.

-- One faction's plan. state_machine is the same JSON shape quests use
-- ({"initial": ..., "states": [...], "edges": [...]}) and the same rules
-- apply: a plan never moves along an edge its machine does not declare.
-- progress is the work paid toward the ACTIVE step (the first step whose
-- state the plan has not entered); cost lives on the step. visibility
-- mirrors facts and the schedule, but plans are DM material by construction
-- (ADR 2) — the column is for the day a plan's existence becomes public
-- knowledge, which is a later stage's product decision, not this one's.
CREATE TABLE faction_plans (
    id                TEXT PRIMARY KEY,
    campaign_id       TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    faction_entity    TEXT NOT NULL REFERENCES entities(id),
    name              TEXT NOT NULL,
    state_machine     TEXT NOT NULL,
    current_state     TEXT NOT NULL,
    progress          REAL NOT NULL DEFAULT 0,
    rate_per_day      REAL NOT NULL DEFAULT 1 CHECK (rate_per_day >= 0),
    status            TEXT NOT NULL DEFAULT 'dormant' CHECK (status IN ('dormant','active','stalled','complete','abandoned')),
    visibility        TEXT NOT NULL DEFAULT 'secret' CHECK (visibility IN ('public','secret')),
    started_day       INTEGER,
    last_advanced_day INTEGER,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

CREATE INDEX faction_plans_campaign ON faction_plans(campaign_id);
CREATE INDEX faction_plans_faction ON faction_plans(faction_entity, status);

-- The plan's checklist, in pursuit order (first listed, first pursued — the
-- rowid IS the order, like quest_transitions). A step is the work to ENTER
-- its state: when progress reaches cost, the plan moves current_state ->
-- state, which must be an edge the machine declares, and the overflow
-- carries into the next step. requires_json names the step's preconditions
-- (entity ids, edges, fact predicates, an enemy plan's position) and the
-- author's chosen reaction when one breaks — the engine applies the
-- arithmetic, it never guesses whether a setback is a setback.
CREATE TABLE faction_plan_steps (
    id            TEXT PRIMARY KEY,
    plan_id       TEXT NOT NULL REFERENCES faction_plans(id) ON DELETE CASCADE,
    state         TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    detail        TEXT NOT NULL DEFAULT '',
    cost          REAL NOT NULL CHECK (cost > 0),
    requires_json TEXT NOT NULL DEFAULT '[]',
    UNIQUE (plan_id, state)
);

CREATE INDEX faction_plan_steps_plan ON faction_plan_steps(plan_id);

-- Every recorded move, mirroring quest_transitions: which edge, the event
-- that caused it when one did, the clock day it happened on, and the reason
-- — which for an advance is the arithmetic that produced the progress
-- (base + each modifier's signed contribution), so "why is the cult at
-- 62%?" is answered from the ledger, not from memory.
CREATE TABLE faction_plan_transitions (
    id         TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL REFERENCES faction_plans(id) ON DELETE CASCADE,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    event_id   TEXT REFERENCES events(id),
    clock_day  INTEGER,
    reason     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX faction_plan_transitions_plan ON faction_plan_transitions(plan_id, created_at);

-- +goose Down
-- Dropping the plans deletes the faction engine's ledger; rolling back is
-- an operator-owned decision (`grimoire migrate down`), the same rule 0002
-- and 0016 set. The faction entities and their edges survive — they were
-- always graph rows, not plan rows.
DROP TABLE faction_plan_transitions;
DROP TABLE faction_plan_steps;
DROP TABLE faction_plans;
