-- +goose Up
-- The canon engine's extraction stage (MAD-307): staged candidates, the
-- per-unit extraction ledger, the drop log, raw model outputs, and the runs
-- that tie them together. The contract is docs/campaign/epistemics.md §
-- "Evidence" and "The review queue"; the Go layer is internal/canon, which
-- creates no tables, following the pattern migrations 0002-0005 set.
--
-- Staging, not write-then-downgrade (ADR 3): extracted candidates land ONLY
-- in canon_candidates. Nothing the extractor produces is written into the
-- campaign graph — not facts, not events, not relationships, not entities —
-- so a candidate is invisible to every scoped retrieval path in
-- internal/knowledge and internal/campaign by construction. Accepting a
-- candidate into canon is the review queue's job (MAD-310), and only ever a
-- human decision.
--
-- session_id / source_id columns carry no foreign key, for the same reason
-- migrations 0002 and 0003 deferred theirs: the rows they point at are owned
-- by the session layer (0004), and the Stage 3 dangling_reference check
-- (MAD-309) sweeps what bypasses the API.

-- One extraction run over a campaign's sources (optionally narrowed to one
-- session or an explicit source set). stats is JSON: units done/skipped,
-- chunks, requests, token totals, estimated cost, staged counts per kind,
-- drop counts per reason, and which guard (if any) stopped the run.
CREATE TABLE canon_runs (
    id             TEXT PRIMARY KEY,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    session_id     TEXT,
    kind           TEXT NOT NULL DEFAULT 'extract' CHECK (kind IN ('extract')),
    prompt_version TEXT NOT NULL,
    model          TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','completed','stopped','failed')),
    stop_reason    TEXT NOT NULL DEFAULT '' CHECK (stop_reason IN ('','budget','candidates','units','error')),
    stats          TEXT NOT NULL DEFAULT '{}',
    error          TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE INDEX canon_runs_campaign ON canon_runs(campaign_id, created_at);

-- The per-unit ledger. One unit = one session_source. A unit whose ledger
-- row is 'done' under the same prompt version AND the same input checksum is
-- skipped on re-run, so extraction is idempotent and resumable: each unit
-- commits in its own transaction (ledger row + model outputs + candidates +
-- drops together), and an interrupted run picks up where it stopped. The
-- input checksum covers everything the model sees — prompt version, chunk
-- plan, source checksum, entity list, relationship vocabulary, roster — so
-- adding entities and re-running re-extracts rather than silently skipping.
CREATE TABLE canon_extract_ledger (
    id             TEXT PRIMARY KEY,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    source_id      TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    run_id         TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    input_checksum TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('done','error')),
    chunks         INTEGER NOT NULL DEFAULT 0,
    staged         INTEGER NOT NULL DEFAULT 0,
    dropped        INTEGER NOT NULL DEFAULT 0,
    error          TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    UNIQUE (source_id, prompt_version, input_checksum)
);

CREATE INDEX canon_extract_ledger_source ON canon_extract_ledger(source_id, created_at);

-- The staged candidates. kind says what the candidate proposes; payload is
-- the validated wire record as JSON (statement/summary, entity references,
-- predicate, stance, participants — the shape varies by kind). Every row
-- carries the span rule's full quadruple: session, source, byte offsets and
-- the verbatim quote. confidence is the extractor's own 0-1 score, not the
-- fact-confidence vocabulary — a candidate is 'proposed' by definition until
-- a human says otherwise. checksum hashes the canonical payload plus the
-- span; the adversarial pass (MAD-308) keys its verdict ledger on it.
CREATE TABLE canon_candidates (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES canon_runs(id) ON DELETE CASCADE,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    session_id     TEXT NOT NULL,
    source_id      TEXT NOT NULL,
    chunk_index    INTEGER NOT NULL DEFAULT 0,
    kind           TEXT NOT NULL CHECK (kind IN ('fact','event','discovery','relationship','entity')),
    payload        TEXT NOT NULL,
    confidence     REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    span_start     INTEGER NOT NULL,
    span_end       INTEGER NOT NULL,
    quote          TEXT NOT NULL,
    checksum       TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);

CREATE INDEX canon_candidates_campaign ON canon_candidates(campaign_id, kind, created_at);
CREATE INDEX canon_candidates_run ON canon_candidates(run_id);
CREATE INDEX canon_candidates_source ON canon_candidates(source_id);

-- The drop log: every candidate the validation rules rejected, with the
-- reason it was rejected. Counted per reason in the run stats, exactly as
-- Arda's per-unit 'dropped' column does — a drop is data, not noise.
CREATE TABLE canon_drops (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES canon_runs(id) ON DELETE CASCADE,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    source_id   TEXT NOT NULL,
    chunk_index INTEGER NOT NULL DEFAULT 0,
    kind        TEXT NOT NULL,
    ref         TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL
);

CREATE INDEX canon_drops_run ON canon_drops(run_id);
CREATE INDEX canon_drops_source ON canon_drops(source_id);

-- Raw model responses, stored verbatim with token accounting and the input
-- reference, so any candidate's provenance bottoms out in what the model
-- actually said. Never parsed on write; parsed only when replayed.
CREATE TABLE model_outputs (
    id             TEXT PRIMARY KEY,
    run_id         TEXT NOT NULL REFERENCES canon_runs(id) ON DELETE CASCADE,
    stage          TEXT NOT NULL DEFAULT 'extract',
    prompt_version TEXT NOT NULL,
    source_id      TEXT NOT NULL,
    chunk_index    INTEGER NOT NULL DEFAULT 0,
    model          TEXT NOT NULL DEFAULT '',
    input_tokens   INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    raw            TEXT NOT NULL,
    created_at     INTEGER NOT NULL
);

CREATE INDEX model_outputs_run ON model_outputs(run_id);
CREATE INDEX model_outputs_source ON model_outputs(source_id);

-- +goose Down
-- Same rule as every campaign migration: down drops a campaign's canon data
-- with it and is an operator-owned decision (`grimoire migrate down`), never
-- automatic.
DROP TABLE model_outputs;
DROP TABLE canon_drops;
DROP TABLE canon_candidates;
DROP TABLE canon_extract_ledger;
DROP TABLE canon_runs;
