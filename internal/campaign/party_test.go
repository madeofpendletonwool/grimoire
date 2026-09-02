package campaign

// The declared party block (MAD-378). The load-bearing property is in
// TestPartyLevelsMatchTheEngineRule and in canon's TestPartySnapshotMatches…:
// Levels() must be bit-for-bit what the continuity engine computed by hand
// before this block existed, or every planned encounter in every campaign
// re-bands on upgrade.

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func pc(id, name string, payload map[string]any) Entity {
	return Entity{ID: id, Kind: KindPC, Name: name, Status: StatusActive, Payload: payload}
}

func TestPartyBlockReadsTheWholeSheet(t *testing.T) {
	e := pc("e1", "Mira", map[string]any{
		"level": float64(5), "class": "wizard", "subclass": "evocation",
		"ac": float64(15), "max_hp": float64(32), "passive_perception": float64(14),
		"saves": map[string]any{"str": float64(-1), "int": float64(7)},
		"resources": map[string]any{
			"spell_slots": map[string]any{"1": float64(4), "2": float64(3)},
			"hit_dice":    float64(5),
		},
		"damage_resistances": []any{"fire", "  "},
		"conditions":         []any{},
		"items":              []any{"item-1", "item-2"},
		"notes":              "  far too curious  ",
	})
	b, problems := PartyBlockOf(&e)
	if len(problems) != 0 {
		t.Fatalf("a well-formed block must report nothing: %+v", problems)
	}
	if b.Level != 5 || b.Class != "wizard" || b.Subclass != "evocation" {
		t.Fatalf("identity: %+v", b)
	}
	if b.AC != 15 || b.MaxHP != 32 || b.PassivePerception != 14 {
		t.Fatalf("numbers: %+v", b)
	}
	if b.Saves["str"] != -1 || b.Saves["int"] != 7 {
		t.Fatalf("saves: %+v", b.Saves)
	}
	if b.Resources.HitDice != 5 || b.Resources.SpellSlots["1"] != 4 || b.Resources.SpellSlots["2"] != 3 {
		t.Fatalf("resources: %+v", b.Resources)
	}
	// Blank list entries are dropped, an empty list decodes to nil, and the
	// notes are trimmed — the same cleaning the npc and place blocks apply.
	if !reflect.DeepEqual(b.DamageResistances, []string{"fire"}) {
		t.Fatalf("damage resistances: %+v", b.DamageResistances)
	}
	if b.Conditions != nil {
		t.Fatalf("an empty list must decode to nil, got %+v", b.Conditions)
	}
	if !reflect.DeepEqual(b.Items, []string{"item-1", "item-2"}) {
		t.Fatalf("items: %+v", b.Items)
	}
	if b.Notes != "far too curious" {
		t.Fatalf("notes: %q", b.Notes)
	}
}

func TestPartyBlockDegradesToTodaysBehaviour(t *testing.T) {
	// The payload the seed writes and every campaign before this block has:
	// a level and a class, nothing else. It must read as exactly that.
	e := pc("e1", "Thalia", map[string]any{"level": float64(5), "class": "fighter"})
	b, problems := PartyBlockOf(&e)
	if len(problems) != 0 {
		t.Fatalf("problems on a bare payload: %+v", problems)
	}
	if b.Level != 5 || b.Class != "fighter" {
		t.Fatalf("bare payload: %+v", b)
	}
	if b.AC != 0 || b.Saves != nil || b.Items != nil || !b.Resources.IsZero() {
		t.Fatalf("undeclared keys must stay zero: %+v", b)
	}

	// A pc that declares nothing at all is the zero block, no problems.
	empty := pc("e2", "Nameless", map[string]any{})
	if b, problems := PartyBlockOf(&empty); b.Level != 0 || len(problems) != 0 {
		t.Fatalf("an empty payload must be the zero block: %+v %+v", b, problems)
	}

	// A non-pc entity is not party material, whatever its payload says.
	npc := Entity{ID: "e3", Kind: KindNPC, Name: "Tom", Payload: map[string]any{"level": float64(9)}}
	if b, problems := PartyBlockOf(&npc); b.Level != 0 || len(problems) != 0 {
		t.Fatalf("an npc must not read as party: %+v %+v", b, problems)
	}
	if b, problems := PartyBlockOf(nil); b.Level != 0 || problems != nil {
		t.Fatalf("a nil entity must be the zero block: %+v %+v", b, problems)
	}
}

