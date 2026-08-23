package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite" // same pure-Go driver the app opens the real file with
)

// openDB opens a scratch database with the same DSN shape the app uses and
// applies the migrations — campaign tables exist only through the runner, so
// this is the only correct way to build a campaign store in tests.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "campaign.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := migrate.Up(db); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return db
}

// userIDs mints user rows the membership tables foreign-key. Fixture ids, not
// auth flows: this package only needs the rows to exist.
func userIDs(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := db.Exec(
			`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (?, ?, 'x', 0, 0)`,
			id, "user-"+id); err != nil {
			t.Fatalf("insert user %s: %v", id, err)
		}
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	db := openDB(t)
	s, err := New(db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

// seedCampaign is the minimal campaign + owner every store test starts from.
func seedCampaign(t *testing.T, s *Store) *Campaign {
	t.Helper()
	userIDs(t, s.db, "keeper")
	c, err := s.CreateCampaign(context.Background(), "keeper", "Test Campaign", "dnd5e", "A premise.")
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	return c
}

func TestCreateCampaignMakesOwnerDM(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	if c.Clock != 0 || c.Settings == nil {
		t.Fatalf("new campaign should start at clock 0 with empty settings: %+v", c)
	}
	role, ok, err := s.Role(ctx, c.ID, "keeper")
	if err != nil || !ok || role != RoleDM {
		t.Fatalf("owner must be the dm member: role=%q ok=%v err=%v", role, ok, err)
	}
	// Default deny is a missing row, not an error.
	role, ok, err = s.Role(ctx, c.ID, "stranger")
	if err != nil || ok || role != "" {
		t.Fatalf("no membership row must read as no access: role=%q ok=%v err=%v", role, ok, err)
	}
}

func TestCreateCampaignRejectsUnknownOwner(t *testing.T) {
	s := newStore(t)
	_, err := s.CreateCampaign(context.Background(), "ghost", "Name", "", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for unknown owner, got %v", err)
	}
	if _, err := s.CreateCampaign(context.Background(), "keeper", "  ", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid for blank name, got %v", err)
	}
}

func TestCampaignVisibilityScopesToOwnerOrMember(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	userIDs(t, s.db, "friend", "outsider")
	c := seedCampaign(t, s)

	visible, err := s.ListCampaigns(ctx, "friend")
	if err != nil || len(visible) != 0 {
		t.Fatalf("non-member sees nothing: %v %v", visible, err)
	}
	if err := s.AddMember(ctx, c.ID, "friend", RolePlayer, ""); err != nil {
		t.Fatalf("add member: %v", err)
	}
	visible, err = s.ListCampaigns(ctx, "friend")
	if err != nil || len(visible) != 1 || visible[0].ID != c.ID {
		t.Fatalf("member sees the campaign: %v %v", visible, err)
	}
	if _, ok, _ := s.Role(ctx, c.ID, "outsider"); ok {
		t.Fatal("outsider must not gain access by existing")
	}
}

func TestMembers(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	userIDs(t, s.db, "p1")
	c := seedCampaign(t, s)

	e, err := s.CreateEntity(ctx, c.ID, KindPC, "Thalia", "A fighter.", nil)
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if err := s.AddMember(ctx, c.ID, "p1", RolePlayer, e.ID); err != nil {
		t.Fatalf("add member with character: %v", err)
	}
	if err := s.AddMember(ctx, c.ID, "p1", RoleObserver, ""); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate membership must be ErrAlreadyExists, got %v", err)
	}
	members, err := s.Members(ctx, c.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("want owner + player members: %v %v", members, err)
	}
	if err := s.RemoveMember(ctx, c.ID, "p1"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if _, ok, _ := s.Role(ctx, c.ID, "p1"); ok {
		t.Fatal("removed member must lose access")
	}
}

func TestEntityValidationTable(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	cases := []struct {
		name    string
		kind    string
		enName  string
		wantErr error
	}{
		{"pc", KindPC, "Thalia", nil},
		{"npc", KindNPC, "Tom", nil},
		{"faction", KindFaction, "The Cult", nil},
		{"location", KindLocation, "Blackwater", nil},
		{"item", KindItem, "Silver Key", nil},
		{"deity", KindDeity, "The Verdant God", nil},
		{"organization", KindOrganization, "The Harpers", nil},
		{"creature", KindCreature, "Owlbear", nil},
		{"concept", KindConcept, "The Prophecy", nil},
		{"unknown kind", "dragon", "Smaug", ErrInvalid},
		{"blank name", KindNPC, "  ", ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateEntity(ctx, c.ID, tc.kind, tc.enName, "", nil)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
		})
	}

	if _, err := s.CreateEntity(ctx, "no-such-campaign", KindNPC, "X", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("entity in unknown campaign must be ErrNotFound, got %v", err)
	}
}

func TestEntityNamesAndResolve(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	tom, err := s.CreateEntity(ctx, c.ID, KindNPC, "Tom the Innkeeper", "", nil)
	if err != nil {
		t.Fatalf("create tom: %v", err)
	}
	if _, err := s.AddEntityName(ctx, c.ID, tom.ID, "Thomas Vane", NameAlias); err != nil {
		t.Fatalf("add alias: %v", err)
	}
	if _, err := s.AddEntityName(ctx, c.ID, tom.ID, "the Quiet", NameEpithet); err != nil {
		t.Fatalf("add epithet: %v", err)
	}
	if _, err := s.AddEntityName(ctx, c.ID, tom.ID, "Tom", NameCanonical); !errors.Is(err, ErrInvalid) {
		t.Fatalf("canonical names are managed by create/rename, got %v", err)
	}
	if _, err := s.AddEntityName(ctx, c.ID, tom.ID, "Thomas Vane", NameAlias); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate alias must be ErrAlreadyExists, got %v", err)
	}

	// The canonical name landed in entity_aliases via CreateEntity.
	names, err := s.EntityNames(ctx, ScopeDM, c.ID, tom.ID)
	if err != nil || len(names) != 3 {
		t.Fatalf("want canonical+alias+epithet: %v %v", names, err)
	}

	// Resolve reaches all three, case-insensitively.
	for _, q := range []string{"Tom the Innkeeper", "thomas vane", "THE QUIET"} {
		hits, err := s.ResolveName(ctx, ScopeDM, c.ID, q)
		if err != nil || len(hits) != 1 || hits[0].ID != tom.ID {
			t.Fatalf("resolve %q: %v %v", q, hits, err)
		}
	}

	// Renaming keeps the canonical row in step.
	newName := "Tom of the Waystone"
	if _, err := s.UpdateEntity(ctx, c.ID, tom.ID, &newName, nil, nil, nil); err != nil {
		t.Fatalf("rename: %v", err)
	}
	hits, err := s.ResolveName(ctx, ScopeDM, c.ID, "Tom of the Waystone")
	if err != nil || len(hits) != 1 {
		t.Fatalf("resolve new canonical: %v %v", hits, err)
	}
	if hits, err = s.ResolveName(ctx, ScopeDM, c.ID, "Tom the Innkeeper"); err != nil || len(hits) != 0 {
		t.Fatalf("old canonical should be gone: %v %v", hits, err)
	}
	if hits, err = s.ResolveName(ctx, ScopeDM, c.ID, "Thomas Vane"); err != nil || len(hits) != 1 {
		t.Fatalf("alias should survive a rename: %v %v", hits, err)
	}
}

