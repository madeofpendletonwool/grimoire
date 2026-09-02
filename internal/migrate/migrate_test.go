package migrate

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite" // same pure-Go driver the app opens the real file with
)

// openTemp opens a scratch database on disk with the same DSN shape the app
// uses (WAL, busy timeout, foreign keys), so migrations are exercised against
// the pragmas they will actually meet in production.
func openTemp(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return db
}

// tables lists the user tables (including virtual ones) present in the database.
func tables(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// columns lists the column names of a table.
func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out[name] = true
	}
	return out
}

// TestVersionNumbering is the build check: a duplicate or non-sequential
// version number fails here rather than surprising an operator mid-upgrade,
// where goose would either skip a file or apply two in an undefined order.
func TestVersionNumbering(t *testing.T) {
	if err := Validate(FS); err != nil {
		t.Fatalf("embedded migrations are not well numbered: %v", err)
	}
	got, err := Versions(FS)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no migrations embedded — the //go:embed pattern found nothing")
	}
}

// TestValidateRejects proves the numbering check has teeth. Each fixture is a
// mistake someone will eventually make on a branch: two people numbering the
// same migration, a skipped number, a set that does not start at 1, and a file
// with no version at all.
func TestValidateRejects(t *testing.T) {
	cases := map[string][]string{
		"duplicate":    {"0001_a.sql", "0001_b.sql"},
		"gap":          {"0001_a.sql", "0003_c.sql"},
		"not from one": {"0002_a.sql"},
		"unnumbered":   {"baseline.sql"},
		"zero":         {"0000_a.sql"},
		"non numeric":  {"v1_a.sql"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for _, f := range files {
				fsys["migrations/"+f] = &fstest.MapFile{}
			}
			if err := Validate(fsys); err == nil {
				t.Errorf("Validate accepted %v, want an error", files)
			}
		})
	}

	ok := fstest.MapFS{
		"migrations/0001_baseline.sql": &fstest.MapFile{},
		"migrations/0002_campaign.sql": &fstest.MapFile{},
	}
	if err := Validate(ok); err != nil {
		t.Errorf("Validate rejected a well-formed set: %v", err)
	}
}