// A malformed block is reported, not silently dropped, and never costs the
// keys around it. This is the acceptance criterion stated as a test.
func TestPartyBlockReportsMalformedKeys(t *testing.T) {
	e := pc("e1", "Bran", map[string]any{
		"level":              float64(4),
		"class":              float64(3),        // not a string
		"ac":                 "high",            // not a number
		"max_hp":             float64(-2),       // negative
		"saves":              "none",            // not an object
		"items":              []any{"i1", 7},    // one entry is not a string
		"resources":          []any{"nonsense"}, // not an object
		"passive_perception": "12",              // a number spelled as a string: fine
		"conditions":         []any{"poisoned"}, // fine
	})
	b, problems := PartyBlockOf(&e)

	// Everything that could be read, was.
	if b.Level != 4 || b.PassivePerception != 12 {
		t.Fatalf("readable keys must survive a broken sibling: %+v", b)
	}
	if !reflect.DeepEqual(b.Items, []string{"i1"}) {
		t.Fatalf("the good list entry must survive the bad one: %+v", b.Items)
	}
	if !reflect.DeepEqual(b.Conditions, []string{"poisoned"}) {
		t.Fatalf("conditions: %+v", b.Conditions)
	}
	// Everything that could not, was reported — against the right entity.
	want := map[string]bool{"class": true, "ac": true, "max_hp": true, "saves": true, "items": true, "resources": true}
	got := map[string]bool{}
	for _, p := range problems {
		if p.EntityID != "e1" || p.Name != "Bran" {
			t.Fatalf("a problem must name its entity: %+v", p)
		}
		if p.Detail == "" {
			t.Fatalf("a problem must say what is wrong: %+v", p)
		}
		got[p.Field] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reported fields %v, want %v", got, want)
	}
}

// The level window is the one rule the continuity engine already enforced:
// present, parseable, 1..20. Outside it the level is not usable — and now it
// is also reported instead of vanishing.
func TestPartyBlockLevelWindow(t *testing.T) {
	cases := []struct {
		name    string
		raw     any
		level   int
		problem bool
	}{
		{"in range", float64(5), 5, false},
		{"string spelling", "7", 7, false},
		{"padded string", "  12  ", 12, false},
		{"lowest", float64(1), 1, false},
		{"highest", float64(20), 20, false},
		{"zero", float64(0), 0, true},
		{"negative", float64(-3), 0, true},
		{"above twenty", float64(25), 0, true},
		{"fractional above twenty", 20.5, 0, true},
		{"not a number", "level five", 0, true},
		{"wrong type", []any{5}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := pc("e1", "X", map[string]any{"level": tc.raw})
			b, problems := PartyBlockOf(&e)
			if b.Level != tc.level {
				t.Fatalf("level = %d, want %d", b.Level, tc.level)
			}
			if b.HasLevel() != (tc.level != 0) {
				t.Fatalf("HasLevel() = %v for level %d", b.HasLevel(), b.Level)
			}
			if got := len(problems) > 0; got != tc.problem {
				t.Fatalf("problems = %+v, want reported=%v", problems, tc.problem)
			}
		})
	}
}

func TestWithPartyBlockPreservesForeignKeys(t *testing.T) {
	payload := map[string]any{
		"level": float64(3), "class": "rogue",
		"portrait": "bran.png", // another feature's key
		"ac":       float64(16),
	}
	out := WithPartyBlock(payload, PartyBlock{Level: 8, Class: " rogue ", Notes: "levelled up"})

	if out["portrait"] != "bran.png" {
		t.Fatalf("a key the block does not own was lost: %+v", out)
	}
	if _, still := out["ac"]; still {
		t.Fatalf("a block key left at zero must be removed, not written as 0: %+v", out)
	}
	// The input map is not mutated.
	if payload["level"] != float64(3) {
		t.Fatalf("WithPartyBlock mutated its argument: %+v", payload)
	}

	// And it round-trips through the block reader.
	e := pc("e1", "Bran", out)
	b, problems := PartyBlockOf(&e)
	if len(problems) != 0 {
		t.Fatalf("round-trip reported problems: %+v", problems)
	}
	if b.Level != 8 || b.Class != "rogue" || b.Notes != "levelled up" || b.AC != 0 {
		t.Fatalf("round-trip: %+v", b)
	}
}

// PartyTableOf is the ordering and membership contract Levels() rides on.
func TestPartyTableOrdersByNameAndSkipsNonParty(t *testing.T) {
	entities := []Entity{
		pc("e1", "Thalia", map[string]any{"level": float64(5)}),
		pc("e2", "Bran", map[string]any{"level": float64(4)}),
		{ID: "e3", Kind: KindNPC, Name: "Aaron", Status: StatusActive, Payload: map[string]any{"level": float64(9)}},
		{ID: "e4", Kind: KindPC, Name: "Ghost", Status: StatusDeleted, Payload: map[string]any{"level": float64(1)}},
		pc("e5", "Keth", map[string]any{"class": "cleric"}), // no level declared
	}
	table := PartyTableOf("camp-1", entities)

	if table.CampaignID != "camp-1" {
		t.Fatalf("campaign id: %q", table.CampaignID)
	}
	var names []string
	for _, m := range table.Members {
		names = append(names, m.Name)
	}
	if !reflect.DeepEqual(names, []string{"Bran", "Keth", "Thalia"}) {
		t.Fatalf("members: %v — npcs and deleted pcs are not the party, and the order is by name", names)
	}
	// A pc with no declared level is a member but contributes no level.
	if got := table.Levels(); !reflect.DeepEqual(got, []int{4, 5}) {
		t.Fatalf("levels = %v, want [4 5]", got)
	}
	if table.Size() != 2 {
		t.Fatalf("size = %d, want 2", table.Size())
	}
}

