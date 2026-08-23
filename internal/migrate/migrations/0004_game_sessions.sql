-- +goose Up
-- The session layer (MAD-306, absorbing MAD-286): game sessions, their
-- sources, and the chronological event log — the ruling log from MAD-286 is
-- one `kind` of session event rather than a parallel system (ADR 7).
--
-- The contract is docs/campaign/model.md § "Sessions and sources". The Go
-- layer is internal/gamesession; it creates no tables, following the pattern
-- migrations 0002 and 0003 set.
--
-- Naming: game_sessions, never sessions — auth already owns that name for
-- login sessions (ADR 7).
--
-- fact_provenance (0002) and discoveries (0003) carry source_id / span columns
-- that point here without a foreign key: they predate this migration, and
-- rebuilding either table to add one is not worth the risk on a self-hosted
-- SQLite box. The Stage 3 dangling_reference check (MAD-309) sweeps what
-- bypasses the API instead.

-- One played (or planned) sitting. ordinal is per-campaign and assigned by
-- the Go layer (max+1); started_at/ended_at are real-world timestamps in
-- milliseconds, NULL while the session has not started / not ended.
CREATE TABLE game_sessions (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    ordinal     INTEGER NOT NULL,
    name        TEXT NOT NULL,
    started_at  INTEGER,
    ended_at    INTEGER,
    status      TEXT NOT NULL DEFAULT 'planned' CHECK (status IN ('planned','live','done')),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (campaign_id, ordinal)
);

CREATE INDEX game_sessions_campaign ON game_sessions(campaign_id, ordinal);

-- The raw material, stored verbatim and immutable: no UPDATE path exists in
-- the Go layer, and span offsets into content stay valid forever. kind is the
-- author-anchored vocabulary — a player journal and a DM note carry different
-- weight and can contradict each other on purpose.
--
-- checksum (sha256 hex of content) makes downstream extraction runs
-- idempotent (MAD-307). timing is the parsed cue list for .srt/.vtt sources
-- as JSON [{start_ms, end_ms, text}], NULL for plain text.
CREATE TABLE session_sources (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES game_sessions(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('transcript','dm_notes','player_journal','chat_log','live_mark')),
    author     TEXT NOT NULL DEFAULT '',
    title      TEXT NOT NULL DEFAULT '',
    content    TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    timing     TEXT,
    created_at INTEGER NOT NULL
);

CREATE INDEX session_sources_session ON session_sources(session_id, created_at);

-- The chronological log: rulings, Q&A, notes, discoveries, encounters. The
-- in-play "+ DISCOVERY" button (MAD-318) writes one of these; MAD-286's ruling
-- log is kind 'ruling'. seq is per-session and assigned by the Go layer, so
-- the log order is insertion order even when timestamps tie.
--
-- summary and detail are first-class (not buried in payload) so the log can
-- be listed and the FTS trigger below can index it in plain SQL: summary is
-- the question or one-line label, detail the ruling / answer / note body.
-- payload is JSON for the structured remainder (who learned what, by what
-- method, encounter budgets, ...).
CREATE TABLE session_events (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES game_sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('qa','ruling','note','discovery','encounter')),
    summary    TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    UNIQUE (session_id, seq)
);

CREATE INDEX session_events_session ON session_events(session_id, seq);

-- The prior-ruling surfacing retained from MAD-286: when a ruling or qa event
-- is recorded, its question is FTS-matched against past ruling events in the
-- same campaign — "you ruled the other way on this three sessions ago." No
-- LLM involved. ruling and qa events are both indexed (a past Q&A is weak
-- precedent but still a hint); the matching query reads back only rulings.
CREATE VIRTUAL TABLE ruling_fts USING fts5(
    body,
    campaign_id UNINDEXED,
    session_id UNINDEXED,
    event_id UNINDEXED
);

-- +goose StatementBegin
CREATE TRIGGER ruling_fts_ins AFTER INSERT ON session_events BEGIN
    INSERT INTO ruling_fts (body, campaign_id, session_id, event_id)
    SELECT NEW.summary || ' ' || NEW.detail, gs.campaign_id, NEW.session_id, NEW.id
      FROM game_sessions gs
     WHERE gs.id = NEW.session_id
       AND NEW.kind IN ('ruling', 'qa');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER ruling_fts_upd AFTER UPDATE ON session_events BEGIN
    DELETE FROM ruling_fts WHERE event_id = NEW.id;
    INSERT INTO ruling_fts (body, campaign_id, session_id, event_id)
    SELECT NEW.summary || ' ' || NEW.detail, gs.campaign_id, NEW.session_id, NEW.id
      FROM game_sessions gs
     WHERE gs.id = NEW.session_id
       AND NEW.kind IN ('ruling', 'qa');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER ruling_fts_del AFTER DELETE ON session_events BEGIN
    DELETE FROM ruling_fts WHERE event_id = OLD.id;
END;
-- +goose StatementEnd

-- +goose Down
-- Same rule as 0002: down drops a campaign's data with it and is an
-- operator-owned decision (`grimoire migrate down`), never automatic.
DROP TRIGGER ruling_fts_del;
DROP TRIGGER ruling_fts_upd;
DROP TRIGGER ruling_fts_ins;
DROP TABLE ruling_fts;
DROP TABLE session_events;
DROP TABLE session_sources;
DROP TABLE game_sessions;
