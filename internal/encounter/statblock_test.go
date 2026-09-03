package encounter

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

// The mirror carries the fields the CR arithmetic needs, and every action
// lands in exactly one bucket: a structured Attack, or Unparsed with its
// prose intact. Nothing is ever half-parsed.
func TestCatalogMirrorCarriesStatblockFields(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	g, ok := cat.Lookup("goblin")
	if !ok {
		t.Fatal("goblin missing")
	}
	if g.Abilities == nil || g.Abilities.Dex != 14 || g.Abilities.Str != 8 {
		t.Errorf("goblin abilities = %+v", g.Abilities)
	}
	if g.Saves["dex"] != 2 || g.Saves["str"] != -1 {
		t.Errorf("goblin saves = %+v", g.Saves)
	}
	if g.Skills["stealth"] != 4 {
		t.Errorf("goblin skills = %+v", g.Skills)
	}
	// Open5e leaves proficiency null on the 2014 SRD; the table value for
	// CR 1/4 is +2.
	if g.ProfBonus != 2 {
		t.Errorf("goblin prof bonus = %d, want the DMG table's +2", g.ProfBonus)
	}
	if len(g.Attacks) != 1 || g.Attacks[0].ToHit != 4 || g.Attacks[0].Damage[0].Avg != 5 {
		t.Errorf("goblin attacks = %+v", g.Attacks)
	}
	if len(g.Unparsed) != 0 {
		t.Errorf("goblin unparsed = %+v, want every action parsed", g.Unparsed)
	}
	if g.Spellcasting {
		t.Error("a goblin is no spellcaster")
	}

	l, _ := cat.Lookup("lich")
	if !l.Spellcasting {
		t.Error("the lich's Spellcasting trait must set the flag")
	}
	if l.ProfBonus != 7 {
		t.Errorf("lich prof bonus = %d, want +7 for CR 21", l.ProfBonus)
	}
	// The lich's prose names no mechanic the parser reads — it must be
	// recorded as unparsed, not dropped.
	if len(l.Unparsed) != 2 {
		t.Errorf("lich unparsed = %+v, want both actions kept with their prose", l.Unparsed)
	}
	for _, u := range l.Unparsed {
		if u.Desc == "" {
			t.Errorf("unparsed action %q lost its prose", u.Name)
		}
	}
	if len(l.Attacks) != 0 {
		t.Errorf("lich attacks = %+v", l.Attacks)
	}
}

// The bridge feeds the pure calculator from the mirror without inventing
// anything, and ComputeCR over a mirrored goblin lands on the halves its
// unit test pins.
func TestCreatureStatblockBridge(t *testing.T) {
	srv := catalogFixture(t)
	cat, _ := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	g, _ := cat.Lookup("goblin")
	s := g.Statblock()

	if s.AC != 15 || s.HP != 7 || s.Abilities.Dex != 14 || s.ProfBonus != 2 {
		t.Errorf("statblock numbers = %+v", s)
	}
	if len(s.Actions) != 1 || !s.Actions[0].Parsed || s.Actions[0].Attack.Damage[0].Avg != 5 {
		t.Errorf("statblock actions = %+v", s.Actions)
	}
	r := statblock.ComputeCR(s)
	if r.Defensive != 0.25 {
		t.Errorf("goblin defensive CR = %v, want the pinned 0.25", r.Defensive)
	}
	if r.CR == 0 {
		t.Error("a goblin must not compute to CR 0")
	}

	// A creature whose actions were not parseable rates, but honestly low.
	l, _ := cat.Lookup("lich")
	lr := statblock.ComputeCR(l.Statblock())
	if lr.Confidence != statblock.ConfidenceLow {
		t.Errorf("lich confidence = %q, want low — its actions do not parse", lr.Confidence)
	}
	if lr.CR == 0 {
		t.Error("the lich's defense must still rate")
	}
}

