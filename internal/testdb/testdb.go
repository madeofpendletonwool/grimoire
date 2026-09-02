// Package testdb hands integration tests a fully migrated SQLite database
// for the cost of a file copy.
//
// The package harnesses used to open a fresh database per test and replay
// every goose migration into it, so suite time grew with migrations × tests:
// each new migration made every existing test slower. The template here
// inverts that curve. The full migration set runs once per test binary — Go
// runs each package's tests in its own process — and every test then copies
// the finished file. A copy costs milliseconds; a replay costs seconds.
//
// Tests keep the semantics of a database they built themselves: each copy is
// a private file, so writes never leak between tests and leak tests observe
// only their own rows. A copy is stamped at the latest migration version,
// so a migrate.Up over it is a no-op and the migrate.Down/Up cycle tests
// behave exactly as they do over a freshly replayed database.
package testdb

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

var (
	once     sync.Once
	tmplPath string
	tmplErr  error
)

// template builds the migrated template database the first time a test in
// this binary asks for one. The file lives for the process; the directory
// it sits in is cleaned by the OS, not by any test.
func template() (string, error) {
	once.Do(func() {
		dir, err := os.MkdirTemp("", "grimoire-testdb")
		if err != nil {
			tmplErr = err
			return
		}
		// index.OpenDB is the app's one definition of the DSN — WAL, busy
		// timeout, foreign keys, a single connection — so the template is
		// built through the same seam the real database opens through.
		db, err := index.OpenDB(filepath.Join(dir, "template.db"))
		if err != nil {
			tmplErr = err
			return
		}
		if err := migrate.Up(db); err != nil {
			_ = db.Close()
			tmplErr = err
			return
		}
		// Fold the WAL back into the main file so the template is one
		// self-contained file a plain copy can reproduce.
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			_ = db.Close()
			tmplErr = err
			return
		}
		if err := db.Close(); err != nil {
			tmplErr = err
			return
		}
		tmplPath = filepath.Join(dir, "template.db")
	})
	return tmplPath, tmplErr
}

// Path returns the path to a private copy of the migrated template database,
// in a directory that dies with the test. Callers that open their own store
// (index.Open and friends) hand the path straight through; callers that want
// a raw handle use Open.
func Path(t *testing.T) string {
	t.Helper()
	src, err := template()
	if err != nil {
		t.Fatalf("build template db: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read template db: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write test db: %v", err)
	}
	return dst
}

// Open opens a private, fully migrated database the way the app opens its
// own. The handle closes with the test.
func Open(t *testing.T) *sql.DB {
	t.Helper()
	db, err := index.OpenDB(Path(t))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
