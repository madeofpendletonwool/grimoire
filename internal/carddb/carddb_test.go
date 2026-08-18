package carddb

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (FTS5 enabled)
)

// fixtureAtomic builds a small gzipped AtomicCards payload around the given
// card entries (name -> faces).
func fixtureAtomic(t *testing.T, cards map[string][]map[string]any) []byte {
	t.Helper()
	payload := map[string]any{
		"meta": map[string]any{"date": "2026-08-17"},
		"data": cards,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func rank(n int) *int         { return &n }
func salt(f float64) *float64 { return &f }
func ls(cmdr bool) map[string]any {
	return map[string]any{"Commander": cmdr}
}

// kaaliaFixture is a miniature card universe: a commander, themed payoffs, a
// color-illegal card, an unranked card, and a split card.
func kaaliaFixture(t *testing.T) []byte {
	return fixtureAtomic(t, map[string][]map[string]any{
		"Kaalia of the Vast": {{
			"manaCost": "{1}{R}{W}{B}", "manaValue": 4,
			"type":             "Legendary Creature — Angel Cleric",
			"text":             "Flying\n{T}: Put an Angel, Demon, or Dragon creature card from your hand onto the battlefield.",
			"colorIdentity":    []string{"R", "W", "B"},
			"edhrecRank":       rank(122),
			"edhrecSaltiness":  salt(1.9),
			"leadershipSkills": ls(true),
			"legalities":       map[string]string{"commander": "Legal"},
		}},
		"Angel of Serenity": {{
			"manaCost": "{5}{W}{W}{W}", "manaValue": 7,
			"type":          "Creature — Angel",
			"text":          "Flying\nWhen Angel of Serenity enters, exile up to three target creatures.",
			"colorIdentity": []string{"W"},
			"edhrecRank":    rank(800),
			"legalities":    map[string]string{"commander": "Legal"},
		}},
		"Dragon Tempest": {{
			"manaCost": "{1}{R}{R}", "manaValue": 3,
			"type":          "Enchantment",
			"text":          "Whenever a creature with flying enters under your control, it deals damage equal to its power to any target.",
			"colorIdentity": []string{"R"},
			"edhrecRank":    rank(1500),
			"legalities":    map[string]string{"commander": "Legal"},
		}},
		"Counterspell": {{
			"manaCost": "{U}{U}", "manaValue": 2,
			"type":          "Instant",
			"text":          "Counter target spell.",
			"colorIdentity": []string{"U"},
			"edhrecRank":    rank(15),
			"legalities":    map[string]string{"commander": "Legal"},
		}},
		"Sol Ring": {{
			"manaCost": "{1}", "manaValue": 1,
			"type":          "Artifact",
			"text":          "{T}: Add {C}{C}.",
			"colorIdentity": []string{},
			"edhrecRank":    rank(1),
			"legalities":    map[string]string{"commander": "Legal"},
		}},
		"Plains": {{
			"type":          "Basic Land — Plains",
			"text":          "{T}: Add {W}.",
			"colorIdentity": []string{"W"},
			"edhrecRank":    rank(30),
			"legalities":    map[string]string{"commander": "Legal"},
		}},
		"Fire // Ice": {{
			"manaCost": "{1}{R}//{1}{U}", "manaValue": 2,
			"type":          "Instant // Instant",
			"text":          "Fire deals 2 damage // Tap target permanent.",
			"colorIdentity": []string{"R", "U"},
			"edhrecRank":    rank(900),
			"legalities":    map[string]string{"commander": "Legal"},
		}},
		"Mistmeadow Vanisher": {{
			"manaCost": "{3}{U}", "manaValue": 4,
			"type":          "Creature — Faerie Wizard",
			"text":          "Flash. Mistmeadow Vanisher can't be countered.",
			"colorIdentity": []string{"U"},
			"legalities":    map[string]string{"commander": "Legal"},
		}},
	})
}

func populateFixture(t *testing.T, fixture []byte) (*Store, []string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/gzip")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(srv.Close)

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cards.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	names, err := Populate(context.Background(), db, srv.URL)
	if err != nil {
		t.Fatalf("populate: %v", err)
	}
	return store, names
}

func TestPopulateFoldsFaces(t *testing.T) {
	store, names := populateFixture(t, kaaliaFixture(t))
	if len(names) != 8 {
		t.Fatalf("names = %d, want 8 (%v)", len(names), names)
	}
	if n, _ := store.Count(); n != 8 {
		t.Fatalf("count = %d, want 8", n)
	}

	kaalia, err := store.Get("kaalia of the vast")
	if err != nil {
		t.Fatalf("get (case-insensitive): %v", err)
	}
	if !kaalia.CommanderLegal || kaalia.EDHRECRank != 122 || kaalia.EDHRECSaltiness != 1.9 {
		t.Errorf("kaalia = %+v", kaalia)
	}
	if kaalia.ColorIdentity != "BRW" {
		t.Errorf("identity = %q, want BRW", kaalia.ColorIdentity)
	}

	// Split card: faces joined, identity unioned, rank kept.
	fire, err := store.Get("Fire // Ice")
	if err != nil {
		t.Fatalf("get split: %v", err)
	}
	if !strings.Contains(fire.OracleText, "//") || fire.ColorIdentity != "RU" {
		t.Errorf("fire = %+v", fire)
	}
	if fire.ManaValue != 2 {
		t.Errorf("split mana value = %v, want 2", fire.ManaValue)
	}

	// Unranked card: rank stays zero, no error.
	if mist, err := store.Get("Mistmeadow Vanisher"); err != nil || mist.EDHRECRank != 0 {
		t.Errorf("unranked: %+v err=%v", mist, err)
	}
}

func TestSearchNames(t *testing.T) {
	store, _ := populateFixture(t, kaaliaFixture(t))
	hits, err := store.SearchNames(context.Background(), "zzz-nothing", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("garbage query returned hits: %+v", hits)
	}

	hits, err = store.SearchNames(context.Background(), "angel serenity", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Name != "Angel of Serenity" {
		t.Fatalf("fts hits = %+v", hits)
	}
}

func TestCommandersFilterAndRank(t *testing.T) {
	store, _ := populateFixture(t, kaaliaFixture(t))

	// Any identity: only Kaalia is commander-eligible.
	cmdrs, err := store.Commanders(context.Background(), 0, nil, 10)
	if err != nil {
		t.Fatalf("commanders: %v", err)
	}
	if len(cmdrs) != 1 || cmdrs[0].Name != "Kaalia of the Vast" {
		t.Fatalf("commanders = %+v", cmdrs)
	}

	// Terms narrow by FTS: "angel" still finds Kaalia.
	cmdrs, err = store.Commanders(context.Background(), 0, []string{"angel"}, 10)
	if err != nil {
		t.Fatalf("commanders terms: %v", err)
	}
	if len(cmdrs) != 1 || cmdrs[0].Name != "Kaalia of the Vast" {
		t.Fatalf("themed commanders = %+v", cmdrs)
	}
}

func TestCandidatesIdentityFilter(t *testing.T) {
	store, _ := populateFixture(t, kaaliaFixture(t))
	m := MaskForColors("BRW")

	cands, err := store.Candidates(context.Background(), m, nil, nil, 50)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	for _, c := range cands {
		if c.Name == "Counterspell" || c.Name == "Fire // Ice" {
			t.Errorf("out-of-identity card returned: %s", c.Name)
		}
		if c.IsLand() {
			t.Errorf("land returned as candidate: %s", c.Name)
		}
	}
	// Sol Ring (rank 1) leads.
	if len(cands) == 0 || cands[0].Name != "Sol Ring" {
		t.Fatalf("candidates[0] = %+v, want Sol Ring", cands[0])
	}

	// Exclusion drops an already-drafted name.
	cands, err = store.Candidates(context.Background(), m, nil, map[string]bool{"sol ring": true}, 50)
	if err != nil {
		t.Fatalf("candidates exclude: %v", err)
	}
	for _, c := range cands {
		if c.Name == "Sol Ring" {
			t.Fatal("excluded card returned")
		}
	}

	// Theme terms surface the on-theme enchantment even past a better rank.
	cands, err = store.Candidates(context.Background(), m, []string{"flying", "angel"}, nil, 10)
	if err != nil {
		t.Fatalf("candidates theme: %v", err)
	}
	if len(cands) == 0 || cands[0].Name != "Kaalia of the Vast" {
		t.Fatalf("themed candidates[0] = %+v", cands[0])
	}
}

func TestIdentityMath(t *testing.T) {
	if got := NormalizeIdentity("rb"); got != "BR" {
		t.Errorf("normalize = %q", got)
	}
	if got := ColorMask("WUBRG"); got != 31 {
		t.Errorf("mask = %d, want 31", got)
	}
	card := &Card{ColorIdentity: "U"}
	if !card.IdentityAllowed(MaskForColors("WUB")) {
		t.Error("U card should be legal in WUB")
	}
	if card.IdentityAllowed(MaskForColors("BR")) {
		t.Error("U card should be illegal in BR")
	}
}