func TestEntityDeleteIsSoft(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	tom, err := s.CreateEntity(ctx, c.ID, KindNPC, "Tom", "", nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteEntity(ctx, c.ID, tom.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := s.GetEntity(ctx, ScopeDM, c.ID, tom.ID)
	if err != nil {
		t.Fatalf("soft-deleted entity must still load: %v", err)
	}
	if got.Status != StatusDeleted {
		t.Fatalf("want status deleted, got %q", got.Status)
	}
}

func TestEntityStatusValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)
	e, _ := s.CreateEntity(ctx, c.ID, KindNPC, "X", "", nil)

	for _, ok := range []string{StatusActive, StatusInactive, StatusDead, StatusDestroyed, StatusMissing, StatusDeleted} {
		st := ok
		if _, err := s.UpdateEntity(ctx, c.ID, e.ID, nil, nil, &st, nil); err != nil {
			t.Fatalf("status %q should be accepted: %v", ok, err)
		}
	}
	bad := "zombie"
	if _, err := s.UpdateEntity(ctx, c.ID, e.ID, nil, nil, &bad, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status must be ErrInvalid, got %v", err)
	}
}

func TestRelationshipsFromControlledVocabulary(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := seedCampaign(t, s)

	duke, _ := s.CreateEntity(ctx, c.ID, KindNPC, "The Duke", "", nil)
	cult, _ := s.CreateEntity(ctx, c.ID, KindFaction, "The Cult", "", nil)

	if _, err := s.CreateRelationship(ctx, c.ID, duke.ID, "secretly_controls", cult.ID, 0, "", ""); err != nil {
		t.Fatalf("vocabulary edge: %v", err)
	}
	if _, err := s.CreateRelationship(ctx, c.ID, duke.ID, "secretly_mARRIES", cult.ID, 0, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("off-vocabulary edge must be ErrInvalid, got %v", err)
	}
	if _, err := s.CreateRelationship(ctx, c.ID, duke.ID, "knows", duke.ID, 0, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self edge must be ErrInvalid, got %v", err)
	}
	if _, err := s.CreateRelationship(ctx, c.ID, duke.ID, "knows", cult.ID, 200, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("strength out of range must be ErrInvalid, got %v", err)
	}
	if _, err := s.CreateRelationship(ctx, c.ID, duke.ID, "knows", cult.ID, 10, "", ""); err != nil {
		t.Fatalf("in-range edge: %v", err)
	}
	if _, err := s.CreateRelationship(ctx, c.ID, duke.ID, "knows", cult.ID, 10, "", ""); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate edge must be ErrAlreadyExists, got %v", err)
	}

	edges, err := s.RelationshipsOf(ctx, ScopeDM, c.ID, cult.ID)
	if err != nil || len(edges) != 2 {
		t.Fatalf("both directions should come back: %v %v", edges, err)
	}

	// A cross-campaign endpoint reads as not found.
	userIDs(t, s.db, "keeper2")
	other, _ := s.CreateCampaign(ctx, "keeper2", "Other", "", "")
	if _, err := s.CreateRelationship(ctx, other.ID, duke.ID, "knows", cult.ID, 0, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-campaign edge must be ErrNotFound, got %v", err)
	}
}
