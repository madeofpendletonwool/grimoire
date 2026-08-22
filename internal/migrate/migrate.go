// Package migrate owns the database schema.
//
// Every schema change in Grimoire is a numbered SQL file under migrations/,
// embedded in the binary and applied by [Up] before the server serves a single
// request. Nothing else in the codebase imports goose: this package is the one
// seam, so swapping the runner later is a change to one file rather than to
// every package that owns a table.
//
// The migrations are embedded rather than shipped as a directory. Grimoire is
// one static binary with one SQLite file, and a migration set that lives
// outside the binary would be a second thing to deploy and a second thing to
// get out of sync.
package migrate

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
)

// FS holds the embedded migration files. Exported so tests (and the version
// linter in migrations_test.go) can walk the same set the runner applies.
//
//go:embed migrations/*.sql
var FS embed.FS

// dir is the path inside FS that holds the .sql files.
const dir = "migrations"

// goose keeps dialect and base FS in package-level state, so every entry point
// funnels through configure to set them exactly once per process.
var configureOnce sync.Once

// configure prepares the goose package for this application.
//
// The dialect is "sqlite3" even though the registered driver name is "sqlite"
// (modernc.org/sqlite registers itself under the shorter name). The dialect
// only picks the SQL flavour goose uses for its own goose_db_version
// bookkeeping table; the actual driver comes from the *sql.DB handed in. Using
// the driver name here fails with an unknown-dialect error that reads like a
// driver problem and is not one.
func configure() {
	configureOnce.Do(func() {
		if err := goose.SetDialect("sqlite3"); err != nil {
			// Only reachable if the constant above is misspelled, which the
			// package's own tests would catch first.
			panic(fmt.Sprintf("migrate: set dialect: %v", err))
		}
		// goose logs each applied migration through its own logger; route it
		// at the standard logger so migration output lands in the same place
		// as every other boot line.
		goose.SetLogger(log.Default())
	})
}

// Up applies every migration the database has not seen yet.
//
// Callers must treat a returned error as fatal. A half-applied schema on a
// self-hosted box is a data-loss shape, so the binary refuses to serve rather
// than serving whatever made it through.
func Up(db *sql.DB) error { return up(db, FS) }

// up is Up with an injectable file system, so tests can drive the runner over
// a deliberately broken migration set without shipping one.
func up(db *sql.DB, fsys fs.FS) error {
	configure()
	goose.SetBaseFS(fsys)
	defer goose.SetBaseFS(nil)
	if err := goose.Up(db, dir); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down rolls back the most recently applied migration.
//
// Operator-facing only, via `grimoire migrate down`. Nothing in the serving
// path ever rolls back on its own.
func Down(db *sql.DB) error {
	configure()
	goose.SetBaseFS(FS)
	defer goose.SetBaseFS(nil)
	if err := goose.Down(db, dir); err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// Status writes the applied/pending state of every migration to w.
func Status(db *sql.DB, w io.Writer) error {
	configure()
	goose.SetBaseFS(FS)
	defer goose.SetBaseFS(nil)
	goose.SetLogger(log.New(w, "", 0))
	defer goose.SetLogger(log.Default())
	if err := goose.Status(db, dir); err != nil {
		return fmt.Errorf("migrate status: %w", err)
	}
	return nil
}

// Version reports the migration version the database is currently stamped at.
// A database that has never been migrated reports 0.
func Version(db *sql.DB) (int64, error) {
	configure()
	goose.SetBaseFS(FS)
	defer goose.SetBaseFS(nil)
	v, err := goose.GetDBVersion(db)
	if err != nil {
		return 0, fmt.Errorf("migrate version: %w", err)
	}
	return v, nil
}

// Versions lists the migration versions present in fsys, in ascending order,
// alongside the file each came from. It is the input to the numbering check
// that makes a collision a build failure rather than a runtime surprise.
func Versions(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Validate reports whether a migration set is well formed: every filename
// leads with a version, versions start at 1, and each following version is
// exactly one higher — no duplicates, no gaps.
//
// goose itself tolerates gaps and resolves duplicates in an order nobody
// should have to reason about, so the stricter rule lives here and runs as a
// test. A numbering collision is a build failure, not something an operator
// discovers halfway through an upgrade.
func Validate(fsys fs.FS) error {
	got, err := Versions(fsys)
	if err != nil {
		return err
	}
	if len(got) == 0 {
		return errors.New("no migrations found")
	}
	for i, m := range got {
		want := int64(i + 1)
		if m.Version == want {
			continue
		}
		if i > 0 && m.Version == got[i-1].Version {
			return fmt.Errorf("%s and %s share version %d", got[i-1].Name, m.Name, m.Version)
		}
		return fmt.Errorf("%s has version %d, expected %d (versions start at 1 and increase by one)", m.Name, m.Version, want)
	}
	return nil
}

// Migration is one embedded migration file.
type Migration struct {
	Version int64
	Name    string
}

// errBadName reports a migration file that does not lead with a version.
var errBadName = errors.New("migration file must be named <version>_<description>.sql")

// parseVersion pulls the leading numeric version off a migration filename.
func parseVersion(name string) (int64, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("%s: %w", name, errBadName)
	}
	v, err := strconv.ParseInt(name[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, errBadName)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: version must be positive", name)
	}
	return v, nil
}
