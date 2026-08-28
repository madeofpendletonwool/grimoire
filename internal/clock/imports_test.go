package clock

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// TestPackageImportsNoDatabase is the acceptance guarantee that internal/clock
// stays a pure package: no file in it may import database/sql (or the net
// packages). The moment someone reaches for a handle from inside the
// calendar, this test fails the build — the purity is the feature the
// offline, no-model promise of MAD-365 rests on.
func TestPackageImportsNoDatabase(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{
		"database/sql": true,
		"net/http":     true,
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
				t.Errorf("%s imports %q; internal/clock must stay pure", name, path)
			}
		}
	}
}
