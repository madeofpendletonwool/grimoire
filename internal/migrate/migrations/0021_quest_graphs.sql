-- +goose Up
-- Quest graphs (MAD-369, stage 1 of MAD-316's quest layer): the quest stops
-- being a bare machine and becomes a thing in the campaign — a summary, a
-- status, a visibility, an act it belongs to, and links into the entity graph
-- and the knowledge layer. The machine JSON itself grows (labelled edges,
-- keyed states, terminal markers, edge preconditions) and that needs no
-- migration: state_machine is a JSON column and ParseStateMachine accepts
-- both shapes, so no DM's JSON is rewritten in place.
--
-- This issue's note on migration numbers: it claimed 0016 when main was at
-- 0011, but MAD-359/360 (0012/0013), MAD-322 (0014), MAD-363 (0015),
-- MAD-365 (0016), ui_state (0017), MAD-366 (0018), MAD-367 (0019) and
-- MAD-368 (0020) landed first — so this takes the next genuinely free
-- number, the rebase rule the issue set for itself.

-- act_id carries no foreign key on purpose: acts live in the narrative spine
-- (MAD-360) and may be created or dropped independently of a quest's life.
-- That is the precedent 0002 and 0003 set for their session_id columns, and
-- the dangling_reference check sweeps what bypasses the API.
ALTER TABLE quests ADD COLUMN summary TEXT NOT NULL DEFAULT '';
ALTER TABLE quests ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active','complete','failed','abandoned'));
ALTER TABLE quests ADD COLUMN visibility TEXT NOT NULL DEFAULT 'secret'
    CHECK (visibility IN ('public','secret'));
ALTER TABLE quests ADD COLUMN act_id TEXT NOT NULL DEFAULT '';

CREATE INDEX quests_act ON quests(act_id);

-- Who and what a quest is about: one join table rather than five nullable
-- entity columns, because a quest touches many entities and "who else is in
-- this" is the question the graph exists to answer. role: giver | subject |
-- obstacle | reward | site.
CREATE TABLE quest_entities (
    id         TEXT PRIMARY KEY,
    quest_id   TEXT NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    entity_id  TEXT NOT NULL REFERENCES entities(id),
    role       TEXT NOT NULL CHECK (role IN ('giver','subject','obstacle','reward','site')),
    created_at INTEGER NOT NULL,
    UNIQUE (quest_id, entity_id, role)
);

CREATE INDEX quest_entities_entity ON quest_entities(entity_id);

-- What a state knows: the tie between a quest and the knowledge layer. A
-- state that REQUIRES a fact presumes the party discovered it; a state that
-- REVEALS a secret fact is a clue path — exactly what unreachable_secret
-- scores. Rows land by hand or from the generators (MAD-371); there is no
-- REST write surface for this table in this issue.
CREATE TABLE quest_state_facts (
    id          TEXT PRIMARY KEY,
    quest_id    TEXT NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    state_key   TEXT NOT NULL,
    fact_id     TEXT NOT NULL REFERENCES facts(id),
    disposition TEXT NOT NULL CHECK (disposition IN ('requires','reveals')),
    created_at  INTEGER NOT NULL,
    UNIQUE (quest_id, state_key, fact_id)
);

CREATE INDEX quest_state_facts_fact ON quest_state_facts(fact_id);

-- +goose Down
-- Rolling back drops the join tables and returns quests to its 0002 shape.
-- SQLite cannot drop the added columns in place, so quests is rebuilt the
-- way 0007 and 0009 rebuild theirs — but quests is referenced by
-- quest_transitions, so the child is lifted out first (a rename re-points
-- nothing at the table being rebuilt) and both copies are made before
-- either old table drops: the order is what keeps the ON DELETE cascades
-- from eating the transitions while the rebuild runs.
DROP TABLE quest_state_facts;
DROP TABLE quest_entities;
DROP INDEX quests_act;

ALTER TABLE quest_transitions RENAME TO quest_transitions_old;
ALTER TABLE quests RENAME TO quests_old;
-- The old indexes keep their names across the renames; drop them so the
-- recreated tables can take the names back.
DROP INDEX quest_transitions_quest;
DROP INDEX quests_campaign;

CREATE TABLE quests (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    state_machine TEXT NOT NULL,
    current_state TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX quests_campaign ON quests(campaign_id);

INSERT INTO quests (id, campaign_id, name, state_machine, current_state, created_at, updated_at)
    SELECT id, campaign_id, name, state_machine, current_state, created_at, updated_at FROM quests_old;

CREATE TABLE quest_transitions (
    id         TEXT PRIMARY KEY,
    quest_id   TEXT NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    event_id   TEXT REFERENCES events(id),
    created_at INTEGER NOT NULL
);

CREATE INDEX quest_transitions_quest ON quest_transitions(quest_id, created_at);

INSERT INTO quest_transitions (id, quest_id, from_state, to_state, event_id, created_at)
    SELECT id, quest_id, from_state, to_state, event_id, created_at FROM quest_transitions_old;

DROP TABLE quest_transitions_old;
DROP TABLE quests_old;
