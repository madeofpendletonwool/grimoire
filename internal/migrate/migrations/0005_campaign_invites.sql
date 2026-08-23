-- +goose Up
-- Campaign invites (MAD-305). An invite may now carry an optional campaign
-- binding: redeeming it writes a campaign_members row alongside the account,
-- so a DM mints a player invite link exactly the way the keeper mints an
-- account invite today (MAD-302 ADR 4). No second password path, no second
-- session cookie — the same invites table and the same redeem flow, plus one
-- binding row.
--
-- The table is declared with IF NOT EXISTS because internal/auth's own schema
-- declares it too (the pre-migration compatibility pattern the baseline set
-- with users.is_admin): an install whose stores booted before the runner
-- creates it here is already in shape, and a fresh one takes it from this
-- migration. The shapes are identical by contract — schema_compat_test pins
-- them together.
--
-- An invite with no binding row is the plain account invite the keeper has
-- always minted.

CREATE TABLE IF NOT EXISTS campaign_invites (
    invite_id   TEXT PRIMARY KEY REFERENCES invites(id) ON DELETE CASCADE,
    campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('dm','player','observer')),
    created_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS campaign_invites_campaign ON campaign_invites(campaign_id);

-- +goose Down
DROP TABLE campaign_invites;