// A mirror written before the statblock fields reports stale and re-syncs
// once, filling the new fields — and a saved encounter that references the
// creature survives the re-sync untouched.
func TestCatalogResyncMigratesMirror(t *testing.T) {
	srv := catalogFixture(t)
	cat, store := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	// A DM saved an encounter against the goblin.
	encounters, err := New(store.DB())
	if err != nil {
		t.Fatalf("encounter store: %v", err)
	}
	saved, err := encounters.Create(context.Background(), "dm-1", "ambush", []int{3}, []Monster{{Name: "Goblin", CR: "1/4", XP: 50, Count: 4}}, "")
	if err != nil {
		t.Fatalf("save encounter: %v", err)
	}

	// Roll the stored mirror back to the pre-statblock schema: the meta row
	// names version 1 and the blobs carry no statblock fields.
	if err := downgradeMirror(store.DB()); err != nil {
		t.Fatalf("downgrade mirror: %v", err)
	}
	if err := cat.Load(); err != nil {
		t.Fatalf("load downgraded mirror: %v", err)
	}
	g, _ := cat.Lookup("goblin")
	if len(g.Attacks) != 0 || g.Abilities != nil {
		t.Fatal("test setup: the downgraded blob should have no statblock fields")
	}
	if !cat.Stale() {
		t.Fatal("a v1 mirror must report stale so it re-syncs once")
	}

	if err := cat.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("ensure fresh: %v", err)
	}
	if cat.Stale() {
		t.Error("a re-synced mirror must not still report stale")
	}
	g, _ = cat.Lookup("goblin")
	if len(g.Attacks) != 1 || g.Abilities == nil {
		t.Errorf("migrated goblin carries attacks=%d abilities=%+v, want both filled",
			len(g.Attacks), g.Abilities)
	}

	// The saved encounter is untouched by the bestiary rewrite.
	got, err := encounters.Get(context.Background(), "dm-1", saved.ID)
	if err != nil {
		t.Fatalf("get saved encounter after re-sync: %v", err)
	}
	if len(got.Monsters) != 1 || got.Monsters[0].Name != "Goblin" {
		t.Errorf("saved encounter = %+v, want its goblin roster intact", got.Monsters)
	}
}

// downgradeMirror rewrites every stored blob without the statblock fields
// and stamps the mirror as schema version 1 — the shape an install from
// before this issue carries.
func downgradeMirror(db *sql.DB) error {
	rows, err := db.Query(`SELECT key, data FROM bestiary`)
	if err != nil {
		return err
	}
	type row struct{ key, data string }
	var rowsOut []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.data); err != nil {
			rows.Close()
			return err
		}
		rowsOut = append(rowsOut, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, r := range rowsOut {
		var blob map[string]any
		if err := json.Unmarshal([]byte(r.data), &blob); err != nil {
			return err
		}
		for _, k := range []string{"attacks", "unparsed", "abilities", "saves", "skills", "prof_bonus", "spellcasting", "lair_action"} {
			delete(blob, k)
		}
		data, err := json.Marshal(blob)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE bestiary SET data = ? WHERE key = ?`, string(data), r.key); err != nil {
			return err
		}
	}
	_, err = db.Exec(`UPDATE bestiary_meta SET value = '1' WHERE key = 'schema_version'`)
	return err
}

// The mirror persists the statblock fields across a restart: reload the
// table from disk and the parsed attacks are still there, no sync needed.
func TestCatalogStatblockFieldsSurviveReload(t *testing.T) {
	srv := catalogFixture(t)
	cat, store := newCatalog(t, srv.URL)
	if err := cat.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	reloaded, err := NewCatalog(store.DB(), srv.URL)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := reloaded.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if reloaded.Stale() {
		t.Fatal("a freshly written v2 mirror must not report stale after reload")
	}
	g, _ := reloaded.Lookup("goblin")
	if len(g.Attacks) != 1 {
		t.Errorf("reloaded goblin attacks = %d, want 1", len(g.Attacks))
	}
}
