-- +goose Up
-- The knowledge layer (MAD-304): awareness, discoveries, and the FTS index
-- over campaign prose. The contract is docs/campaign/model.md and
-- docs/campaign/epistemics.md; the scope-enforcement queries that read these
-- tables live in internal/knowledge.
--
-- session_id columns carry no foreign key for the same reason as migration
-- 0002: game_sessions and session_sources land with the Stage 3 session layer
-- (MAD-306), which owns them and their constraints.
--
-- knower / discovered_by are an entity id or the literal 'party'. A foreign
-- key cannot express "entity or this one magic string", so the Go layer
-- validates it and the Stage 3 dangling_reference check sweeps what bypassed
-- the API.

-- First-class discoveries: the audit trail behind "why does Grimoire think
-- Mira knows this?". method is prose ("read the mining ledger"); the span
-- quadruple (session, source, offsets, quote) is present when the discovery
-- came out of a recorded source and empty when a DM typed it at the table.
-- Created before awareness because awareness carries a discovery_id foreign
-- key; SQLite tolerates forward references, but there is no reason to make
-- anyone reason about that.
CREATE TABLE discoveries (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    fact_id       TEXT NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    discovered_by TEXT NOT NULL,
    session_id    TEXT,
    method        TEXT NOT NULL DEFAULT '',
    source_id     TEXT,
    span_start    INTEGER,
    span_end      INTEGER,
    quote         TEXT NOT NULL DEFAULT '',
    confidence    REAL NOT NULL DEFAULT 1 CHECK (confidence >= 0 AND confidence <= 1),
    accepted_by   TEXT NOT NULL DEFAULT '',
    accepted_at   INTEGER,
    created_at    INTEGER NOT NULL
);

CREATE INDEX discoveries_campaign ON discoveries(campaign_id, created_at);
CREATE INDEX discoveries_fact ON discoveries(fact_id);

-- Who knows what, and how sure they are about knowing it. One row per
-- (knower, fact). 'unaware' is stored rather than inferred from absence: a
-- missing row and a deliberate "they walked past the ledger" are different
-- facts about the campaign (epistemics.md, Stance).
CREATE TABLE awareness (
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    knower       TEXT NOT NULL,
    fact_id      TEXT NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    stance       TEXT NOT NULL CHECK (stance IN ('knows','suspects','believes_false','unaware')),
    confidence   REAL NOT NULL DEFAULT 1 CHECK (confidence >= 0 AND confidence <= 1),
    since_event  TEXT REFERENCES events(id) ON DELETE SET NULL,
    discovery_id TEXT REFERENCES discoveries(id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (campaign_id, knower, fact_id)
);

CREATE INDEX awareness_fact ON awareness(fact_id);
CREATE INDEX awareness_knower ON awareness(campaign_id, knower);

-- The FTS index over campaign prose: entity names+summaries, fact statements,
-- event summaries. The index itself carries no epistemics — it is a dumb text
-- mirror of three tables — and every read goes through internal/knowledge,
-- which applies the scope filter in the same SQL statement as the MATCH. A
-- plain (non-external-content) FTS5 table so triggers can maintain it with
-- ordinary INSERT/DELETE.
CREATE VIRTUAL TABLE campaign_prose USING fts5(
    body,
    campaign_id UNINDEXED,
    kind UNINDEXED, -- 'entity' | 'fact' | 'event'
    ref_id UNINDEXED
);

-- Backfill: databases that already carry campaign data get their prose
-- indexed on upgrade, not only on future writes.
INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    SELECT e.name || ' ' || e.summary, e.campaign_id, 'entity', e.id
      FROM entities e WHERE e.status <> 'deleted';
INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    SELECT f.statement, f.campaign_id, 'fact', f.id FROM facts f;
INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    SELECT ev.summary, ev.campaign_id, 'event', ev.id FROM events ev;

-- Maintenance triggers. Facts and events are never deleted (0002's rule);
-- entities are soft-deleted, so the entity update trigger drops the FTS row
-- when status flips to 'deleted' and restores it if the row is ever revived.
-- Delete triggers exist for ON DELETE CASCADE hygiene; a stale FTS row after
-- a campaign delete is inert (every read is campaign-scoped) but wrong, and
-- wrong is worth two more triggers.
-- +goose StatementBegin
CREATE TRIGGER campaign_prose_entity_ins AFTER INSERT ON entities BEGIN
    INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    VALUES (NEW.name || ' ' || NEW.summary, NEW.campaign_id, 'entity', NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER campaign_prose_entity_upd AFTER UPDATE OF name, summary, status ON entities BEGIN
    DELETE FROM campaign_prose WHERE kind = 'entity' AND ref_id = NEW.id AND campaign_id = NEW.campaign_id;
    INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
        SELECT NEW.name || ' ' || NEW.summary, NEW.campaign_id, 'entity', NEW.id
         WHERE NEW.status <> 'deleted';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER campaign_prose_entity_del AFTER DELETE ON entities BEGIN
    DELETE FROM campaign_prose WHERE kind = 'entity' AND ref_id = OLD.id AND campaign_id = OLD.campaign_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER campaign_prose_fact_ins AFTER INSERT ON facts BEGIN
    INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    VALUES (NEW.statement, NEW.campaign_id, 'fact', NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER campaign_prose_fact_upd AFTER UPDATE OF statement ON facts BEGIN
    DELETE FROM campaign_prose WHERE kind = 'fact' AND ref_id = NEW.id AND campaign_id = NEW.campaign_id;
    INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    VALUES (NEW.statement, NEW.campaign_id, 'fact', NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER campaign_prose_fact_del AFTER DELETE ON facts BEGIN
    DELETE FROM campaign_prose WHERE kind = 'fact' AND ref_id = OLD.id AND campaign_id = OLD.campaign_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER campaign_prose_event_ins AFTER INSERT ON events BEGIN
    INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    VALUES (NEW.summary, NEW.campaign_id, 'event', NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER campaign_prose_event_upd AFTER UPDATE OF summary ON events BEGIN
    DELETE FROM campaign_prose WHERE kind = 'event' AND ref_id = NEW.id AND campaign_id = NEW.campaign_id;
    INSERT INTO campaign_prose (body, campaign_id, kind, ref_id)
    VALUES (NEW.summary, NEW.campaign_id, 'event', NEW.id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER campaign_prose_event_del AFTER DELETE ON events BEGIN
    DELETE FROM campaign_prose WHERE kind = 'event' AND ref_id = OLD.id AND campaign_id = OLD.campaign_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER campaign_prose_event_del;
DROP TRIGGER campaign_prose_event_upd;
DROP TRIGGER campaign_prose_event_ins;
DROP TRIGGER campaign_prose_fact_del;
DROP TRIGGER campaign_prose_fact_upd;
DROP TRIGGER campaign_prose_fact_ins;
DROP TRIGGER campaign_prose_entity_del;
DROP TRIGGER campaign_prose_entity_upd;
DROP TRIGGER campaign_prose_entity_ins;
DROP TABLE campaign_prose;
DROP TABLE discoveries;
DROP TABLE awareness;
