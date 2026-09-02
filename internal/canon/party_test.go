package canon

// Campaign-scoped encounters and the party block (MAD-378), at the seam where
// they meet the continuity engine.
//
// The load-bearing test is TestPartyMatchesTheOldEngineRule: the engine no
// longer computes Snapshot.Party by hand, it asks campaign.PartyTableOf, and
// the two must agree exactly. A disagreement re-bands every planned encounter
// in every campaign on upgrade, which is the one outcome this issue promised
// would not happen.

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

// legacyParty is the exact loader canon.LoadSnapshot ran before MAD-378,
// copied verbatim from the commit that this issue replaced. It is the
// reference the block is measured against; do not "clean it up".
func legacyParty(entities []campaign.Entity) []int {
	payloadLevel := func(payload map[string]any) (int, bool) {
		v, ok := payload["level"]
		if !ok {
			return 0, false
		}
		switch lvl := v.(type) {
		case float64:
			return int(lvl), lvl >= 1 && lvl <= 20
		case string:
			n, err := strconv.Atoi(strings.TrimSpace(lvl))
			return n, err == nil && n >= 1 && n <= 20
		default:
			return 0, false
		}
	}
	type pcLevel struct {
		name  string
		level int
	}
	var pcs []pcLevel
	for _, e := range entities {
		if e.Kind != campaign.KindPC || e.Status == campaign.StatusDeleted {
			continue
		}
		if lvl, ok := payloadLevel(e.Payload); ok {
			pcs = append(pcs, pcLevel{name: e.Name, level: lvl})
		}
	}
	sort.Slice(pcs, func(i, j int) bool { return pcs[i].name < pcs[j].name })
	var out []int
	for _, p := range pcs {
		out = append(out, p.level)
	}
	return out
}

func TestPartyMatchesTheOldEngineRule(t *testing.T) {
	// Every shape a pc payload arrives in, including the ones the block now
	// reports on. What the party is must not move for any of them.
	entities := []campaign.Entity{
		{ID: "1", Kind: campaign.KindPC, Name: "Thalia", Status: campaign.StatusActive, Payload: map[string]any{"level": float64(5), "class": "fighter"}},
		{ID: "2", Kind: campaign.KindPC, Name: "Bran", Status: campaign.StatusActive, Payload: map[string]any{"level": "3"}},
		{ID: "3", Kind: campaign.KindPC, Name: "Keth", Status: campaign.StatusActive, Payload: map[string]any{"class": "cleric"}},
		{ID: "4", Kind: campaign.KindPC, Name: "Mira", Status: campaign.StatusDead, Payload: map[string]any{"level": float64(4)}},
		{ID: "5", Kind: campaign.KindPC, Name: "Ghost", Status: campaign.StatusDeleted, Payload: map[string]any{"level": float64(9)}},
		{ID: "6", Kind: campaign.KindPC, Name: "Overlevelled", Status: campaign.StatusActive, Payload: map[string]any{"level": float64(25)}},
		{ID: "7", Kind: campaign.KindPC, Name: "Fractional", Status: campaign.StatusActive, Payload: map[string]any{"level": 20.5}},
		{ID: "8", Kind: campaign.KindPC, Name: "Wordy", Status: campaign.StatusActive, Payload: map[string]any{"level": "five"}},
		{ID: "9", Kind: campaign.KindPC, Name: "Listy", Status: campaign.StatusActive, Payload: map[string]any{"level": []any{5}}},
		{ID: "10", Kind: campaign.KindNPC, Name: "Aaron", Status: campaign.StatusActive, Payload: map[string]any{"level": float64(9)}},
		{ID: "11", Kind: campaign.KindPC, Name: "Zero", Status: campaign.StatusActive, Payload: map[string]any{"level": float64(0)}},
	}
	want := legacyParty(entities)
	got := campaign.PartyTableOf("c1", entities).Levels()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("party block levels %v, the pre-MAD-378 loader %v — findings would move", got, want)
	}
	if len(want) == 0 {
		t.Fatal("the fixture must actually produce levels, or this proves nothing")
	}
}

