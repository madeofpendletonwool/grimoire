-- +goose Up
-- The Magic table core (MAD-322, stage 1 of MAD-321): the game row, the
-- seats, the event log, and four small ledgers around it. The contract these
-- tables implement is docs/table/model.md; the engine that writes them lands
-- with MAD-323 in internal/table, which creates no tables of its own,
-- following the pattern migrations 0002 and up set.
--
-- Prefix is mtg_ on purpose: game_sessions already means a D&D session and
-- sessions already means a login session. No exceptions.
--
-- Additive only: CREATE TABLE and indexes, nothing else touched, so it
-- applies cleanly on a fresh database and is inert on a populated one.

-- The container. Lifecycle metadata only: current turn, phase, step,
-- priority, who is alive — all of that is fold state over mtg_events and is
-- never stored. format is open vocabulary (Commander first; the engine is
-- format-agnostic underneath) and settings JSON carries variant config the
-- way campaigns.settings does.
CREATE TABLE mtg_games (
    id            TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL DEFAULT '',
    format        TEXT NOT NULL DEFAULT 'commander',
    starting_life INTEGER NOT NULL DEFAULT 40,
    status        TEXT NOT NULL DEFAULT 'setup' CHECK (status IN ('setup','active','finished')),
    settings      TEXT NOT NULL DEFAULT '{}',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    started_at    INTEGER,
    ended_at      INTEGER
);

CREATE INDEX mtg_games_owner ON mtg_games(owner_id, updated_at);

-- Seats carry setup; the fold carries play. Turn order is position
-- ascending. user_id binds a seat to an account in a pod (NULL = a local
-- seat tracked by the owner's client; the owner is entitled to everything,
-- the DM analog). deck_id attaches the known-card universe (MAD-329) and
-- makes library composition known — order never is. commander is
-- denormalized because commander damage and tax bookkeeping need it on
-- every cast; starting_life per seat overrides the game default.
CREATE TABLE mtg_seats (
    id            TEXT PRIMARY KEY,
    game_id       TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    user_id       TEXT REFERENCES users(id) ON DELETE SET NULL,
    name          TEXT NOT NULL DEFAULT '',
    deck_id       TEXT REFERENCES decks(id) ON DELETE SET NULL,
    commander     TEXT NOT NULL DEFAULT '',
    starting_life INTEGER,
    joined_at     INTEGER NOT NULL,
    UNIQUE (game_id, position)
);

CREATE INDEX mtg_seats_user ON mtg_seats(user_id);
CREATE INDEX mtg_seats_deck ON mtg_seats(deck_id);

-- The log. THE table: the only writer of game state (ADR 9). One writer
-- assigns ord per game — contiguous, starting at 1 — in a single transaction
-- with the games.updated_at bump; multiplayer is replication of this stream,
-- never concurrent writing. cause is the Action JSON that produced the row
-- (source, confidence, disposition), so every log entry is self-contained
-- and amend prefills from the row itself. visibility is the hidden-zone
-- gate (ADR 13): a seat row is selected only for the seat named by
-- visible_seat, in SQL, never post-filtered and never entrusted to a
-- prompt.
--
-- kind is deliberately not CHECK-constrained: the taxonomy lives in
-- docs/table/model.md and in the Go type system, the reducer is the single
-- writer, and the vocabulary grows with the engine. The stable small
-- vocabularies below are CHECKed, mirroring how facts CHECK confidence but
-- leave predicate open.
CREATE TABLE mtg_events (
    id           TEXT PRIMARY KEY,
    game_id      TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    ord          INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    actor_seat   INTEGER,
    source       TEXT NOT NULL DEFAULT 'system' CHECK (source IN ('tap','grammar','llm','voice','manual','system')),
    cause        TEXT NOT NULL DEFAULT '',
    payload      TEXT NOT NULL,
    visibility   TEXT NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','seat')),
    visible_seat INTEGER,
    created_at   INTEGER NOT NULL,
    UNIQUE (game_id, ord),
    CHECK ((visibility = 'public') = (visible_seat IS NULL))
);

