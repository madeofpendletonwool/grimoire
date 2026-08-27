-- +goose Up
-- The command transcript (MAD-363): one row per natural-language command
-- exchange, so the referent stack is real and "undo that" is meaningful.
--
-- The referent stack is the reason this table exists. A pronoun in a
-- command ("put him in Blackwater") binds to the entity the previous
-- command PROPOSED -- not to prompt context, which a reload forgets and a
-- model paraphrases. referents stores those proposals as a JSON array of
-- {review_id, name, kind}: the review's own status is the referent's
-- lifetime. Open means still waiting on the DM's decision (the referent
-- asks rather than assumes), accepted means the entity now exists (the
-- review's result_ref is its id), dismissed means the referent expired.
--
-- kind mirrors the API response shapes: 'batch' (a proposal batch was
-- staged; batch_id points at it), 'question' (a clarifying question with
-- its candidates -- nothing was staged), 'unsupported' (the vocabulary
-- does not cover the text -- nothing was staged), 'undo' (an open command
-- batch was dropped -- nothing was ever written), 'written' (a spine row
-- the DM could have written by hand: a scene cast member, a merged name),
-- 'noop' (understood, but there was nothing to do). response keeps the
-- payload the surface rendered, so the transcript replays without
-- re-running anything.

CREATE TABLE command_log (
    id          TEXT PRIMARY KEY,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL DEFAULT '',
    text        TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('batch','question','unsupported','undo','written','noop')),
    verb        TEXT NOT NULL DEFAULT '',
    message     TEXT NOT NULL DEFAULT '',
    response    TEXT NOT NULL DEFAULT '',
    batch_id    TEXT REFERENCES proposal_batches(id) ON DELETE SET NULL,
    referents   TEXT NOT NULL DEFAULT '[]',
    created_at  INTEGER NOT NULL
);
CREATE INDEX command_log_campaign ON command_log(campaign_id, created_at);

-- +goose Down
-- The transcript is pure history: drop it.
DROP TABLE command_log;
