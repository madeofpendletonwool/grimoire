package sheet_test

// The projection contract (MAD-418), exercised over the real schema: rows
// derive from the payloads, the write path refreshes one row, boot rebuilds
// the table, the unstructured marker is queryable, and nothing is invented.
// External test package because the campaign store (the entity writer)
// imports internal/sheet, and an internal test would cycle.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/sheet"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

func seedUser(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (?, ?, 'x', 0, 0)`,
		id, "user-"+id); err != nil {
		t.Fatalf("insert user %s: %v", id, err)
	}
}

func TestProjectionDerivesFromPayloads(t *testing.T) {
	db := testdb.Open(t)
	store, err := campaign.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedUser(t, db, "keeper")
	c, err := store.CreateCampaign(ctx, "keeper", "Projection Campaign", "dnd5e", "")
	if err != nil {
		t.Fatal(err)
	}

	// A structured pc: the sheet is the definition.
	structured, err := store.CreateEntity(ctx, c.ID, "pc", "Thalia", "",
		sheet.WithSheet(nil, sheet.Sheet{
			Classes: []sheet.ClassLevel{{Class: "fighter", Level: 8}, {Class: "wizard", Level: 2}},
			AC:      18, MaxHP: 49,
		}))
	if err != nil {
		t.Fatal(err)
	}
	// A legacy pc: top-level party keys, no sheet — the pre-MAD-418 shape
	// every existing campaign carries.
	legacy, err := store.CreateEntity(ctx, c.ID, "pc", "Bran", "",
		map[string]any{"level": float64(5), "class": "rogue", "ac": float64(15), "max_hp": float64(33)})
	if err != nil {
		t.Fatal(err)
	}
	// An empty pc: nothing declared at all.
	bare, err := store.CreateEntity(ctx, c.ID, "pc", "Nobody", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := sheet.SyncProjections(ctx, db); err != nil {
		t.Fatalf("boot sync: %v", err)
	}
	rows, err := sheet.Projections(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("projection rows = %d, want 3 (%+v)", len(rows), rows)
	}
	byID := map[string]sheet.Projection{}
	for _, p := range rows {
		byID[p.EntityID] = p
	}

	thalia := byID[structured.ID]
	if !thalia.Structured || thalia.Level != 10 || thalia.Classes != "fighter 8/wizard 2" || thalia.MaxHP != 49 || thalia.AC != 18 {
		t.Fatalf("structured projection = %+v", thalia)
	}
	bran := byID[legacy.ID]
	if bran.Structured {
		t.Fatal("a legacy payload must project as unstructured")
	}
	if bran.Level != 5 || bran.Classes != "rogue" || bran.AC != 15 || bran.MaxHP != 33 {
		t.Fatalf("legacy projection = %+v (existing keys must still derive)", bran)
	}
	nobody := byID[bare.ID]
	if nobody.Structured || nobody.Level != 0 {
		t.Fatalf("bare projection = %+v", nobody)
	}

	// The single-entity refresh after a write: the promoted legacy pc reads
	// structured the moment its sheet is written.
	if _, err := store.UpdateEntity(ctx, c.ID, legacy.ID, nil, nil, nil,
		sheet.WithSheet(legacy.Payload, sheet.Sheet{
			Classes: []sheet.ClassLevel{{Class: "rogue", Level: 6}}, AC: 16, MaxHP: 38,
		})); err != nil {
		t.Fatal(err)
	}
	if err := sheet.SyncEntity(ctx, db, c.ID, legacy.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = sheet.Projections(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.EntityID == legacy.ID && (!p.Structured || p.Level != 6 || p.AC != 16) {
			t.Fatalf("post-write projection = %+v", p)
		}
	}

	// A deleted pc drops out of the query surface entirely.
	deleted := "deleted"
	if _, err := store.UpdateEntity(ctx, c.ID, bare.ID, nil, nil, &deleted, nil); err != nil {
		t.Fatal(err)
	}
	if err := sheet.SyncProjections(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows, err = sheet.Projections(ctx, db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.EntityID == bare.ID {
			t.Fatal("a deleted pc must not project")
		}
	}
}

// SyncEntity on a non-pc entity leaves no row behind: the projection
// tracks pcs and nothing else.
func TestSyncEntityIgnoresNonPCs(t *testing.T) {
	db := testdb.Open(t)
	store, err := campaign.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedUser(t, db, "keeper2")
	c, err := store.CreateCampaign(ctx, "keeper2", "Projection Campaign 2", "dnd5e", "")
	if err != nil {
		t.Fatal(err)
	}
	npc, err := store.CreateEntity(ctx, c.ID, "npc", "The Duke", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sheet.SyncEntity(ctx, db, c.ID, npc.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pc_sheet_projection`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("non-pc projected %d rows", n)
	}
}
