package uistate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite" // the pure-Go driver the app opens the real file with
)

// openDB opens a scratch database with the same DSN shape the app uses and
// applies the migrations — user_layouts exists only through the runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "uistate.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Up(db); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return db
}

func users(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := db.Exec(
			`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (?, ?, 'x', 0, 0)`,
			id, "user-"+id); err != nil {
			t.Fatalf("insert user %s: %v", id, err)
		}
	}
}

// A small but realistic layout: chat beside a tabbed pair, the shape the
// "At the table" preset produces.
const sampleTree = `{"t":"split","dir":"row","fr":[0.6,0.4],"kids":[
	{"t":"leaf","tool":"chat"},
	{"t":"tabs","active":1,"kids":[{"t":"leaf","tool":"sessions"},{"t":"leaf","tool":"encounter"}]}
]}`

func store(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db := openDB(t)
	users(t, db, "u1", "u2")
	return New(db), db
}

/* ---------- layouts ---------- */

func TestSaveAndReadLayout(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()

	in := Layout{Corpus: "dnd", Slot: 2, Name: "At the table", Tree: json.RawMessage(sampleTree)}
	if err := s.SaveLayout(ctx, "u1", in); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.Layouts(ctx, "u1", "dnd")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 layout, got %d", len(got))
	}
	if got[0].Slot != 2 || got[0].Name != "At the table" || got[0].Corpus != "dnd" {
		t.Fatalf("round trip lost fields: %+v", got[0])
	}
	if got[0].UpdatedAt.IsZero() {
		t.Error("updated_at was not set")
	}

	// The tree must survive byte-for-byte in structure, since the client
	// re-parses it and any silent rewrite would move the user's windows.
	var want, have any
	json.Unmarshal([]byte(sampleTree), &want)
	if err := json.Unmarshal(got[0].Tree, &have); err != nil {
		t.Fatalf("stored tree is not JSON: %v", err)
	}
	if fmt.Sprint(want) != fmt.Sprint(have) {
		t.Errorf("tree changed in storage:\n want %v\n  got %v", want, have)
	}
}

func TestSaveLayoutReplacesTheSlot(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()

	base := Layout{Corpus: "dnd", Slot: 1, Name: "Prep", Tree: json.RawMessage(`{"t":"leaf","tool":"planner"}`)}
	if err := s.SaveLayout(ctx, "u1", base); err != nil {
		t.Fatalf("save: %v", err)
	}
	base.Name = "Prep (mine)"
	base.Tree = json.RawMessage(`{"t":"leaf","tool":"campaign"}`)
	if err := s.SaveLayout(ctx, "u1", base); err != nil {
		t.Fatalf("resave: %v", err)
	}

	got, _ := s.Layouts(ctx, "u1", "dnd")
	if len(got) != 1 {
		t.Fatalf("want 1 layout after replace, got %d", len(got))
	}
	if got[0].Name != "Prep (mine)" || !strings.Contains(string(got[0].Tree), "campaign") {
		t.Errorf("slot was not replaced: %+v", got[0])
	}
}

