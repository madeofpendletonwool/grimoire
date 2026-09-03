package encounter

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/index"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "encounters.db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := New(store.DB())
	if err != nil {
		t.Fatalf("new encounter store: %v", err)
	}
	return s
}

func TestStoreCRUD(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Create(ctx, "alice", "Ambush on the Triboar Trail", []int{1, 1, 2}, []Monster{
		{Name: "Goblin", CR: "1/4", XP: 50, Count: 4},
	}, "## The pitch\nA scouting party, badly hidden.")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create did not mint an id")
	}

	got, err := s.Get(ctx, "alice", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Ambush on the Triboar Trail" || !reflect.DeepEqual(got.Party, []int{1, 1, 2}) || len(got.Monsters) != 1 || got.Monsters[0].Count != 4 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !strings.Contains(got.Notes, "badly hidden") {
		t.Fatalf("design notes not round-tripped: %q", got.Notes)
	}

	name := "Renamed"
	newParty := []int{3, 3, 3, 2}
	updated, err := s.Update(ctx, "alice", created.ID, &name, newParty, []Monster{
		{Name: "Bugbear", CR: "1", XP: 200, Count: 1},
		{Name: "Hobgoblin", CR: "1/2", XP: 100, Count: 3},
	}, nil, true, true)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "Renamed" || len(updated.Party) != 4 || len(updated.Monsters) != 2 {
		t.Fatalf("update mismatch: %+v", updated)
	}
	if !updated.UpdatedAt.After(updated.CreatedAt) && !updated.UpdatedAt.Equal(updated.CreatedAt) {
		t.Fatalf("updated_at not moved: %+v", updated)
	}

	list, err := s.List(ctx, "alice")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (err %v), want 1", list, err)
	}

	if err := s.Delete(ctx, "alice", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "alice", created.ID); err != ErrNotFound {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "alice", created.ID); err != ErrNotFound {
		t.Fatalf("double delete = %v, want ErrNotFound", err)
	}
}