func TestPartyTableWithNoDeclaredLevelsIsNil(t *testing.T) {
	// The rule party_level_drift skips on: nil, not an empty slice, so
	// len(snap.Party) > 0 stays the single predicate.
	table := PartyTableOf("camp-1", []Entity{pc("e1", "Keth", map[string]any{"class": "cleric"})})
	if table.Levels() != nil {
		t.Fatalf("no declared levels must be nil, got %#v", table.Levels())
	}
	if (*PartyTable)(nil).Levels() != nil {
		t.Fatal("a nil table must read as no levels rather than panicking")
	}
}

func TestPartySnapshotIsDMOnly(t *testing.T) {
	s := newStore(t)
	c := seedCampaign(t, s)
	ctx := context.Background()

	for _, scope := range []Scope{ScopeParty, ScopeCharacter("e1"), ScopeNPC("e2")} {
		if _, err := PartySnapshot(ctx, scope, s.db, c.ID); !errors.Is(err, ErrScope) {
			t.Fatalf("scope %s must be refused with ErrScope, got %v", scope, err)
		}
	}
	if _, err := PartySnapshot(ctx, ScopeDM, s.db, c.ID); err != nil {
		t.Fatalf("dm scope: %v", err)
	}
}

func TestPartySnapshotReadsWhatWasWritten(t *testing.T) {
	s := newStore(t)
	c := seedCampaign(t, s)
	ctx := context.Background()

	if _, err := s.CreateEntity(ctx, c.ID, KindPC, "Mira", "Elf wizard.", WithPartyBlock(nil, PartyBlock{
		Level: 5, Class: "wizard", AC: 15, MaxHP: 32,
		Saves:     map[string]int{"int": 7},
		Resources: PartyResources{SpellSlots: map[string]int{"3": 2}, HitDice: 5},
	})); err != nil {
		t.Fatalf("create pc: %v", err)
	}
	if _, err := s.CreateEntity(ctx, c.ID, KindPC, "Bran", "Halfling rogue.",
		map[string]any{"level": 3, "class": "rogue"}); err != nil {
		t.Fatalf("create pc: %v", err)
	}
	// An npc with a level must not join the party.
	if _, err := s.CreateEntity(ctx, c.ID, KindNPC, "Aaron", "A guard.",
		map[string]any{"level": 9}); err != nil {
		t.Fatalf("create npc: %v", err)
	}

	table, err := PartySnapshot(ctx, ScopeDM, s.db, c.ID)
	if err != nil {
		t.Fatalf("party snapshot: %v", err)
	}
	if got := table.Levels(); !reflect.DeepEqual(got, []int{3, 5}) {
		t.Fatalf("levels = %v, want [3 5] (Bran then Mira, by name)", got)
	}
	if len(table.Problems) != 0 {
		t.Fatalf("problems: %+v", table.Problems)
	}
	var mira PartyMember
	for _, m := range table.Members {
		if m.Name == "Mira" {
			mira = m
		}
	}
	// Everything survived the JSON round-trip through SQLite, including the
	// numbers that arrive back as float64.
	if mira.Block.AC != 15 || mira.Block.MaxHP != 32 || mira.Block.Saves["int"] != 7 {
		t.Fatalf("block did not round-trip: %+v", mira.Block)
	}
	if mira.Block.Resources.HitDice != 5 || mira.Block.Resources.SpellSlots["3"] != 2 {
		t.Fatalf("resources did not round-trip: %+v", mira.Block.Resources)
	}
}

// A malformed block must never fail the load — the DM gets their party and a
// report, which is what "reported, not silently dropped" means at this layer.
func TestPartySnapshotSurvivesMalformedPayload(t *testing.T) {
	s := newStore(t)
	c := seedCampaign(t, s)
	ctx := context.Background()

	if _, err := s.CreateEntity(ctx, c.ID, KindPC, "Thalia", "", map[string]any{"level": 5}); err != nil {
		t.Fatalf("create pc: %v", err)
	}
	if _, err := s.CreateEntity(ctx, c.ID, KindPC, "Broken", "", map[string]any{
		"level": "not a level", "ac": []any{1, 2},
	}); err != nil {
		t.Fatalf("create pc: %v", err)
	}

	table, err := PartySnapshot(ctx, ScopeDM, s.db, c.ID)
	if err != nil {
		t.Fatalf("a malformed block must not fail the load: %v", err)
	}
	if got := table.Levels(); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("levels = %v, want [5] — the broken pc contributes none", got)
	}
	if len(table.Members) != 2 {
		t.Fatalf("the broken pc is still a member: %+v", table.Members)
	}
	fields := map[string]bool{}
	for _, p := range table.Problems {
		if p.Name != "Broken" {
			t.Fatalf("problem attributed to the wrong pc: %+v", p)
		}
		fields[p.Field] = true
	}
	if !fields["level"] || !fields["ac"] {
		t.Fatalf("both broken keys must be reported: %+v", table.Problems)
	}
}
