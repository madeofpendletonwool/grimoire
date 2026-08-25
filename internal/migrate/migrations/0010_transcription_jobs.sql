-- +goose Up
-- Transcription jobs (MAD-320): the resumable ledger behind the optional
-- audio→transcript hook. One row per uploaded recording. chunks is the
-- per-chunk transcript state as JSON ([{start, end, offset_ms, text,
-- segments, done}]), so a job interrupted by a restart resumes at the first
-- chunk it had not finished — the same idea as the canon extract ledger.
--
-- The raw audio itself never enters SQLite: it waits beside the database
-- file for the job to finish and is deleted once the transcript source
-- exists (kept only when the operator opts in), because session recordings
-- of real people are the most sensitive data this app touches. The finished
-- product is an ordinary session_sources row of kind 'transcript', exactly
-- what a pasted transcript produces, so everything downstream — spans,
-- extraction, the canon engine — is unchanged.

CREATE TABLE transcription_jobs (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES game_sessions(id) ON DELETE CASCADE,
    title        TEXT NOT NULL DEFAULT '',
    author       TEXT NOT NULL DEFAULT '',
    filename     TEXT NOT NULL DEFAULT '',
    format       TEXT NOT NULL DEFAULT '',
    language     TEXT NOT NULL DEFAULT '',
    audio_path   TEXT NOT NULL,
    audio_bytes  INTEGER NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('pending','running','completed','failed','cancelled')) DEFAULT 'pending',
    chunks_total INTEGER NOT NULL DEFAULT 0,
    chunks_done  INTEGER NOT NULL DEFAULT 0,
    chunks       TEXT NOT NULL DEFAULT '[]',
    source_id    TEXT,
    error        TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    finished_at  INTEGER
);

CREATE INDEX transcription_jobs_session ON transcription_jobs(session_id, created_at);
-- The worker takes the oldest pending job; this index is that query.
CREATE INDEX transcription_jobs_pending ON transcription_jobs(status, created_at);

-- +goose Down
DROP TABLE transcription_jobs;
