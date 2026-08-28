-- +goose Up
-- Downtime requests (MAD-368, stage 5.3 of MAD-315): the simulation tick
-- pointed at one character. One row per request — its identity, not its
-- answer: the deterministic result is re-derived from the campaign inputs,
-- the window, the character, the activity, the subject and the seed
-- (internal/downtime), never cached. What the row carries is the
-- bookkeeping: whose downtime, doing what, about what, over which window,
-- under which seed, against which snapshot digest, and where the proposal
-- went.
--
-- status is requested | staged | applied | discarded:
--   requested recorded, nothing staged, nothing written to the graph
--   staged    the outcomes live in one proposal_batch behind the review gate
--   applied   the batch was accepted; the clock moved by exactly the window
--   discarded the batch was dismissed; time did not pass
--
-- subject is NULL when the activity names no entity (work, recuperate); a
-- deleted subject sets it back to NULL rather than dangling.

CREATE TABLE downtime_requests (
    id              TEXT PRIMARY KEY,
    campaign_id     TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    character_id    TEXT NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    activity        TEXT NOT NULL,
    subject         TEXT REFERENCES entities(id) ON DELETE SET NULL,
    activity_text   TEXT NOT NULL DEFAULT '',
    days            INTEGER NOT NULL CHECK (days >= 1),
    from_day        INTEGER NOT NULL,
    to_day          INTEGER NOT NULL CHECK (to_day > from_day),
    seed            INTEGER NOT NULL,
    snapshot_digest TEXT NOT NULL,
    batch_id        TEXT REFERENCES proposal_batches(id) ON DELETE SET NULL,
    status          TEXT NOT NULL DEFAULT 'requested'
                    CHECK (status IN ('requested','staged','applied','discarded')),
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX downtime_requests_campaign ON downtime_requests(campaign_id, created_at);
CREATE INDEX downtime_requests_batch ON downtime_requests(batch_id);

-- +goose Down
-- Dropping the requests deletes the request ledger; the batches they staged
-- are proposal_batches rows and survive (the same rule 0012, 0016 and 0019
-- set).
DROP TABLE downtime_requests;
