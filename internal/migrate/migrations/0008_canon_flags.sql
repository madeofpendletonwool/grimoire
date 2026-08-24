-- +goose Up
-- The deterministic engine's flag ledger (MAD-309). One row per finding the
-- engine reports, keyed by (check_code, record_kind, record_id) — the key
-- Arda's ledger uses, ported exactly.
--
-- Ledger semantics (docs/campaign/model.md, canon_flags):
--   * a re-run refreshes severity and message but NEVER clobbers a human
--     decision — status stays 'accepted' or 'dismissed' forever;
--   * an open finding the engine stops reporting is marked 'cleared';
--   * a cleared finding that reappears reopens as 'open'.
--
-- The DM's decision fields live on the row itself; the review queue (MAD-310)
-- layers its canon_reviews items on top of this ledger for engine_flag items.

CREATE TABLE canon_flags (
    id            TEXT PRIMARY KEY,
    campaign_id   TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    check_code    TEXT NOT NULL,
    record_kind   TEXT NOT NULL,
    record_id     TEXT NOT NULL,
    severity      TEXT NOT NULL CHECK (severity IN ('error','review','warning')),
    message       TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','accepted','dismissed','cleared')),
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,
    cleared_at    INTEGER,
    decided_by    TEXT NOT NULL DEFAULT '',
    decided_at    INTEGER,
    decision_note TEXT NOT NULL DEFAULT '',
    UNIQUE (campaign_id, check_code, record_kind, record_id)
);

CREATE INDEX canon_flags_campaign ON canon_flags(campaign_id, status);

-- +goose Down
DROP TABLE canon_flags;