func TestLayoutsAreScopedPerUserAndCorpus(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()

	mine := Layout{Corpus: "dnd", Slot: 1, Name: "Prep", Tree: json.RawMessage(`{"t":"leaf","tool":"planner"}`)}
	theirs := Layout{Corpus: "dnd", Slot: 1, Name: "Theirs", Tree: json.RawMessage(`{"t":"leaf","tool":"chat"}`)}
	magic := Layout{Corpus: "mtg", Slot: 1, Name: "Brew", Tree: json.RawMessage(`{"t":"leaf","tool":"deck"}`)}

	for _, c := range []struct {
		user string
		l    Layout
	}{{"u1", mine}, {"u2", theirs}, {"u1", magic}} {
		if err := s.SaveLayout(ctx, c.user, c.l); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	dnd, _ := s.Layouts(ctx, "u1", "dnd")
	if len(dnd) != 1 || dnd[0].Name != "Prep" {
		t.Errorf("u1/dnd leaked or lost rows: %+v", dnd)
	}
	mtg, _ := s.Layouts(ctx, "u1", "mtg")
	if len(mtg) != 1 || mtg[0].Name != "Brew" {
		t.Errorf("u1/mtg leaked or lost rows: %+v", mtg)
	}
	other, _ := s.Layouts(ctx, "u2", "dnd")
	if len(other) != 1 || other[0].Name != "Theirs" {
		t.Errorf("u2 saw the wrong layouts: %+v", other)
	}
}

func TestLayoutsReturnedInSlotOrder(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()
	for _, slot := range []int{5, 1, 3} {
		l := Layout{Corpus: "dnd", Slot: slot, Name: fmt.Sprintf("w%d", slot),
			Tree: json.RawMessage(`{"t":"leaf","tool":"chat"}`)}
		if err := s.SaveLayout(ctx, "u1", l); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	got, _ := s.Layouts(ctx, "u1", "dnd")
	if len(got) != 3 || got[0].Slot != 1 || got[1].Slot != 3 || got[2].Slot != 5 {
		t.Errorf("not in slot order: %+v", got)
	}
}

func TestLayoutsEmptyForNewAccount(t *testing.T) {
	s, _ := store(t)
	got, err := s.Layouts(context.Background(), "u1", "dnd")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no layouts, got %d", len(got))
	}
}

func TestDeleteLayoutClearsOneSlot(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()
	for _, slot := range []int{1, 2} {
		l := Layout{Corpus: "dnd", Slot: slot, Name: "w", Tree: json.RawMessage(`{"t":"leaf","tool":"chat"}`)}
		s.SaveLayout(ctx, "u1", l)
	}
	if err := s.DeleteLayout(ctx, "u1", "dnd", 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ := s.Layouts(ctx, "u1", "dnd")
	if len(got) != 1 || got[0].Slot != 2 {
		t.Errorf("wrong slot removed: %+v", got)
	}
}

func TestDeletingAUserTakesTheirLayouts(t *testing.T) {
	s, db := store(t)
	ctx := context.Background()
	l := Layout{Corpus: "dnd", Slot: 1, Name: "Prep", Tree: json.RawMessage(`{"t":"leaf","tool":"planner"}`)}
	s.SaveLayout(ctx, "u1", l)
	s.SetPrefs(ctx, "u1", map[string]string{"scene": "cave"})

	if _, err := db.Exec(`DELETE FROM users WHERE id = 'u1'`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if got, _ := s.Layouts(ctx, "u1", "dnd"); len(got) != 0 {
		t.Errorf("layouts outlived the account: %+v", got)
	}
	if got, _ := s.Prefs(ctx, "u1"); len(got) != 0 {
		t.Errorf("prefs outlived the account: %+v", got)
	}
}

func TestSaveLayoutRejectsBadInput(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()
	ok := json.RawMessage(`{"t":"leaf","tool":"chat"}`)

	cases := []struct {
		name string
		l    Layout
	}{
		{"unknown corpus", Layout{Corpus: "pokemon", Slot: 1, Name: "x", Tree: ok}},
		{"slot too low", Layout{Corpus: "dnd", Slot: 0, Name: "x", Tree: ok}},
		{"slot too high", Layout{Corpus: "dnd", Slot: 10, Name: "x", Tree: ok}},
		{"empty name", Layout{Corpus: "dnd", Slot: 1, Name: "", Tree: ok}},
		{"long name", Layout{Corpus: "dnd", Slot: 1, Name: strings.Repeat("x", MaxNameLen+1), Tree: ok}},
		{"empty tree", Layout{Corpus: "dnd", Slot: 1, Name: "x", Tree: nil}},
		{"malformed tree", Layout{Corpus: "dnd", Slot: 1, Name: "x", Tree: json.RawMessage(`{"t":"leaf"`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.SaveLayout(ctx, "u1", c.l)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("want ErrInvalid so the handler answers 400, got %v", err)
			}
		})
	}

	// Nothing may have been written by any of the rejected calls.
	if got, _ := s.Layouts(ctx, "u1", "dnd"); len(got) != 0 {
		t.Errorf("a rejected layout reached the table: %+v", got)
	}
}

/* ---------- preferences ---------- */

func TestPrefsRoundTripAndMerge(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()

	if err := s.SetPrefs(ctx, "u1", map[string]string{"scene": "cave", "corpus": "dnd"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// A second write of one key must leave the other alone — the client saves
	// one setting at a time.
	if err := s.SetPrefs(ctx, "u1", map[string]string{"scene": "snowy"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := s.Prefs(ctx, "u1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got["scene"] != "snowy" {
		t.Errorf("scene not updated: %q", got["scene"])
	}
	if got["corpus"] != "dnd" {
		t.Errorf("merge dropped an untouched key: %+v", got)
	}
}

func TestPrefsAreScopedPerUser(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()
	s.SetPrefs(ctx, "u1", map[string]string{"scene": "cave"})
	s.SetPrefs(ctx, "u2", map[string]string{"scene": "plains"})

	a, _ := s.Prefs(ctx, "u1")
	b, _ := s.Prefs(ctx, "u2")
	if a["scene"] != "cave" || b["scene"] != "plains" {
		t.Errorf("prefs leaked between accounts: %v / %v", a, b)
	}
}

func TestPrefsEmptyForNewAccount(t *testing.T) {
	s, _ := store(t)
	got, err := s.Prefs(context.Background(), "u1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("want an empty map, got nil — callers index it without a check")
	}
	if len(got) != 0 {
		t.Errorf("want no prefs, got %+v", got)
	}
}

func TestSetPrefsRejectsBadInput(t *testing.T) {
	s, _ := store(t)
	ctx := context.Background()

	tooMany := map[string]string{}
	for i := 0; i <= MaxPrefs; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}

	cases := map[string]map[string]string{
		"empty key":  {"": "v"},
		"long key":   {strings.Repeat("k", MaxPrefKey+1): "v"},
		"long value": {"scene": strings.Repeat("v", MaxPrefVal+1)},
		"too many":   tooMany,
	}
	for name, prefs := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.SetPrefs(ctx, "u1", prefs)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
	if got, _ := s.Prefs(ctx, "u1"); len(got) != 0 {
		t.Errorf("a rejected write landed: %+v", got)
	}
}

/* ---------- tree validation ----------
   This is the wire parsing ADR 8 makes mandatory: the payload has been
   through a browser, a database and a release boundary before it gets here. */

func TestValidateTreeAccepts(t *testing.T) {
	good := map[string]string{
		"a lone leaf":        `{"t":"leaf","tool":"chat"}`,
		"a split":            `{"t":"split","dir":"row","fr":[0.5,0.5],"kids":[{"t":"leaf","tool":"chat"},{"t":"leaf","tool":"deck"}]}`,
		"a split sans fr":    `{"t":"split","dir":"col","kids":[{"t":"leaf","tool":"chat"},{"t":"leaf","tool":"deck"}]}`,
		"tabs":               `{"t":"tabs","active":0,"kids":[{"t":"leaf","tool":"chat"}]}`,
		"tabs sans active":   `{"t":"tabs","kids":[{"t":"leaf","tool":"chat"}]}`,
		"a hyphenated tool":  `{"t":"leaf","tool":"ask-campaign"}`,
		"an empty workspace": `null`,
		"the sample":         sampleTree,
	}
	for name, tree := range good {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTree(json.RawMessage(tree)); err != nil {
				t.Errorf("rejected a valid tree: %v", err)
			}
		})
	}
}

func TestValidateTreeRejects(t *testing.T) {
	deep := `{"t":"leaf","tool":"chat"}`
	for i := 0; i < MaxDepth+2; i++ {
		deep = `{"t":"split","dir":"row","kids":[` + deep + `,{"t":"leaf","tool":"chat"}]}`
	}

	wide := make([]string, MaxNodes+10)
	for i := range wide {
		wide[i] = `{"t":"leaf","tool":"chat"}`
	}
	huge := `{"t":"split","dir":"row","kids":[` + strings.Join(wide, ",") + `]}`

	bad := map[string]string{
		"not JSON":             `{`,
		"a bare number":        `42`,
		"unknown node kind":    `{"t":"window","kids":[]}`,
		"leaf without a tool":  `{"t":"leaf"}`,
		"tool with spaces":     `{"t":"leaf","tool":"my tool"}`,
		"tool with a slash":    `{"t":"leaf","tool":"../etc/passwd"}`,
		"uppercase tool":       `{"t":"leaf","tool":"Chat"}`,
		"overlong tool":        `{"t":"leaf","tool":"` + strings.Repeat("a", MaxToolLen+5) + `"}`,
		"leaf with children":   `{"t":"leaf","tool":"chat","kids":[{"t":"leaf","tool":"deck"}]}`,
		"split without a dir":  `{"t":"split","kids":[{"t":"leaf","tool":"chat"},{"t":"leaf","tool":"deck"}]}`,
		"split with a bad dir": `{"t":"split","dir":"diagonal","kids":[{"t":"leaf","tool":"chat"},{"t":"leaf","tool":"deck"}]}`,
		"childless split":      `{"t":"split","dir":"row","kids":[]}`,
		"childless tabs":       `{"t":"tabs","kids":[]}`,
		"fr/kids mismatch":     `{"t":"split","dir":"row","fr":[0.5,0.3,0.2],"kids":[{"t":"leaf","tool":"chat"},{"t":"leaf","tool":"deck"}]}`,
		"negative fraction":    `{"t":"split","dir":"row","fr":[-1,2],"kids":[{"t":"leaf","tool":"chat"},{"t":"leaf","tool":"deck"}]}`,
		"active out of range":  `{"t":"tabs","active":7,"kids":[{"t":"leaf","tool":"chat"}]}`,
		"negative active":      `{"t":"tabs","active":-1,"kids":[{"t":"leaf","tool":"chat"}]}`,
		"too deep":             deep,
		"too many nodes":       huge,
	}
	for name, tree := range bad {
		t.Run(name, func(t *testing.T) {
			err := ValidateTree(json.RawMessage(tree))
			if err == nil {
				t.Fatal("accepted a tree it should have refused")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestValidateTreeRejectsOversizedPayload(t *testing.T) {
	err := ValidateTree(json.RawMessage(`{"t":"leaf","tool":"` + strings.Repeat("a", MaxTreeSize) + `"}`))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for an oversized tree, got %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("a", MaxToolLen+1)) {
		t.Error("the error echoes the whole rejected payload back")
	}
}

func TestValidateCorpus(t *testing.T) {
	for _, ok := range []string{"mtg", "dnd"} {
		if err := ValidateCorpus(ok); err != nil {
			t.Errorf("rejected %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "MTG", "pokemon", "dnd; DROP TABLE users"} {
		if err := ValidateCorpus(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %q", bad)
		}
	}
}
