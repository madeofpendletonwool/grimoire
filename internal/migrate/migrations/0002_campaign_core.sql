-- +goose Up
-- The campaign core graph (MAD-303): campaigns, members, entities, facts,
-- provenance, contradictions, events, quests, and the relationships between
-- them. The contract these tables implement is docs/campaign/model.md.
--
-- This is the first schema that lives only in migrations: internal/campaign
-- contains no CREATE TABLE of its own, and every later campaign table follows
-- the same rule.
--
-- session_id columns here (fact_provenance, events) deliberately carry no
-- foreign key: the game_sessions and session_sources tables land with the
-- Stage 3 session layer (MAD-306), which owns them and their constraints.

-- The controlled edge vocabulary. A free-text edge type is a graph nobody can
-- query, so rel_type below points here and the database rejects anything else.
-- Inverse pairs are seeded both ways; inverse is informational (what the
-- reverse edge would be called), not enforced.
CREATE TABLE relationship_types (
    name        TEXT PRIMARY KEY,
    inverse     TEXT,
    description TEXT NOT NULL DEFAULT ''
);

INSERT INTO relationship_types (name, inverse, description) VALUES
    ('knows', 'knows', 'Has met and can recognize the other.'),
    ('allied_with', 'allied_with', 'Fighting on the same side, by agreement.'),
    ('enemy_of', 'enemy_of', 'Actively opposed.'),
    ('related_to', 'related_to', 'Family or other kinship.'),
    ('serves', 'served_by', 'Answers to the other, openly or not.'),
    ('served_by', 'serves', 'Receives the service of the other.'),
    ('member_of', 'has_member', 'Belongs to the faction or organization.'),
    ('has_member', 'member_of', 'Counts the other as a member.'),
    ('leads', 'led_by', 'Commands or speaks for the other side.'),
    ('led_by', 'leads', 'Takes direction from the other.'),
    ('owns', 'owned_by', 'Holds title or possession.'),
    ('owned_by', 'owns', 'Is held by the other.'),
    ('located_in', 'contains', 'Present at or within the other (place, region, building).'),
    ('contains', 'located_in', 'Holds the other within its bounds.'),
    ('worships', 'worshipped_by', 'Offers devotion to the deity or ideal.'),
    ('worshipped_by', 'worships', 'Receives the other''s devotion.'),
    ('betrayed', 'betrayed_by', 'Broke faith with the other.'),
    ('betrayed_by', 'betrayed', 'Was betrayed by the other.'),
    ('secretly_controls', 'secretly_controlled_by', 'Pulls the other''s strings without it being known.'),
    ('secretly_controlled_by', 'secretly_controls', 'Is secretly directed by the other.'),
    ('killed', 'killed_by', 'Ended the other''s life.'),
    ('killed_by', 'killed', 'Was ended by the other.');

CREATE TABLE campaigns (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    system     TEXT NOT NULL DEFAULT '',
    premise    TEXT NOT NULL DEFAULT '',
    clock      INTEGER NOT NULL DEFAULT 0,
    settings   TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX campaigns_owner ON campaigns(owner_id, updated_at);

CREATE TABLE entities (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('pc','npc','faction','location','item','deity','organization','creature','concept')),
    name        TEXT NOT NULL,
    summary     TEXT NOT NULL DEFAULT '',
    payload     TEXT NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','inactive','dead','destroyed','missing','deleted')),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE INDEX entities_campaign ON entities(campaign_id, kind);
CREATE INDEX entities_name ON entities(campaign_id, name COLLATE NOCASE);

-- One entity, many names: canonical name, aliases, epithets. The "Tom the
-- innkeeper is also Thomas Vane" table; extraction matches against it instead
-- of minting a second Tom, and the entity_merge_candidate integrity check
-- reads it.
--
-- Named entity_aliases, not entity_names as the model doc sketch had it:
-- entity_names is already the rules-index's SRD name dictionary and means
-- something entirely different.
CREATE TABLE entity_aliases (
    id         TEXT PRIMARY KEY,
    entity_id  TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('canonical','alias','epithet')),
    created_at INTEGER NOT NULL,
    UNIQUE (entity_id, name COLLATE NOCASE, kind)
);

