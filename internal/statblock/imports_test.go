package statblock

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// TestPackageImportsNoDatabase is the acceptance guarantee that
// internal/statblock stays a pure package: no file in it may import
// database/sql or the net packages. The calculator is fed by the encounter
// mirror and later by the monster designer, and it must be testable — and
// trustworthy — without either. The same rule internal/dungeon and
// internal/clock carry.
func TestPackageImportsNoDatabase(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"database/sql": true,
		"net/http":     true,
		"net":          true,
		"time":         true,
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range file.Imports {
			path := imp.Path.Value
			path = path[1 : len(path)-1] // strip quotes
			if banned[path] {
				t.Errorf("%s imports %q; internal/statblock must stay pure", name, path)
			}
		}
	}
}