// The same equality over the real seeded campaign the engine's fixtures use,
// through the real database and the real snapshot load.
func TestPartySnapshotMatchesLoadedSnapshot(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()

	snap, err := LoadSnapshot(ctx, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	table, err := campaign.PartySnapshot(ctx, campaign.ScopeDM, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("party snapshot: %v", err)
	}

	// The seed's four pcs, all level 5.
	if want := []int{5, 5, 5, 5}; !reflect.DeepEqual(snap.Party, want) {
		t.Fatalf("engine party %v, want %v", snap.Party, want)
	}
	if !reflect.DeepEqual(table.Levels(), snap.Party) {
		t.Fatalf("PartySnapshot levels %v, engine party %v", table.Levels(), snap.Party)
	}
	if !reflect.DeepEqual(table.Levels(), legacyParty(snap.Entities)) {
		t.Fatalf("PartySnapshot levels %v, the pre-MAD-378 loader %v", table.Levels(), legacyParty(snap.Entities))
	}
	// The snapshot carries the table itself now, for the surfaces that want
	// the rest of the sheet.
	if snap.PartyTable == nil || len(snap.PartyTable.Members) != 4 {
		t.Fatalf("snapshot party table: %+v", snap.PartyTable)
	}
}

// A malformed pc payload is reported and never fails the load — the engine's
// own acceptance of the same rule the block states.
func TestSnapshotSurvivesMalformedPartyBlock(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO entities (id, campaign_id, kind, name, summary, payload, status, created_at, updated_at)
		VALUES ('pc-broken', ?, 'pc', 'Broken', '', '{"level":"not a level","ac":["x"]}', 'active', 0, 0)`,
		fx.Campaign.ID); err != nil {
		t.Fatalf("insert broken pc: %v", err)
	}

	snap, err := LoadSnapshot(ctx, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("a malformed pc payload must not fail the snapshot load: %v", err)
	}
	if want := []int{5, 5, 5, 5}; !reflect.DeepEqual(snap.Party, want) {
		t.Fatalf("party %v, want %v — the broken pc contributes no level", snap.Party, want)
	}
	var reported bool
	for _, p := range snap.PartyTable.Problems {
		if p.Name == "Broken" && p.Field == "level" {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the malformed block must be reported, not dropped: %+v", snap.PartyTable.Problems)
	}
}

/* ---------- campaign-scoped encounter records ---------- */

// The acceptance criterion: an encounter whose roster names a monster with no
// bestiary entry raises stat_block_unresolved whether it arrived as a session
// event or as an `encounters` row.
func TestCampaignEncounterRecordIsCheckedLikeAnEvent(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO bestiary (key, name, cr, cr_num, xp, type, data, synced_at)
		 VALUES ('goblin', 'Goblin', '1/4', 0.25, 50, 'humanoid', '{}', 0)`); err != nil {
		t.Fatalf("bestiary row: %v", err)
	}
	// The record the builder saved: campaign-scoped, naming a monster the
	// mirror has never heard of.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO encounters (id, owner_id, name, notes, party, monsters, campaign_id, status, created_at, updated_at)
		VALUES ('enc-1', 'keeper', 'The Zorblat Pit', '', '[5,5,5,5]',
		        '[{"name":"Goblin","cr":"1/4","count":6},{"name":"Zorblat","cr":"2","count":1}]',
		        ?, 'planned', ?, ?)`,
		fx.Campaign.ID, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert encounter record: %v", err)
	}

	snap, err := LoadSnapshot(ctx, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snap.Encounters) != 1 {
		t.Fatalf("encounters loaded: %+v", snap.Encounters)
	}
	ref := snap.Encounters[0]
	if ref.EncounterID != "enc-1" || ref.EventID != "" {
		t.Fatalf("a record with no session event stands on its own: %+v", ref)
	}
	// XP is derived from the challenge rating on the way in, the same as an
	// event payload's roster.
	if ref.Monsters[0].XP != 50 {
		t.Fatalf("XP not derived from CR: %+v", ref.Monsters)
	}

	findings := CheckSnapshot(snap, DefaultCheckOptions())
	if n := has(findings, CheckStatBlockUnresolved, "enc-1"); n != 1 {
		t.Fatalf("stat_block_unresolved on the record: got %d, want 1 (%d findings)", n, len(findings))
	}
	for _, f := range findings {
		if f.Check == CheckStatBlockUnresolved && f.RecordID == "enc-1" {
			if f.RecordKind != "encounter" {
				t.Fatalf("record kind %q, want \"encounter\"", f.RecordKind)
			}
			if !strings.Contains(f.Message, "Zorblat") {
				t.Fatalf("message must name the unresolved monster: %q", f.Message)
			}
		}
	}
}

// A record that is the long form of a session event replaces it rather than
// doubling it, and the finding still anchors to the event — so a campaign
// that adopts the record does not see its findings move.
func TestEncounterRecordReplacesItsSessionEvent(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO bestiary (key, name, cr, cr_num, xp, type, data, synced_at)
		 VALUES ('goblin', 'Goblin', '1/4', 0.25, 50, 'humanoid', '{}', 0)`); err != nil {
		t.Fatalf("bestiary row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
		VALUES ('sess-planned', ?, 2, 'Session 2', 'planned', 0, 0)`, fx.Campaign.ID); err != nil {
		t.Fatalf("insert planned session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO session_events (id, session_id, seq, kind, summary, detail, payload, created_at)
		VALUES ('ev-1', 'sess-planned', 1, 'encounter', 'Roadside ambush', '',
		        '{"name":"Roadside ambush","party":[5,5,5,5],"monsters":[{"name":"Goblin","cr":"1/4","count":6}]}', 0)`); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	// The long form the builder saved, with the fuller roster.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO encounters (id, owner_id, name, notes, party, monsters, campaign_id, session_event_id, status, created_at, updated_at)
		VALUES ('enc-1', 'keeper', 'Roadside ambush', 'the long form', '[5,5,5,5]',
		        '[{"name":"Goblin","cr":"1/4","count":6},{"name":"Zorblat","cr":"2","count":1}]',
		        ?, 'ev-1', 'planned', 0, 0)`, fx.Campaign.ID); err != nil {
		t.Fatalf("insert record: %v", err)
	}

	snap, err := LoadSnapshot(ctx, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snap.Encounters) != 1 {
		t.Fatalf("the record is the event's long form, not a second encounter: %+v", snap.Encounters)
	}
	ref := snap.Encounters[0]
	if ref.EventID != "ev-1" || ref.EncounterID != "enc-1" || ref.SessionID != "sess-planned" {
		t.Fatalf("ref: %+v", ref)
	}
	if len(ref.Monsters) != 2 {
		t.Fatalf("the record's roster wins: %+v", ref.Monsters)
	}

	findings := CheckSnapshot(snap, DefaultCheckOptions())
	if n := has(findings, CheckStatBlockUnresolved, "ev-1"); n != 1 {
		t.Fatalf("the finding must still anchor to the session event: got %d for ev-1", n)
	}
	if n := has(findings, CheckStatBlockUnresolved, "enc-1"); n != 0 {
		t.Fatalf("no second finding for the record: got %d for enc-1", n)
	}
}

func TestDiscardedEncounterRecordIsNotPlanned(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO bestiary (key, name, cr, cr_num, xp, type, data, synced_at)
		 VALUES ('goblin', 'Goblin', '1/4', 0.25, 50, 'humanoid', '{}', 0)`); err != nil {
		t.Fatalf("bestiary row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO encounters (id, owner_id, name, notes, party, monsters, campaign_id, status, created_at, updated_at)
		VALUES ('enc-1', 'keeper', 'Abandoned idea', '', '[]',
		        '[{"name":"Zorblat","cr":"2","count":1}]', ?, 'discarded', 0, 0)`,
		fx.Campaign.ID); err != nil {
		t.Fatalf("insert record: %v", err)
	}
	snap, err := LoadSnapshot(ctx, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snap.Encounters) != 0 {
		t.Fatalf("a discarded encounter is not planned: %+v", snap.Encounters)
	}
}

// An owner-scoped encounter — the builder's ordinary save, with no campaign —
// is invisible to every campaign's engine, which is what "behaves identically
// to today" means here.
func TestOwnerScopedEncounterIsNotCampaignMaterial(t *testing.T) {
	db, fx, _ := seeded(t)
	ctx := context.Background()

	store, err := encounter.New(db)
	if err != nil {
		t.Fatalf("encounter store: %v", err)
	}
	if _, err := store.Create(ctx, "keeper", "Personal notes", []int{3, 3},
		[]encounter.Monster{{Name: "Zorblat", CR: "2", XP: 450, Count: 1}}, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	snap, err := LoadSnapshot(ctx, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if len(snap.Encounters) != 0 {
		t.Fatalf("an encounter with no campaign belongs to no campaign: %+v", snap.Encounters)
	}
}