CREATE INDEX entity_aliases_name ON entity_aliases(name COLLATE NOCASE);

-- Atomic statements with an entity or a literal object, never both. Nothing
-- deletes a fact: superseded_by plus confidence 'retconned' is how a change of
-- mind is recorded. CHECK constraints mirror the Go vocabularies so raw SQL
-- cannot smuggle in a confidence the engine does not know.
CREATE TABLE facts (
    id             TEXT PRIMARY KEY,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    subject_entity TEXT NOT NULL REFERENCES entities(id),
    predicate      TEXT NOT NULL,
    object_entity  TEXT REFERENCES entities(id),
    object_literal TEXT,
    statement      TEXT NOT NULL,
    confidence     TEXT NOT NULL CHECK (confidence IN ('proposed','canon','derived','contested','retconned')),
    visibility     TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','secret')),
    created_by     TEXT NOT NULL DEFAULT '',
    superseded_by  TEXT REFERENCES facts(id),
    created_at     INTEGER NOT NULL,
    CHECK ((object_entity IS NULL) <> (object_literal IS NULL))
);

CREATE INDEX facts_campaign ON facts(campaign_id, subject_entity);
CREATE INDEX facts_object ON facts(campaign_id, object_entity);
CREATE INDEX facts_superseded ON facts(superseded_by);

-- Where a fact came from. Every fact carries at least one row here; a fact
-- with none is a bug the integrity check reports. For extracted facts the
-- session/source/span/quote quadruple is mandatory (the span rule); a
-- dm_authored fact may carry none of them.
CREATE TABLE fact_provenance (
    id         TEXT PRIMARY KEY,
    fact_id    TEXT NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    session_id TEXT,
    source_id  TEXT,
    span_start INTEGER,
    span_end   INTEGER,
    quote      TEXT NOT NULL DEFAULT '',
    method     TEXT NOT NULL CHECK (method IN ('dm_authored','ai_proposed','extracted','imported')),
    created_at INTEGER NOT NULL
);

CREATE INDEX fact_provenance_fact ON fact_provenance(fact_id);

-- The contradiction register. Two credibly-sourced facts that conflict are
-- both downgraded to 'contested' and linked through fact_versions; nothing
-- picks a winner.
CREATE TABLE contradictions (
    id              TEXT PRIMARY KEY,
    campaign_id     TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    subject_entity  TEXT NOT NULL REFERENCES entities(id),
    predicate       TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved_by_review')),
    resolution_note TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL
);

CREATE INDEX contradictions_campaign ON contradictions(campaign_id, status);

-- Per-source versions of a contradiction, keeping the conflicting accounts
-- separable.
CREATE TABLE fact_versions (
    id               TEXT PRIMARY KEY,
    contradiction_id TEXT NOT NULL REFERENCES contradictions(id) ON DELETE CASCADE,
    fact_id          TEXT NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    label            TEXT NOT NULL,
    statement        TEXT NOT NULL,
    created_at       INTEGER NOT NULL,
    UNIQUE (contradiction_id, fact_id)
);

-- The timeline. Dual ordering on purpose: clock_at is in-world (day counter
-- on the campaign clock, NULL when unknown), real_ordinal is the order the
-- table actually played through it. They diverge on flashbacks and split
-- parties, and collapsing them loses a question people actually ask.
CREATE TABLE events (
    id              TEXT PRIMARY KEY,
    campaign_id     TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    session_id      TEXT,
    summary         TEXT NOT NULL,
    clock_at        INTEGER,
    real_ordinal    INTEGER NOT NULL,
    location_entity TEXT REFERENCES entities(id),
    created_at      INTEGER NOT NULL,
    UNIQUE (campaign_id, real_ordinal)
);

