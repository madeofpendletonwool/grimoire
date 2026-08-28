-- +goose Up
-- Campaign OS layout state (MAD-366): saved workspaces and per-user
-- preferences. The front end became a tiling window manager, so "which tools
-- are open, where" is now state worth keeping — and keeping it here rather
-- than in localStorage is the point: a DM preps on a laptop and runs the
-- session from a tablet, and the layout should follow the account.

-- One row per workspace slot. slot is the Alt+N index, so the primary key is
-- (user, corpus, slot) and switching games swaps the whole set — corpus
-- separation lives in this key, not in a list of foreign surfaces to close.
--
-- tree is the serialised container tree that web/static/js/wm/tree.js owns.
-- Deliberately opaque to SQL: the layout's shape is a front-end concern and
-- the server only checks that it is well-formed (see internal/uistate), so
-- adding a node kind does not need a migration.
CREATE TABLE user_layouts (
    user_id    TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    corpus     TEXT    NOT NULL CHECK (corpus IN ('mtg','dnd')),
    slot       INTEGER NOT NULL CHECK (slot BETWEEN 1 AND 9),
    name       TEXT    NOT NULL,
    tree       TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, corpus, slot)
);

-- Small typed-enough key/value for interface preferences: the chosen scene,
-- the pixel cursor, the corpus for new chats, the active workspace slot.
-- These lived in localStorage (state.js, scene.js) and so did not survive a
-- new browser; they are per-user, never per-campaign, which is why this is
-- not campaigns.settings — 0016 already recorded why that blob is the wrong
-- home for anything typed.
CREATE TABLE user_prefs (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, key)
);

-- +goose Down
-- Rolling back loses saved layouts and interface preferences. Both regenerate
-- from presets and defaults on next sign-in, so unlike 0016 this is cheap;
-- it is still operator-owned (`grimoire migrate down`).
DROP TABLE user_prefs;
DROP TABLE user_layouts;