// Another user's encounters are invisible: foreign reads, updates, and
// deletes all report not-found and leave the row untouched.
func TestStoreOwnerScoping(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	created, err := s.Create(ctx, "alice", "Keep", []int{5}, []Monster{{Name: "Ogre", CR: "2", XP: 450, Count: 2}}, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.Get(ctx, "bob", created.ID); err != ErrNotFound {
		t.Fatalf("foreign get = %v, want ErrNotFound", err)
	}
	if _, err := s.Update(ctx, "bob", created.ID, nil, nil, nil, nil, false, false); err != ErrNotFound {
		t.Fatalf("foreign update = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "bob", created.ID); err != ErrNotFound {
		t.Fatalf("foreign delete = %v, want ErrNotFound", err)
	}
	if list, err := s.List(ctx, "bob"); err != nil || len(list) != 0 {
		t.Fatalf("bob sees %v (err %v), want nothing", list, err)
	}
	got, err := s.Get(ctx, "alice", created.ID)
	if err != nil || got.Name != "Keep" {
		t.Fatalf("alice's encounter disturbed by foreign access: %+v (%v)", got, err)
	}
}

func TestStoreUpdateKeepsUnsentFields(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	created, err := s.Create(ctx, "alice", "Waves", []int{2, 2, 3}, []Monster{{Name: "Hobgoblin", CR: "1/2", XP: 100, Count: 6}}, "keep me")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name := "Waves, revised"
	updated, err := s.Update(ctx, "alice", created.ID, &name, nil, nil, nil, false, false)
	if err != nil {
		t.Fatalf("rename-only update: %v", err)
	}
	if updated.Name != "Waves, revised" || len(updated.Party) != 3 || len(updated.Monsters) != 1 || updated.Monsters[0].Count != 6 {
		t.Fatalf("rename-only update lost fields: %+v", updated)
	}
	if updated.Notes != "keep me" {
		t.Fatalf("rename-only update lost the notes: %q", updated.Notes)
	}
}

/* ---------- campaign scope (MAD-378) ---------- */

// The zero Scope is exactly Create: an owner-scoped encounter with no
// campaign — the fallback surface, which must not regress.
func TestStoreCampaignScope(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	scoped, err := s.CreateIn(ctx, "alice", Scope{CampaignID: "camp-1"}, "The Zorblat Pit",
		[]int{5, 5, 5, 5}, []Monster{{Name: "Zorblat", CR: "2", XP: 450, Count: 1}}, "notes")
	if err != nil {
		t.Fatalf("create in campaign: %v", err)
	}
	if scoped.CampaignID != "camp-1" || scoped.Status != StatusPlanned {
		t.Fatalf("scoped encounter: %+v", scoped)
	}
	if scoped.SessionEventID != "" || scoped.SceneID != "" || scoped.Objective != nil || scoped.Terrain != nil {
		t.Fatalf("reserved columns must stay empty: %+v", scoped)
	}

	plain, err := s.Create(ctx, "alice", "No campaign", []int{3}, []Monster{{Name: "Goblin", CR: "1/4", XP: 50, Count: 1}}, "")
	if err != nil {
		t.Fatalf("plain create: %v", err)
	}
	if plain.CampaignID != "" || plain.Status != StatusPlanned {
		t.Fatalf("an owner-scoped encounter is planned and belongs to no campaign: %+v", plain)
	}

	// The campaign reads its own, regardless of which member wrote them.
	list, err := s.ListCampaign(ctx, "camp-1")
	if err != nil || len(list) != 1 || list[0].ID != scoped.ID {
		t.Fatalf("list campaign = %v (err %v), want the scoped one", list, err)
	}
	if _, err := s.ListCampaign(ctx, ""); err == nil {
		t.Fatal("an empty campaign id must be refused, not return everything")
	}

	got, err := s.GetCampaign(ctx, "camp-1", scoped.ID)
	if err != nil || got.Name != "The Zorblat Pit" {
		t.Fatalf("get campaign encounter: %+v (%v)", got, err)
	}
	// A foreign campaign id is indistinguishable from a missing one.
	if _, err := s.GetCampaign(ctx, "camp-2", scoped.ID); err != ErrNotFound {
		t.Fatalf("foreign campaign get = %v, want ErrNotFound", err)
	}

	// The builder's own picker still shows both: a campaign encounter is
	// still its author's.
	mine, err := s.List(ctx, "alice")
	if err != nil || len(mine) != 2 {
		t.Fatalf("owner list = %d encounters (err %v), want 2", len(mine), err)
	}
}

// The vocabulary the CHECK constraint enforces is refused here first, with
// ErrInvalid rather than a SQL error; and a session event or scene without a
// campaign is nonsense — there is nothing for the id to belong to.
func TestStoreRejectsBadScope(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.CreateIn(ctx, "alice", Scope{CampaignID: "c", Status: "finished"}, "X", nil, nil, ""); err == nil {
		t.Fatal("an unknown status must be refused")
	} else if !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateIn(ctx, "alice", Scope{SessionEventID: "ev-1"}, "X", nil, nil, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a session event without a campaign = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateIn(ctx, "alice", Scope{SceneID: "scene-1"}, "X", nil, nil, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a scene without a campaign = %v, want ErrInvalid", err)
	}
}

// The objective and terrain options (MAD-380) round-trip through the
// objective and terrain columns migration 0026 reserved — and anything
// outside the declared vocabularies is refused before a write happens.
func TestStoreObjectiveAndTerrain(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.Create(ctx, "alice", "Hold the Bridge", []int{3, 3, 3, 3}, []Monster{
		{Name: "Goblin", CR: "1/4", XP: 50, Count: 4},
	}, "", WithObjective(Objective{Kind: Survive, Rounds: 6}), WithTerrain(Terrain{
		Features: []Feature{{Kind: "chokepoint", Effect: featureEffects["chokepoint"], Area: "the bridge"}},
		Hazards: []Hazard{{Kind: "rockfall", Name: "Rockfall", SaveAbility: "dex", DC: 15,
			Damage: "2d10", DamageType: "bludgeoning", Trigger: "a creature starts its turn under the span", Area: "the span"}},
	}))
	if err != nil {
		t.Fatalf("create with objective: %v", err)
	}
	got, err := s.Get(ctx, "alice", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Objective == nil || got.Objective.Kind != Survive || got.Objective.Rounds != 6 {
		t.Fatalf("objective did not round-trip: %+v", got.Objective)
	}
	if got.Terrain == nil || len(got.Terrain.Features) != 1 || len(got.Terrain.Hazards) != 1 {
		t.Fatalf("terrain did not round-trip: %+v", got.Terrain)
	}
	if got.Terrain.Hazards[0].DC != 15 || got.Terrain.Hazards[0].Damage != "2d10" {
		t.Errorf("hazard details lost: %+v", got.Terrain.Hazards[0])
	}

	// An update without the options leaves both alone.
	if _, err := s.Update(ctx, "alice", created.ID, nil, nil, nil, nil, false, false); err != nil {
		t.Fatalf("rename without options: %v", err)
	}
	if got, _ := s.Get(ctx, "alice", created.ID); got.Objective == nil || got.Terrain == nil {
		t.Fatal("a plain update dropped the objective or terrain")
	}

	// WithTerrain(Terrain{}) clears the battlefield; the objective stays.
	if _, err := s.Update(ctx, "alice", created.ID, nil, nil, nil, nil, false, false,
		WithTerrain(Terrain{})); err != nil {
		t.Fatalf("clear terrain: %v", err)
	}
	got, _ = s.Get(ctx, "alice", created.ID)
	if got.Terrain != nil {
		t.Errorf("terrain not cleared: %+v", got.Terrain)
	}
	if got.Objective == nil {
		t.Error("the objective was lost with the terrain")
	}

	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"unknown objective kind", []Option{WithObjective(Objective{Kind: "escort"})}},
		{"rounds out of range", []Option{WithObjective(Objective{Kind: Survive, Rounds: 99})}},
		{"unknown feature kind", []Option{WithTerrain(Terrain{Features: []Feature{{Kind: "lava"}}})}},
		{"hazard outside the grammar", []Option{WithTerrain(Terrain{Hazards: []Hazard{{Kind: "rockfall", SaveAbility: "dex", DC: 15, Damage: "lots", DamageType: "bludgeoning", Trigger: "t", Area: "a"}}})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Create(ctx, "alice", tc.name, nil, nil, "", tc.opts...); !errors.Is(err, ErrInvalid) {
				t.Fatalf("create = %v, want ErrInvalid", err)
			}
		})
	}

	// An explicit defeat objective stores as declared: it is a kind the DM
	// picked, not absence.
	defeated, err := s.Create(ctx, "alice", "Plain", nil, nil, "", WithObjective(Objective{Kind: Defeat}))
	if err != nil {
		t.Fatalf("explicit defeat: %v", err)
	}
	if got, _ := s.Get(ctx, "alice", defeated.ID); got.Objective == nil || got.Objective.Kind != Defeat {
		t.Errorf("explicit defeat lost: %+v", got.Objective)
	}
}

// A database that has never run migration 0026 — the package's own DDL grew
// the campaign columns instead — round-trips its old encounters through the
// campaign-aware store unchanged.
func TestStoreAdoptsPreCampaignTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "encounters.db")
	store, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	if _, err := db.Exec(`CREATE TABLE encounters (
		id          TEXT PRIMARY KEY,
		owner_id    TEXT NOT NULL,
		name        TEXT NOT NULL DEFAULT '',
		notes       TEXT NOT NULL DEFAULT '',
		party       TEXT NOT NULL DEFAULT '[]',
		monsters    TEXT NOT NULL DEFAULT '[]',
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("pre-campaign table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO encounters (id, owner_id, name, notes, party, monsters, created_at, updated_at)
		VALUES ('e1', 'keeper', 'Old faithful', 'keep', '[3,3]', '[{"name":"Goblin","cr":"1/4","count":6}]', 1, 2)`); err != nil {
		t.Fatalf("seed old encounter: %v", err)
	}

	s, err := New(db)
	if err != nil {
		t.Fatalf("new store over the old table: %v", err)
	}
	ctx := context.Background()
	got, err := s.Get(ctx, "keeper", "e1")
	if err != nil {
		t.Fatalf("get the old encounter: %v", err)
	}
	if got.Name != "Old faithful" || got.Notes != "keep" || !reflect.DeepEqual(got.Party, []int{3, 3}) || len(got.Monsters) != 1 {
		t.Fatalf("the old encounter did not survive adoption: %+v", got)
	}
	if got.CampaignID != "" || got.Status != StatusPlanned {
		t.Fatalf("adopted defaults: %+v", got)
	}
	// And the adopted table takes a campaign-scoped write.
	if _, err := s.CreateIn(ctx, "keeper", Scope{CampaignID: "c1"}, "New", []int{5}, nil, ""); err != nil {
		t.Fatalf("create in campaign on the adopted table: %v", err)
	}
}
