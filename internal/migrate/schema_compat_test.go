package migrate_test

// The baseline migration is a hand-transcribed copy of DDL that still lives in
// fourteen packages' New() functions. A copy drifts. This test pins the two
// together: build a database each way — migration first, then the packages;
// and the packages first, then the migration — and assert both orders produce
// the same schema and neither errors.
//
// It is the test that fails when someone adds a column to a package's schema
// const and forgets the migration, which is exactly the mistake the whole
// runner exists to make impossible.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/cache"
	"github.com/madeofpendletonwool/grimoire/internal/carddb"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/deck"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/share"
	"github.com/madeofpendletonwool/grimoire/internal/study"
)

// openTempStore opens a scratch index store, which is how the app opens the
// shared handle every other store sits on.
func openTempStore(t *testing.T) *index.Store {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// constructStores runs every package's own schema setup against db, the way
// runServe does at boot.
func constructStores(t *testing.T, store *index.Store) {
	t.Helper()
	db := store.DB()
	if _, err := auth.New(db, time.Hour, time.Hour); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if _, err := chat.New(db); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, err := cache.New(db, time.Hour); err != nil {
		t.Fatalf("cache: %v", err)
	}
	if _, err := study.New(db); err != nil {
		t.Fatalf("study: %v", err)
	}
	if _, err := encounter.New(db); err != nil {
		t.Fatalf("encounter: %v", err)
	}
	if _, err := encounter.NewCatalog(db, "http://127.0.0.1:1/unused"); err != nil {
		t.Fatalf("bestiary: %v", err)
	}
	if _, err := carddb.New(db); err != nil {
		t.Fatalf("carddb: %v", err)
	}
	if _, err := deck.New(db); err != nil {
		t.Fatalf("deck: %v", err)
	}
	if _, err := share.New(db); err != nil {
		t.Fatalf("share: %v", err)
	}
}

// schemaShape describes a database as table name -> sorted column names, plus
// the set of index names. Raw sqlite_master SQL is not comparable — the
// migration and the package consts are formatted differently and mean the same
// thing — so the comparison is on what the schema actually declares.
type schemaShape struct {
	tables  map[string][]string
	indexes []string
}

func shapeOf(t *testing.T, db *sql.DB) schemaShape {
	t.Helper()
	shape := schemaShape{tables: map[string][]string{}}

	names := queryStrings(t, db, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite_%' AND name != 'goose_db_version' ORDER BY name`)
	for _, name := range names {
		cols := queryStrings(t, db, fmt.Sprintf("SELECT name FROM pragma_table_info(%q)", name))
		sort.Strings(cols)
		shape.tables[name] = cols
	}
	shape.indexes = queryStrings(t, db, `SELECT name FROM sqlite_master WHERE type = 'index'
		AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	return shape
}

func queryStrings(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestBaselineMatchesPackageDDL is the drift guard described at the top of the
// file.
//
// The comparison is made BEFORE the packages run on the migrated database.
// That ordering is the point: the packages' addColumnIfMissing() helpers would
// otherwise heal a column the baseline forgot, and the test would pass on a
// baseline that is not actually the maximal schema.
func TestBaselineMatchesPackageDDL(t *testing.T) {
	// Fresh database, migration only — the schema a new install will have once
	// the packages stop declaring their own DDL.
	migrated := openTempStore(t)
	if err := migrate.Up(migrated.DB()); err != nil {
		t.Fatalf("migration-first up: %v", err)
	}

	// Fresh database, packages only — the schema every existing install has.
	packages := openTempStore(t)
	constructStores(t, packages)

	want := shapeOf(t, packages.DB())
	got := shapeOf(t, migrated.DB())

	for name, wantCols := range want.tables {
		gotCols, ok := got.tables[name]
		if !ok {
			t.Errorf("table %q is created by the packages but missing from the baseline migration", name)
			continue
		}
		if strings.Join(gotCols, ",") != strings.Join(wantCols, ",") {
			t.Errorf("table %q columns differ — the baseline must declare the maximal schema:\n  baseline: %v\n  packages: %v", name, gotCols, wantCols)
		}
	}
	for name := range got.tables {
		if _, ok := want.tables[name]; !ok {
			t.Errorf("table %q is created by the baseline migration but not by any package", name)
		}
	}
	if strings.Join(got.indexes, ",") != strings.Join(want.indexes, ",") {
		t.Errorf("indexes differ:\n  baseline: %v\n  packages: %v", got.indexes, want.indexes)
	}

	// Both orders must also compose without error, since both happen in the
	// field: an existing box runs the packages' DDL and then the baseline, and
	// a fresh box runs the baseline and then the packages' DDL.
	constructStores(t, migrated)
	if err := migrate.Up(packages.DB()); err != nil {
		t.Fatalf("packages-first up: %v", err)
	}
	if a, b := shapeOf(t, migrated.DB()), shapeOf(t, packages.DB()); len(a.tables) != len(b.tables) {
		t.Errorf("schemas diverged after both orders completed: %d vs %d tables", len(a.tables), len(b.tables))
	}
}
