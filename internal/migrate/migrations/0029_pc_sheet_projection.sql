-- +goose Up
-- The typed character sheet's query projection (MAD-418, stage 1 of MAD-417).
--
-- The sheet itself is data on the entity payload — the house pattern
-- (docs/campaign/model.md): a pc's definition is a typed Go struct
-- (internal/sheet) serialized under the payload's "sheet" key, because a
-- sheet and a place block have almost nothing in common and a wide table of
-- mostly-NULL columns would be worse than honest JSON. The full decision is
-- ADR 15 in docs/decisions.md.
--
-- This table is NOT the sheet. It is a narrow, always-derivable projection
-- of it: the handful of numbers query surfaces want to filter and sort on —
-- party boards, encounter budgets, future mechanical stages — without
-- parsing JSON per row. The party block's remaining-slots keys stay on the
-- payload untouched; runtime state is the resource ledger's job (MAD-419),
-- never this table's.
--
-- It is maintained, never source of truth: every sheet write refreshes the
-- one row, and server start re-derives the whole table from the payloads
-- (internal/sheet.SyncProjections). A dropped table costs a boot, not data.
-- Backfill deliberately happens there and not in SQL: reading a payload is
-- typed-struct work with validation rules, and a json_extract one-liner
-- would silently invent numbers the typed reader rejects.
--
-- structured is the visible "unstructured sheet" marker made queryable: 1
-- when the payload carries a typed sheet, 0 when it carries only the legacy
-- top-level party keys (or nothing). Legacy rows still project level / hp /
-- ac / classes when those keys are present — reading keys that already
-- exist is derivation, not invention.
--
-- classes is a rendered label ("fighter 8/wizard 2"), for humans and
-- filters; the multiclass list itself lives on the sheet.

CREATE TABLE pc_sheet_projection (
    entity_id   TEXT PRIMARY KEY REFERENCES entities(id) ON DELETE CASCADE,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    level       INTEGER NOT NULL DEFAULT 0,
    classes     TEXT NOT NULL DEFAULT '',
    max_hp      INTEGER NOT NULL DEFAULT 0,
    ac          INTEGER NOT NULL DEFAULT 0,
    structured  INTEGER NOT NULL DEFAULT 0,
    synced_at   INTEGER NOT NULL
);

CREATE INDEX pc_sheet_projection_campaign ON pc_sheet_projection(campaign_id);

-- +goose Down
-- The projection is derivable from the payloads at boot; dropping it loses
-- no data.
DROP TABLE pc_sheet_projection;