// TestUpFromEmpty applies the whole set to a fresh database and checks the
// baseline really landed the schema the app expects.
func TestUpFromEmpty(t *testing.T) {
	db := openTemp(t)
	if err := Up(db); err != nil {
		t.Fatalf("up: %v", err)
	}

	have := tables(t, db)
	for _, want := range []string{
		"goose_db_version",
		"users", "sessions", "invites",
		"corpus_meta", "index_meta", "docs", "card_names", "entity_names", "doc_vectors",
		"reader_nodes", "reader_guides",
		"conversations", "chat_messages", "answer_cache",
		"shares", "share_snapshots", "reviews",
		"encounters", "bestiary",
		"cards", "cards_fts", "cards_build", "cards_fts_build", "decks",
	} {
		if !have[want] {
			t.Errorf("table %q missing after Up", want)
		}
	}

	// The maximal-schema rule: the columns the old addColumnIfMissing helpers
	// bolted on must be present on a fresh database too, or a fresh install
	// and an upgraded one would diverge.
	for table, cols := range map[string][]string{
		"users":           {"is_admin"},
		"encounters":      {"name", "notes"},
		"chat_messages":   {"rulings", "entities"},
		"answer_cache":    {"rulings", "entities"},
		"share_snapshots": {"entities"},
	} {
		have := columns(t, db, table)
		for _, c := range cols {
			if !have[c] {
				t.Errorf("%s.%s missing — baseline must declare the maximal schema", table, c)
			}
		}
	}

	v, err := Version(db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	all, err := Versions(FS)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if want := all[len(all)-1].Version; v != want {
		t.Errorf("stamped at %d, want %d", v, want)
	}
}

// TestUpIsIdempotent covers the boot path: every restart runs Up, and every
// restart after the first must be a clean no-op.
func TestUpIsIdempotent(t *testing.T) {
	db := openTemp(t)
	if err := Up(db); err != nil {
		t.Fatalf("first up: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('u1', 'keeper', 'hash', 1, 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, _ := Version(db)

	if err := Up(db); err != nil {
		t.Fatalf("second up: %v", err)
	}
	after, _ := Version(db)
	if before != after {
		t.Errorf("version moved on a no-op run: %d -> %d", before, after)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Errorf("seeded user count is %d after a re-run, want 1", n)
	}
}

// TestBaselineOverPopulatedDatabase is the upgrade case that matters: a box
// that has been running Grimoire since before migrations existed. The tables
// were created by the packages' own DDL and hold real rows; adopting the
// baseline must stamp the version and touch nothing else.
func TestBaselineOverPopulatedDatabase(t *testing.T) {
	db := openTemp(t)

	// Stand in for the pre-migration world: the packages' New() DDL, run at
	// boot, including the addColumnIfMissing shape (users created without
	// is_admin, then altered).
	preMigration := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`ALTER TABLE users ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS conversations (
			id TEXT PRIMARY KEY, user_id TEXT NOT NULL DEFAULT 'anonymous',
			corpus TEXT NOT NULL, title TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('u1', 'keeper', 'hash', 1, 1)`,
		`INSERT INTO conversations (id, user_id, corpus, title, created_at, updated_at)
			VALUES ('c1', 'u1', 'dnd', 'the vampire question', 1, 1)`,
	}
	for _, stmt := range preMigration {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("pre-migration setup %q: %v", stmt, err)
		}
	}

	if err := Up(db); err != nil {
		t.Fatalf("up over a populated database: %v", err)
	}

	var username, title string
	if err := db.QueryRow(`SELECT username FROM users WHERE id = 'u1'`).Scan(&username); err != nil {
		t.Fatalf("account did not survive the baseline: %v", err)
	}
	if username != "keeper" {
		t.Errorf("username is %q, want %q", username, "keeper")
	}
	if err := db.QueryRow(`SELECT title FROM conversations WHERE id = 'c1'`).Scan(&title); err != nil {
		t.Fatalf("conversation did not survive the baseline: %v", err)
	}
	if title != "the vampire question" {
		t.Errorf("title is %q, want %q", title, "the vampire question")
	}

	// The tables the pre-migration database did not have are created anyway.
	if have := tables(t, db); !have["decks"] || !have["bestiary"] {
		t.Error("baseline did not create the tables the old database was missing")
	}

	v, err := Version(db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v < 1 {
		t.Errorf("goose_db_version not stamped: %d", v)
	}
}

// TestUpPreservesEncounters is MAD-378's round-trip acceptance: a database
// from before migration 0026 holds encounters in the old owner-scoped shape,
// and adopting the campaign columns must leave every one of those rows
// byte-identical on the old columns — with the new columns defaulted, never
// rewritten. 0026 rebuilds the table rather than ALTERing it (the
// schema-compat ordering is why), so this is the test that proves the copy
// back loses nothing.
func TestUpPreservesEncounters(t *testing.T) {
	db := openTemp(t)

	// The pre-0026 shape: the 0001 columns, as an old install's table was.
	pre := `CREATE TABLE IF NOT EXISTS encounters (
		id          TEXT PRIMARY KEY,
		owner_id    TEXT NOT NULL,
		name        TEXT NOT NULL DEFAULT '',
		notes       TEXT NOT NULL DEFAULT '',
		party       TEXT NOT NULL DEFAULT '[]',
		monsters    TEXT NOT NULL DEFAULT '[]',
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS encounters_owner ON encounters(owner_id, updated_at);`
	for _, stmt := range []string{
		pre,
		`INSERT INTO encounters (id, owner_id, name, notes, party, monsters, created_at, updated_at)
		 VALUES ('e1', 'keeper', 'Ambush on the Triboar Trail', '## The pitch',
		     '[1,1,2]', '[{"name":"Goblin","cr":"1/4","count":4}]', 1000, 2000)`,
		`INSERT INTO encounters (id, owner_id, name, notes, party, monsters, created_at, updated_at)
		 VALUES ('e2', 'keeper', 'Empty one', '', '[]', '[]', 3000, 4000)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("pre-0026 setup: %v", err)
		}
	}

	if err := Up(db); err != nil {
		t.Fatalf("up over a pre-0026 encounters table: %v", err)
	}

	// The campaign columns arrived, with their defaults.
	cols := columns(t, db, "encounters")
	for _, c := range []string{"campaign_id", "session_event_id", "scene_id", "objective", "terrain", "status"} {
		if !cols[c] {
			t.Errorf("encounters.%s missing after Up", c)
		}
	}

	// Every old row survived the rebuild unchanged on the old columns…
	rows, err := db.Query(`SELECT id, owner_id, name, notes, party, monsters, created_at, updated_at
		FROM encounters ORDER BY id`)
	if err != nil {
		t.Fatalf("select encounters: %v", err)
	}
	defer rows.Close()
	want := map[string][7]any{
		"e1": {"keeper", "Ambush on the Triboar Trail", "## The pitch", "[1,1,2]", `[{"name":"Goblin","cr":"1/4","count":4}]`, int64(1000), int64(2000)},
		"e2": {"keeper", "Empty one", "", "[]", "[]", int64(3000), int64(4000)},
	}
	got := map[string][7]any{}
	for rows.Next() {
		var id string
		var v [7]any
		if err := rows.Scan(&id, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6]); err != nil {
			t.Fatalf("scan encounter: %v", err)
		}
		got[id] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("encounters after Up: %d rows, want %d", len(got), len(want))
	}
	for id, w := range want {
		if g, ok := got[id]; !ok || g != w {
			t.Errorf("encounter %q did not round-trip:\n got %+v\nwant %+v", id, got[id], w)
		}
	}

	// …and took the new defaults: still nobody's campaign, still planned.
	var campaign, sessionEvent, scene, objective, terrain, status string
	if err := db.QueryRow(`SELECT campaign_id, session_event_id, scene_id, objective, terrain, status
		FROM encounters WHERE id = 'e1'`).
		Scan(&campaign, &sessionEvent, &scene, &objective, &terrain, &status); err != nil {
		t.Fatalf("scan new columns: %v", err)
	}
	if campaign != "" || sessionEvent != "" || scene != "" || objective != "" || terrain != "{}" || status != "planned" {
		t.Errorf("new columns not defaulted: campaign=%q session_event=%q scene=%q objective=%q terrain=%q status=%q",
			campaign, sessionEvent, scene, objective, terrain, status)
	}
}

// TestUpRefusesBadMigration proves a failing migration surfaces as an error
// rather than a partially applied schema. The boot path treats this as fatal,
// which is the whole point of running Up before serving.
func TestUpRefusesBadMigration(t *testing.T) {
	db := openTemp(t)
	broken := fstest.MapFS{
		"migrations/0001_broken.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\nCREATE TABLE ok (id INTEGER PRIMARY KEY);\nTHIS IS NOT SQL;\n")},
	}
	err := up(db, broken)
	if err == nil {
		t.Fatal("expected a bad migration to fail")
	}
	if !strings.Contains(err.Error(), "migrate up") {
		t.Errorf("error is not wrapped by this package: %v", err)
	}
	// One transaction per migration: the statement before the bad one must
	// have rolled back with it.
	if tables(t, db)["ok"] {
		t.Error("a failed migration left a partially applied schema behind")
	}
}

// TestStatusReports checks the operator-facing status output names the
// migrations rather than failing silently.
func TestStatusReports(t *testing.T) {
	db := openTemp(t)
	if err := Up(db); err != nil {
		t.Fatalf("up: %v", err)
	}
	var buf bytes.Buffer
	if err := Status(db, &buf); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(buf.String(), "0001_baseline.sql") {
		t.Errorf("status output does not mention the baseline:\n%s", buf.String())
	}
}
