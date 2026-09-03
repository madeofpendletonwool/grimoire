-- Baseline: adopt the schema Grimoire already had before migrations existed.
--
-- Every statement here is IF NOT EXISTS on purpose. On a fresh database this
-- migration *creates* the whole schema; on an existing one it is a *no-op*
-- that only stamps goose_db_version at 1. There is no stamping logic, no data
-- movement, and no install loses an account, a conversation or a saved deck by
-- upgrading into migrations.
--
-- The declaration below is the MAXIMAL current schema — it includes the
-- columns the old addColumnIfMissing() helpers bolted on after the fact
-- (users.is_admin, encounters.name, encounters.notes, chat_messages.rulings,
-- chat_messages.entities, answer_cache.rulings, answer_cache.entities,
-- share_snapshots.entities, and the campaign-chat pins
-- conversations.campaign_id, conversations.scope, chat_messages.campaign).
-- Those helpers still run at boot, so any live database already has them;
-- declaring anything narrower would make a fresh database differ from an
-- upgraded one.
--
-- Existing packages keep their own New() DDL for now. Stripping it is a
-- deliberate follow-up, not part of adopting the runner.

-- +goose Up

-- ---------------------------------------------------------------- accounts --
CREATE TABLE IF NOT EXISTS users (
	id            TEXT PRIMARY KEY,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS invites (
	id         TEXT PRIMARY KEY,
	code_hash  TEXT NOT NULL UNIQUE,
	created_by TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER,
	used_by    TEXT REFERENCES users(id) ON DELETE SET NULL,
	used_at    INTEGER,
	note       TEXT
);
CREATE INDEX IF NOT EXISTS invites_created_by ON invites(created_by);

-- ------------------------------------------------------------- rules index --
CREATE TABLE IF NOT EXISTS corpus_meta (
	corpus        TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	version       TEXT NOT NULL,
	source_url    TEXT NOT NULL,
	record_count  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS index_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS docs USING fts5(
	corpus UNINDEXED,
	number,
	title,
	body,
	source UNINDEXED,
	tokenize = 'porter unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS card_names (
	name TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS entity_names (
	name TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS doc_vectors (
	corpus    TEXT NOT NULL,
	number    TEXT NOT NULL,
	title     TEXT NOT NULL,
	body      TEXT NOT NULL,
	source    TEXT NOT NULL,
	embedding BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS doc_vectors_corpus ON doc_vectors(corpus);

-- ------------------------------------------------------------------ reader --
CREATE TABLE IF NOT EXISTS reader_nodes (
	id INTEGER PRIMARY KEY,
	corpus       TEXT NOT NULL,
	guide        TEXT NOT NULL,
	guide_title  TEXT NOT NULL,
	guide_kind   TEXT NOT NULL DEFAULT '',
	number       TEXT NOT NULL,
	title        TEXT NOT NULL,
	level        INTEGER NOT NULL,
	position     INTEGER NOT NULL,
	body         TEXT NOT NULL DEFAULT '',
	source       TEXT NOT NULL DEFAULT '',
	UNIQUE(corpus, guide, number)
);
CREATE INDEX IF NOT EXISTS reader_nodes_toc ON reader_nodes(corpus, guide, position);

CREATE TABLE IF NOT EXISTS reader_guides (
	corpus      TEXT NOT NULL,
	guide       TEXT NOT NULL,
	title       TEXT NOT NULL,
	kind        TEXT NOT NULL DEFAULT '',
	position    INTEGER NOT NULL DEFAULT 0,
	node_count  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(corpus, guide)
);

-- -------------------------------------------------------------------- chat --
CREATE TABLE IF NOT EXISTS conversations (
	id          TEXT PRIMARY KEY,
	user_id     TEXT NOT NULL DEFAULT 'anonymous',
	corpus      TEXT NOT NULL,
	campaign_id TEXT NOT NULL DEFAULT '',
	scope       TEXT NOT NULL DEFAULT '',
	title       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS conversations_owner ON conversations(user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS chat_messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
	role            TEXT NOT NULL,
	content         TEXT NOT NULL,
	sources         TEXT NOT NULL DEFAULT '',
	cards           TEXT NOT NULL DEFAULT '',
	entities        TEXT NOT NULL DEFAULT '',
	rulings         TEXT NOT NULL DEFAULT '',
	campaign        TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS chat_messages_thread ON chat_messages(conversation_id, id);

-- ------------------------------------------------------------ answer cache --
CREATE TABLE IF NOT EXISTS answer_cache (
	key        TEXT PRIMARY KEY,
	corpus     TEXT NOT NULL,
	answer     TEXT NOT NULL,
	sources    TEXT NOT NULL DEFAULT '',
	cards      TEXT NOT NULL DEFAULT '',
	entities   TEXT NOT NULL DEFAULT '',
	rulings    TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS answer_cache_corpus ON answer_cache(corpus, created_at);

-- ------------------------------------------------------------------ shares --
CREATE TABLE IF NOT EXISTS shares (
	token      TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL,
	chat_id    TEXT NOT NULL,
	message_id INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS shares_owner ON shares(user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS share_snapshots (
	token      TEXT PRIMARY KEY REFERENCES shares(token) ON DELETE CASCADE,
	question   TEXT NOT NULL DEFAULT '',
	answer     TEXT NOT NULL DEFAULT '',
	corpus     TEXT NOT NULL DEFAULT '',
	sources    TEXT NOT NULL DEFAULT '',
	cards      TEXT NOT NULL DEFAULT '',
	entities   TEXT NOT NULL DEFAULT '',
	rulings    TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

-- ------------------------------------------------------------------- study --
CREATE TABLE IF NOT EXISTS reviews (
	user_id      TEXT NOT NULL,
	concept_key  TEXT NOT NULL,
	corpus       TEXT NOT NULL,
	topic        TEXT NOT NULL,
	reps         INTEGER NOT NULL DEFAULT 0,
	lapses       INTEGER NOT NULL DEFAULT 0,
	interval_days REAL NOT NULL DEFAULT 0,
	ease         REAL NOT NULL DEFAULT 2.5,
	due_at       INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	PRIMARY KEY (user_id, concept_key)
);
CREATE INDEX IF NOT EXISTS reviews_deck ON reviews(user_id, corpus, topic, due_at);

-- -------------------------------------------------------------- encounters --
CREATE TABLE IF NOT EXISTS encounters (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	notes       TEXT NOT NULL DEFAULT '',
	party       TEXT NOT NULL DEFAULT '[]',
	monsters    TEXT NOT NULL DEFAULT '[]',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS encounters_owner ON encounters(owner_id, updated_at);

CREATE TABLE IF NOT EXISTS bestiary (
	key         TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	cr          TEXT NOT NULL DEFAULT '',
	cr_num      REAL NOT NULL DEFAULT 0,
	xp          INTEGER NOT NULL DEFAULT 0,
	type        TEXT NOT NULL DEFAULT '',
	data        TEXT NOT NULL,
	synced_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS bestiary_cr ON bestiary(cr_num);
CREATE TABLE IF NOT EXISTS bestiary_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- ------------------------------------------------------------ deck builder --
-- cards_build / cards_fts_build are the staging twins carddb.New() creates
-- alongside the live tables; Populate drops and recreates them per rebuild.
-- They are declared here so a fresh database matches an upgraded one exactly.
-- A future migration that alters `cards` must alter `cards_build` with it.
CREATE TABLE IF NOT EXISTS cards (
	name             TEXT PRIMARY KEY,
	mana_cost        TEXT NOT NULL DEFAULT '',
	mana_value       REAL NOT NULL DEFAULT 0,
	type_line        TEXT NOT NULL DEFAULT '',
	oracle_text      TEXT NOT NULL DEFAULT '',
	color_identity   TEXT NOT NULL DEFAULT '',
	color_mask       INTEGER NOT NULL DEFAULT 0,
	edhrec_rank      INTEGER,
	edhrec_saltiness REAL NOT NULL DEFAULT 0,
	commander_legal  INTEGER NOT NULL DEFAULT 0,
	legal_commander  TEXT NOT NULL DEFAULT '',
	game_changer     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS cards_rank ON cards(edhrec_rank);

CREATE VIRTUAL TABLE IF NOT EXISTS cards_fts USING fts5(
	name,
	type_line,
	oracle_text,
	tokenize = 'porter unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS cards_build (
	name             TEXT PRIMARY KEY,
	mana_cost        TEXT NOT NULL DEFAULT '',
	mana_value       REAL NOT NULL DEFAULT 0,
	type_line        TEXT NOT NULL DEFAULT '',
	oracle_text      TEXT NOT NULL DEFAULT '',
	color_identity   TEXT NOT NULL DEFAULT '',
	color_mask       INTEGER NOT NULL DEFAULT 0,
	edhrec_rank      INTEGER,
	edhrec_saltiness REAL NOT NULL DEFAULT 0,
	commander_legal  INTEGER NOT NULL DEFAULT 0,
	legal_commander  TEXT NOT NULL DEFAULT '',
	game_changer     INTEGER NOT NULL DEFAULT 0
);

CREATE VIRTUAL TABLE IF NOT EXISTS cards_fts_build USING fts5(
	name,
	type_line,
	oracle_text,
	tokenize = 'porter unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS decks (
	id          TEXT PRIMARY KEY,
	owner_id    TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	commander   TEXT NOT NULL DEFAULT '',
	cards       TEXT NOT NULL DEFAULT '[]',
	notes       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS decks_owner ON decks(owner_id, updated_at);

-- +goose Down

-- The baseline adopts a schema that already existed, so there is no earlier
-- state to return to: dropping these tables would delete every account,
-- conversation, deck and encounter on the box. Rolling the baseline back
-- therefore only un-stamps the version — `up` re-applies it as a no-op.
SELECT 1;