CREATE INDEX mtg_events_kind ON mtg_events(game_id, kind);

-- The unresolved tray (MAD-331): a clarification that could not be reduced
-- to tappable answers is parked open and play continues — a question that
-- stops the game is worse than a wrong board state. options is a JSON array
-- of tappable answers; context carries the candidate action and the ordinal
-- it concerns, so a rewind past that ordinal auto-dismisses open rows.
CREATE TABLE mtg_pending (
    id            TEXT PRIMARY KEY,
    game_id       TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    question      TEXT NOT NULL,
    options       TEXT NOT NULL DEFAULT '[]',
    context       TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','answered','dismissed')),
    answer        TEXT NOT NULL DEFAULT '',
    answered_seat INTEGER,
    created_at    INTEGER NOT NULL,
    answered_at   INTEGER
);

CREATE INDEX mtg_pending_game ON mtg_pending(game_id, status);

-- The per-game identity cache (MAD-329/331): a normalized spoken form maps
-- to a canonical card name once, so the same mumble is never re-inferred
-- and never re-billed. method records which universe matched
-- (deck_exact | deck_fuzzy | global | manual), per ADR 12's resolution
-- order. Survives rewind — a corrected card is still the resolution of that
-- mumble. No FK to cards(name): the card index is bulk-replaced on re-index
-- while this cache is game history.
CREATE TABLE mtg_name_resolutions (
    game_id     TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    spoken      TEXT NOT NULL,
    card_name   TEXT NOT NULL,
    method      TEXT NOT NULL CHECK (method IN ('deck_exact','deck_fuzzy','global','manual')),
    confidence  REAL NOT NULL DEFAULT 0,
    resolved_at INTEGER NOT NULL,
    PRIMARY KEY (game_id, spoken)
);

CREATE INDEX mtg_name_resolutions_card ON mtg_name_resolutions(card_name);

-- The trigger registry (MAD-335), cross-game and install-wide: card
-- knowledge is universal, like the rules corpora. A trigger is registered
-- once per card against a structural event kind the engine already emits
-- (LAND_PLAYED, CREATURE_ETB, CAST, ATTACKERS_DECLARED, DIED, STEP_ENTERED
-- with a step condition in effect), never simulated from oracle text —
-- ADR 11's declared-vs-simulated line. origin records where it came from:
-- declared by a human, or model-proposed and confirmed by one.
CREATE TABLE mtg_trigger_registry (
    card_name    TEXT NOT NULL,
    event_kind   TEXT NOT NULL,
    effect       TEXT NOT NULL,
    origin       TEXT NOT NULL CHECK (origin IN ('declared','confirmed')),
    confirmed_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (card_name, event_kind)
);

-- The judge log (MAD-338): a ruling anchored to the event ordinal it
-- concerns, so the game's history carries its own rulings. Rows survive a
-- rewind — a human record is never clobbered by a truncate, the same
-- semantics as a decided canon review.
CREATE TABLE mtg_rulings (
    id         TEXT PRIMARY KEY,
    game_id    TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    ord        INTEGER NOT NULL,
    ruled_by   TEXT REFERENCES users(id) ON DELETE SET NULL,
    note       TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX mtg_rulings_game ON mtg_rulings(game_id, ord);

-- Private per-seat scratch (MAD-337): editable, latest-wins, and never
-- another seat's view or another seat's prompt.
CREATE TABLE mtg_seat_notes (
    game_id    TEXT NOT NULL REFERENCES mtg_games(id) ON DELETE CASCADE,
    seat       INTEGER NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (game_id, seat)
);

-- +goose Down
-- Dropping the mtg tables deletes every recorded game on the box. Rolling
-- back is therefore a destructive, operator-owned decision, exactly as it
-- is for 0002 and 0004 — it only happens through `grimoire migrate down`
-- with intent.
DROP TABLE mtg_seat_notes;
DROP TABLE mtg_rulings;
DROP TABLE mtg_trigger_registry;
DROP TABLE mtg_name_resolutions;
DROP TABLE mtg_pending;
DROP TABLE mtg_events;
DROP TABLE mtg_seats;
DROP TABLE mtg_games;