-- +goose Up
-- Handouts and maps (MAD-490, stage 4 of MAD-319): the one genuinely new
-- object the player portal adds. A handout is material the DM hands to the
-- party — a letter read aloud, a map unrolled on the table — which is a
-- visibility question, not a world-graph node: it joins no entity, facts or
-- awareness machinery, and never becomes extraction input.
--
-- The lifecycle is the portal's own rule, in the row itself: draft is
-- DM-only (the row exists, no player read can address it), published is
-- readable by every member, retired leaves the portal but keeps the row for
-- history. published_at stamps the last hand-out; unpublishing clears it.
--
-- Body is markdown for reading handouts; a map carries its image instead.
-- The image bytes never enter SQLite — they land in the handouts directory
-- beside the database file (the transcription rule, ADR 5's shape), and the
-- row holds the reference: a generated file name, the sniffed MIME type,
-- and the size. No CDN, no third-party loader (design invariant 6).
CREATE TABLE handouts (
    id           TEXT PRIMARY KEY,
    campaign_id  TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('handout','map')),
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    image_ref    TEXT NOT NULL DEFAULT '',
    image_mime   TEXT NOT NULL DEFAULT '',
    image_bytes  INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','published','retired')),
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    published_at INTEGER,
    updated_at   INTEGER NOT NULL
);

CREATE INDEX handouts_campaign ON handouts(campaign_id, status, created_at);
CREATE INDEX handouts_kind ON handouts(campaign_id, kind) WHERE status = 'published';

-- +goose Down
-- The rows go, the campaign stays. Image files in the handouts directory
-- are the operator's to sweep; the table no longer names them.
DROP TABLE handouts;
