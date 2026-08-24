-- +goose Up
-- The canon engine's adversarial validation stage (MAD-308): the verdict
-- ledger. One work item is one staged candidate; an independent model pass in
-- an explicitly skeptical stance re-checks it against only its own evidence
-- and records agree / downgrade / flag_review with a 0-1 agreement score.
--
-- The monotonicity rule (epistemics.md, Arda policy §7) is enforced twice: the
-- wire contract offers no upgrade, and a downgrade whose proposed confidence
-- is not strictly below the candidate's current value is rejected and logged,
-- never applied. Downgrades land with a guarded UPDATE that only ever lowers
-- canon_candidates.confidence, so this pass and the deterministic engine
-- (MAD-309) compose in either order.
--
-- canon_runs grows a 'validate' kind. SQLite cannot drop a CHECK constraint,
-- and dropping canon_runs outright would cascade-delete every candidate, drop
-- and model output under foreign_keys=ON. So the table is rebuilt the safe
-- way, all inside this transaction: park the old table under a new name (the
-- children's references follow the rename), recreate canon_runs with the
-- widened CHECK and the same rows, rebuild the three children to point at it
-- (create, copy, drop, rename — their rows are children, never parents, so
-- their drops cascade nothing), and only then drop the parked table, by which
-- time nothing references it. No statement ever drops a table another table
-- still points at with rows that matter.

-- 1. Widen canon_runs.kind to cover the validation pass.
ALTER TABLE canon_runs RENAME TO canon_runs_old;
DROP INDEX canon_runs_campaign;

CREATE TABLE canon_runs (
    id             TEXT PRIMARY KEY,
    campaign_id    TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    session_id     TEXT,
    kind           TEXT NOT NULL DEFAULT 'extract' CHECK (kind IN ('extract','validate')),
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

INSERT INTO canon_runs (id, campaign_id, session_id, kind, prompt_version, model, status, stop_reason, stats, error, created_at, updated_at)
SELECT id, campaign_id, session_id, kind, prompt_version, model, status, stop_reason, stats, error, created_at, updated_at FROM canon_runs_old;

-- 2. Re-point the extraction children at the recreated canon_runs. Each child
-- is rebuilt with identical columns; the copy preserves ids, so no run's
-- candidates, drops or raw outputs are touched.
CREATE TABLE canon_candidates_new (
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

INSERT INTO canon_candidates_new (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
SELECT id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at FROM canon_candidates;

DROP TABLE canon_candidates;
ALTER TABLE canon_candidates_new RENAME TO canon_candidates;

CREATE INDEX canon_candidates_campaign ON canon_candidates(campaign_id, kind, created_at);
CREATE INDEX canon_candidates_run ON canon_candidates(run_id);
CREATE INDEX canon_candidates_source ON canon_candidates(source_id);

CREATE TABLE canon_drops_new (
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

INSERT INTO canon_drops_new (id, run_id, campaign_id, source_id, chunk_index, kind, ref, reason, detail, created_at)
SELECT id, run_id, campaign_id, source_id, chunk_index, kind, ref, reason, detail, created_at FROM canon_drops;

DROP TABLE canon_drops;
ALTER TABLE canon_drops_new RENAME TO canon_drops;

CREATE INDEX canon_drops_run ON canon_drops(run_id);
CREATE INDEX canon_drops_source ON canon_drops(source_id);

CREATE TABLE model_outputs_new (
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

INSERT INTO model_outputs_new (id, run_id, stage, prompt_version, source_id, chunk_index, model, input_tokens, output_tokens, raw, created_at)
SELECT id, run_id, stage, prompt_version, source_id, chunk_index, model, input_tokens, output_tokens, raw, created_at FROM model_outputs;

DROP TABLE model_outputs;
ALTER TABLE model_outputs_new RENAME TO model_outputs;

CREATE INDEX model_outputs_run ON model_outputs(run_id);
CREATE INDEX model_outputs_source ON model_outputs(source_id);

-- Nothing references the parked table anymore.
DROP TABLE canon_runs_old;

-- 3. The verdict ledger. One row per candidate per prompt version per content
-- checksum (the candidate's own checksum), so a re-run under the same key is
-- skipped without a model call: idempotent, resumable, and nothing is billed
-- twice. verdict is the effective outcome; status separates what the machine
-- did with the model's proposal:
--   applied    — honored: agree (confirmed), downgrade (confidence lowered),
--                flag_review (flag recorded)
--   rejected   — the monotonicity rule refused it (an upgrade in disguise);
--                logged with rejection_reason, nothing applied
--   unparseable— the response yielded no usable verdict; the machine applies
--                its own conservatism (flag_review, agreement 0) rather than
--                letting a candidate pass unexamined
-- The raw response lives on the row itself (with its tokens and model): a
-- verdict's provenance bottoms out in exactly what the validator said, one
-- query away from the review screen.
CREATE TABLE canon_verdicts (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES canon_runs(id) ON DELETE CASCADE,
    campaign_id         TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    candidate_id        TEXT NOT NULL REFERENCES canon_candidates(id) ON DELETE CASCADE,
    prompt_version      TEXT NOT NULL,
    input_checksum      TEXT NOT NULL,
    verdict             TEXT NOT NULL CHECK (verdict IN ('agree','downgrade','flag_review')),
    status              TEXT NOT NULL CHECK (status IN ('applied','rejected','unparseable')),
    agreement           REAL NOT NULL CHECK (agreement >= 0 AND agreement <= 1),
    rationale           TEXT NOT NULL DEFAULT '',
    proposed_confidence REAL CHECK (proposed_confidence IS NULL OR (proposed_confidence >= 0 AND proposed_confidence <= 1)),
    confidence_before   REAL NOT NULL,
    confidence_after    REAL NOT NULL,
    rejection_reason    TEXT NOT NULL DEFAULT '',
    model               TEXT NOT NULL DEFAULT '',
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    raw                 TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    UNIQUE (candidate_id, prompt_version, input_checksum)
);

CREATE INDEX canon_verdicts_campaign ON canon_verdicts(campaign_id, created_at);
CREATE INDEX canon_verdicts_run ON canon_verdicts(run_id);
CREATE INDEX canon_verdicts_candidate ON canon_verdicts(candidate_id, created_at);

-- +goose Down
-- The reverse of the Up, same mechanics: the verdict ledger goes first (it
-- references canon_candidates), the validate runs and their candidates are
-- deleted (the narrow CHECK would reject them — down is an operator-owned,
-- data-losing decision, exactly as 0006's down is), and the children are
-- re-pointed at a canon_runs rebuilt with the original CHECK.
DROP TABLE canon_verdicts;

ALTER TABLE canon_runs RENAME TO canon_runs_old;
DROP INDEX canon_runs_campaign;
DELETE FROM canon_runs_old WHERE kind = 'validate';

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

INSERT INTO canon_runs (id, campaign_id, session_id, kind, prompt_version, model, status, stop_reason, stats, error, created_at, updated_at)
SELECT id, campaign_id, session_id, kind, prompt_version, model, status, stop_reason, stats, error, created_at, updated_at FROM canon_runs_old;

CREATE TABLE canon_candidates_new (
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

INSERT INTO canon_candidates_new (id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at)
SELECT id, run_id, campaign_id, session_id, source_id, chunk_index, kind, payload, confidence, span_start, span_end, quote, checksum, created_at FROM canon_candidates;

DROP TABLE canon_candidates;
ALTER TABLE canon_candidates_new RENAME TO canon_candidates;

CREATE INDEX canon_candidates_campaign ON canon_candidates(campaign_id, kind, created_at);
CREATE INDEX canon_candidates_run ON canon_candidates(run_id);
CREATE INDEX canon_candidates_source ON canon_candidates(source_id);

CREATE TABLE canon_drops_new (
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

INSERT INTO canon_drops_new (id, run_id, campaign_id, source_id, chunk_index, kind, ref, reason, detail, created_at)
SELECT id, run_id, campaign_id, source_id, chunk_index, kind, ref, reason, detail, created_at FROM canon_drops;

DROP TABLE canon_drops;
ALTER TABLE canon_drops_new RENAME TO canon_drops;

CREATE INDEX canon_drops_run ON canon_drops(run_id);
CREATE INDEX canon_drops_source ON canon_drops(source_id);

CREATE TABLE model_outputs_new (
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

INSERT INTO model_outputs_new (id, run_id, stage, prompt_version, source_id, chunk_index, model, input_tokens, output_tokens, raw, created_at)
SELECT id, run_id, stage, prompt_version, source_id, chunk_index, model, input_tokens, output_tokens, raw, created_at FROM model_outputs;

DROP TABLE model_outputs;
ALTER TABLE model_outputs_new RENAME TO model_outputs;

CREATE INDEX model_outputs_run ON model_outputs(run_id);
CREATE INDEX model_outputs_source ON model_outputs(source_id);

DROP TABLE canon_runs_old;