CREATE INDEX events_clock ON events(campaign_id, clock_at);

CREATE TABLE event_participants (
    id        TEXT PRIMARY KEY,
    event_id  TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    entity_id TEXT NOT NULL REFERENCES entities(id),
    role      TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (event_id, entity_id)
);

CREATE INDEX event_participants_entity ON event_participants(entity_id);

CREATE TABLE event_links (
    id         TEXT PRIMARY KEY,
    from_event TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    to_event   TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    link       TEXT NOT NULL CHECK (link IN ('caused','enabled','revealed')),
    created_at INTEGER NOT NULL,
    UNIQUE (from_event, to_event, link),
    CHECK (from_event <> to_event)
);

CREATE INDEX event_links_to ON event_links(to_event);

-- Typed edges. rel_type points at the controlled vocabulary above, so the
-- database itself rejects an edge nobody can query. justified_by_fact makes an
-- edge auditable: it is derived from a fact that carries provenance, not
-- merely asserted.
CREATE TABLE relationships (
    id                TEXT PRIMARY KEY,
    from_entity       TEXT NOT NULL REFERENCES entities(id),
    rel_type          TEXT NOT NULL REFERENCES relationship_types(name),
    to_entity         TEXT NOT NULL REFERENCES entities(id),
    strength          INTEGER NOT NULL DEFAULT 0,
    justified_by_fact TEXT REFERENCES facts(id),
    since_event       TEXT REFERENCES events(id),
    created_at        INTEGER NOT NULL,
    UNIQUE (from_entity, rel_type, to_entity)
);

CREATE INDEX relationships_to ON relationships(to_entity, rel_type);

-- A quest is a state machine, not a checkbox. state_machine is JSON:
-- {"initial": "...", "states": [...], "edges": [{"from": "...", "to": "..."}]}
-- so a DM can describe a quest that branches.
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

-- Every move is tied to the event that caused it (event_id nullable for a
-- manual DM move). A move along an edge the machine does not have is an
-- error-severity integrity finding.
CREATE TABLE quest_transitions (
    id         TEXT PRIMARY KEY,
    quest_id   TEXT NOT NULL REFERENCES quests(id) ON DELETE CASCADE,
    from_state TEXT NOT NULL,
    to_state   TEXT NOT NULL,
    event_id   TEXT REFERENCES events(id),
    created_at INTEGER NOT NULL
);

CREATE INDEX quest_transitions_quest ON quest_transitions(quest_id, created_at);

-- The whole player-identity story (ADR 4): a player is an ordinary auth user
-- narrowed by this row. No row, no access — default deny is a missing row, not
-- a missing route. The invite flow that mints these rows lands in MAD-305.
CREATE TABLE campaign_members (
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         TEXT NOT NULL CHECK (role IN ('dm','player','observer')),
    character_id TEXT REFERENCES entities(id) ON DELETE SET NULL,
    joined_at    INTEGER NOT NULL,
    PRIMARY KEY (campaign_id, user_id)
);

CREATE INDEX campaign_members_user ON campaign_members(user_id);

-- +goose Down
-- Dropping the campaign tables deletes every campaign on the box. Rolling
-- back is therefore a destructive, operator-owned decision, which is exactly
-- why it only happens through `grimoire migrate down` with intent.
DROP TABLE campaign_members;
DROP TABLE quest_transitions;
DROP TABLE quests;
DROP TABLE relationships;
DROP TABLE event_links;
DROP TABLE event_participants;
DROP TABLE events;
DROP TABLE fact_versions;
DROP TABLE contradictions;
DROP TABLE fact_provenance;
DROP TABLE facts;
DROP TABLE entity_aliases;
DROP TABLE entities;
DROP TABLE campaigns;
DROP TABLE relationship_types;
