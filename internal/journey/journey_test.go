package journey

// The pure planner's tests (MAD-375): the day table is a seeded function
// of the route, the density and the seed — byte-identical for the same
// inputs, different for a different seed. The density band lands its
// count for every route length. Every entity a day proposes exists in the
// snapshot (or is a rumour the mill owns). And density none writes no
// days at all, by construction — the store test proves no model is
// called.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite"
)

/* ---------- the harness ---------- */

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journey.db")
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

// journeyFixture is the world the tests run on: a road of three locations
// joined by declared routes (Haven -3 road days-> Ford -4 forest days->
// Deepwood), a shrine off the road at Deepwood, a widow standing at Ford,
// a cult that owns Ford, and two circulating rumours — one the widow
// repeats, one about Deepwood. The campaign seed's own graph rides along
// underneath.
type journeyFixture struct {
	db        *sql.DB
	campaigns *campaign.Store
	snap      *canon.Snapshot
	cal       *clock.Calendar
	locations []campaign.Entity

	campaignID string
	havenID    string
	fordID     string
	deepID     string
	shrineID   string
	widowID    string
	cultID     string
	rumorWidow string // held by the widow, about the mines
	rumorDeep  string // about Deepwood, no fact
}

func buildFixture(t *testing.T) *journeyFixture {
	t.Helper()
	db := openDB(t)
	if _, err := db.Exec(
		`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES ('keeper', 'keeper', 'x', 0, 0)`); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	ctx := context.Background()
	fx, err := campaign.Seed(ctx, db, "keeper", "")
	if err != nil {
		t.Fatalf("campaign seed: %v", err)
	}
	campaigns, err := campaign.New(db)
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}

	mk := func(kind, name, payload string) string {
		t.Helper()
		e, err := campaigns.CreateEntity(ctx, fx.Campaign.ID, kind, name, "", jsonPayload(payload))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return e.ID
	}
	link := func(from, relType, to string) {
		t.Helper()
		if _, err := campaigns.CreateRelationship(ctx, fx.Campaign.ID, from, relType, to, 0, "", ""); err != nil {
			t.Fatalf("link %s %s %s: %v", from, relType, to, err)
		}
	}

	// The road: Haven -> Ford (3 days of road) -> Deepwood (4 days of
	// forest). The routes' placeholders are patched once every id exists,
	// because a route must name an entity that exists.
	haven := mk("location", "Haven", "")
	ford := mk("location", "The Ford", `{"place":{"kind":"hamlet","services":["inn","ferry"]}}`)
	deep := mk("location", "Deepwood", `{"place":{"kind":"wilderness","senses":["birds gone quiet"]}}`)
	patchTravel := func(id, payload string) {
		t.Helper()
		if _, err := campaigns.UpdateEntity(ctx, fx.Campaign.ID, id, nil, nil, nil, jsonPayload(payload)); err != nil {
			t.Fatalf("patch travel on %s: %v", id, err)
		}
	}
	patchTravel(haven, fmt.Sprintf(`{"travel":{"routes":[{"to":"%s","days":3,"terrain":"road"},{"to":"%s","days":9,"terrain":"swamp"}]}}`, ford, deep))
	patchTravel(ford, fmt.Sprintf(`{"travel":{"routes":[{"to":"%s","days":4,"terrain":"forest"}]}}`, deep))

	// The shrine off the road at Deepwood, the widow at the Ford, and the
	// cult's territory over the Ford.
	shrine := mk("location", "The Sunken Shrine", "")
	widow := mk("npc", "Widow Marlow", `{"agent":{"goals":["warn travelers about the woods"]}}`)
	cult := mk("faction", "The Briar Circle", "")
	link(shrine, "located_in", deep)
	link(widow, "located_in", ford)
	link(cult, "owns", ford)

	// Two circulating rumours: one the widow repeats (with a fact), one
	// about Deepwood (fact-less).
	fact, err := campaigns.CreateFact(ctx, fx.Campaign.ID, fx.Mines, "hides", "", "something old",
		"Something old wakes under the Eastern Mines.", "canon", campaign.VisibilitySecret, "seed",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, Quote: "the seed's own substrate"}})
	if err != nil {
		t.Fatalf("create fact: %v", err)
	}
	rumorWidow, err := knowledgeStore.CreateRumor(ctx, fx.Campaign.ID, knowledge.RumorInput{
		Statement: "The mines whistle at night, they say.", Truth: campaign.RumorTruthTrue,
		FactID: fact.ID, AboutEntity: fx.Mines, Spread: campaign.RumorSpreadLocal,
	})
	if err != nil {
		t.Fatalf("create widow rumor: %v", err)
	}
	if _, err := knowledgeStore.SetRumorHolder(ctx, fx.Campaign.ID, rumorWidow.ID, widow, "I heard it from a drover.", ""); err != nil {
		t.Fatalf("set holder: %v", err)
	}
	rumorDeep, err := knowledgeStore.CreateRumor(ctx, fx.Campaign.ID, knowledge.RumorInput{
		Statement: "Deepwood eats surveyors.", Truth: campaign.RumorTruthFalse,
		AboutEntity: deep, Spread: campaign.RumorSpreadLocal,
	})
	if err != nil {
		t.Fatalf("create deepwood rumor: %v", err)
	}

	snap, err := canon.LoadSnapshot(ctx, db, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	snap.Clock = fx.Campaign.Clock
	cal, _, err := campaigns.GetCalendar(ctx, fx.Campaign.ID)
	if err != nil {
		t.Fatalf("load calendar: %v", err)
	}
	locations, err := campaigns.ListEntities(ctx, campaign.ScopeDM, fx.Campaign.ID, campaign.KindLocation)
	if err != nil {
		t.Fatalf("list locations: %v", err)
	}
	return &journeyFixture{
		db: db, campaigns: campaigns, snap: snap, cal: cal, locations: locations,
		campaignID: fx.Campaign.ID, havenID: haven, fordID: ford, deepID: deep,
		shrineID: shrine, widowID: widow, cultID: cult,
		rumorWidow: rumorWidow.ID, rumorDeep: rumorDeep.ID,
	}
}

func jsonPayload(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// inputs is the fixture's standing question: Haven to Deepwood, standard
// density, a fixed seed.
func (f *journeyFixture) inputs() Inputs {
	return Inputs{
		Snapshot: f.snap, Calendar: f.cal, WeatherSeed: f.campaignID,
		Locations: f.locations, From: f.havenID, To: f.deepID,
		Density: DensityStandard, Pace: PaceNormal, Seed: 42,
	}
}

/* ---------- determinism ---------- */

// TestPlanIsByteIdenticalForSameInputs: the acceptance criterion — same
// seed, same density, same route produce the same day-table bytes, and a
// different seed is a different question.
func TestPlanIsByteIdenticalForSameInputs(t *testing.T) {
	f := buildFixture(t)
	a, err := Plan(f.inputs())
	if err != nil {
		t.Fatalf("plan a: %v", err)
	}
	b, err := Plan(f.inputs())
	if err != nil {
		t.Fatalf("plan b: %v", err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("same inputs produced different bytes:\n%s\n%s", ja, jb)
	}
	in := f.inputs()
	in.Seed = 43
	c, err := Plan(in)
	if err != nil {
		t.Fatalf("plan c: %v", err)
	}
	jc, _ := json.Marshal(c)
	if string(ja) == string(jc) {
		t.Fatal("a different seed produced the same day table")
	}

	// The road itself: 3 days of road plus 4 of forest, the short way —
	// not Haven's 9-day swamp declaration.
	if a.Days != 7 {
		t.Fatalf("route cost %d, want 7 (3 road + 4 forest)", a.Days)
	}
	if len(a.Route) != 2 || a.Route[0].Terrain != "road" || a.Route[1].Terrain != "forest" {
		t.Fatalf("route legs wrong: %+v", a.Route)
	}
	if len(a.DayTable) != 7 {
		t.Fatalf("day table has %d days, want 7", len(a.DayTable))
	}
}

// TestPlanDeterminismAcrossFreshLoads recomputes the same question from a
// second process's worth of reads — the same rows loaded again, the same
// bytes out. That is the "across process restarts" guarantee: the table
// is a function of the stored world, not of this process.
func TestPlanDeterminismAcrossFreshLoads(t *testing.T) {
	f := buildFixture(t)
	a, err := Plan(f.inputs())
	if err != nil {
		t.Fatalf("plan a: %v", err)
	}
	// Reload everything the way a restarted server would.
	snap, err := canon.LoadSnapshot(context.Background(), f.db, f.campaignID)
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	snap.Clock = f.snap.Clock
	locations, err := f.campaigns.ListEntities(context.Background(), campaign.ScopeDM, f.campaignID, campaign.KindLocation)
	if err != nil {
		t.Fatalf("reload locations: %v", err)
	}
	in := f.inputs()
	in.Snapshot, in.Locations = snap, locations
	b, err := Plan(in)
	if err != nil {
		t.Fatalf("plan b: %v", err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("a fresh load produced different bytes:\n%s\n%s", ja, jb)
	}
}

/* ---------- the density knob ---------- */

// TestDensityBandLandsItsCount: for every route length and every density,
// the number of event days the roll chose sits inside the band the
// feature publishes. The model is not involved — the roll is ours.
func TestDensityBandLandsItsCount(t *testing.T) {
	f := buildFixture(t)
	for _, density := range []string{DensityLight, DensityStandard, DensityDense} {
		for days := int64(1); days <= 60; days++ {
			override := days
			for _, seed := range []int64{0, 1, 7, 42, 99, 1234567} {
				in := f.inputs()
				in.Density, in.DaysOverride, in.Seed = density, &override, seed
				res, err := Plan(in)
				if err != nil {
					t.Fatalf("plan %s %dd seed %d: %v", density, days, seed, err)
				}
				n := int64(0)
				for _, d := range res.DayTable {
					if d.EventKind != EventUneventful {
						n++
					}
				}
				lo, hi := Band(density, days)
				if n < lo || n > hi {
					t.Fatalf("%s density over %d days (seed %d) landed %d events, outside the band %d..%d",
						density, days, seed, n, lo, hi)
				}
			}
		}
	}
}

// TestDensityNoneIsTheHandWave: no day rows, the line is the answer, and
// the day table is empty by construction — the store test pins the
// zero-model-call half of the criterion.
func TestDensityNoneIsTheHandWave(t *testing.T) {
	f := buildFixture(t)
	in := f.inputs()
	in.Density = DensityNone
	res, err := Plan(in)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(res.DayTable) != 0 {
		t.Fatalf("density none wrote %d day rows, want 0", len(res.DayTable))
	}
	if res.Line == "" || res.Days != 7 {
		t.Fatalf("the hand-wave is wrong: %q over %d days", res.Line, res.Days)
	}
}

/* ---------- the world, not nowhere ---------- */

// TestPlanRollsManyKindsOverTheRoad: over enough seeds, the road's own
// material shows up — the shrine, the widow, the cult's patrol, the
// rumours — and every entity a day proposes exists in the snapshot (or is
// a rumour id the mill owns).
func TestPlanRollsManyKindsOverTheRoad(t *testing.T) {
	f := buildFixture(t)
	entities := map[string]bool{}
	for _, e := range f.snap.Entities {
		entities[e.ID] = true
	}
	rumors := map[string]bool{f.rumorWidow: true, f.rumorDeep: true}
	kinds := map[string]int{}
	shrine, widow, patrol, heard := 0, 0, 0, 0
	for seed := int64(0); seed < 200; seed++ {
		in := f.inputs()
		in.Seed = seed
		res, err := Plan(in)
		if err != nil {
			t.Fatalf("plan seed %d: %v", seed, err)
		}
		for _, d := range res.DayTable {
			if d.EventKind == EventUneventful {
				continue
			}
			kinds[d.EventKind]++
			switch {
			case d.EntityID == "":
				// hazards and encounters name no entity
			case entities[d.EntityID]:
				if d.EventKind == EventDiscovery && d.EntityID == f.shrineID {
					shrine++
				}
				if d.EventKind == EventSocial && d.EntityID == f.widowID {
					widow++
				}
				if d.EventKind == EventSocial && d.EntityID == f.cultID {
					patrol++
				}
			case rumors[d.EntityID]:
				if d.EventKind == EventRumor {
					heard++
					if d.RumorID != d.EntityID {
						t.Fatalf("rumour day %d carries mismatched ids: %s vs %s", d.Index, d.EntityID, d.RumorID)
					}
				}
			default:
				t.Fatalf("day %d proposes %s %q, which neither exists nor is a rumour", d.Index, d.EventKind, d.EntityID)
			}
			if d.EventKind == EventEncounter && d.Encounter == "" {
				t.Fatalf("encounter day %d carries no budget line", d.Index)
			}
		}
	}
	for _, kind := range []string{EventEncounter, EventHazard} {
		if kinds[kind] == 0 {
			t.Fatalf("over 200 seeds the road never rolled a %s", kind)
		}
	}
	if shrine == 0 || widow == 0 || patrol == 0 || heard == 0 {
		t.Fatalf("the world never showed up: shrine %d, widow %d, patrol %d, rumour %d", shrine, widow, patrol, heard)
	}
}

// TestRumorDaysNeedARumourAlongTheLeg: a rumour nobody along the road
// repeats, about nothing on the road, is never a rumour day. The fixture's
// road leaves the mines' rumour reachable only through the widow, who
// stands at the Ford; a road that skips the Ford (the DM's override)
// never hears either.
func TestRumorDaysNeedARumourAlongTheLeg(t *testing.T) {
	f := buildFixture(t)
	heard := 0
	for seed := int64(0); seed < 100; seed++ {
		in := f.inputs()
		in.Seed = seed
		res, err := Plan(in)
		if err != nil {
			t.Fatalf("plan seed %d: %v", seed, err)
		}
		for _, d := range res.DayTable {
			if d.EventKind == EventRumor {
				heard++
				// The only rumour-bearing leg on the routed road is the
				// Ford leg (the widow stands there; Deepwood is rumoured
				// about and is a leg destination too).
				if d.Leg != f.fordID && d.Leg != f.deepID {
					t.Fatalf("rumour heard on leg %q, which carries no rumour", d.Leg)
				}
			}
		}
	}
	if heard == 0 {
		t.Fatal("the routed road never heard either rumour over 100 seeds")
	}
	// The override road names the same endpoints but skips the graph
	// entirely: no legs but the destination, no holders standing there.
	override := int64(7)
	for seed := int64(0); seed < 100; seed++ {
		in := f.inputs()
		in.DaysOverride, in.Seed = &override, seed
		res, err := Plan(in)
		if err != nil {
			t.Fatalf("override plan seed %d: %v", seed, err)
		}
		for _, d := range res.DayTable {
			if d.EventKind == EventRumor && d.Leg == f.deepID {
				// Deepwood itself is rumoured about, and the synthetic
				// leg ends there: hearing it is correct. Only the Ford's
				// widow-held rumour must vanish.
				if d.RumorID == f.rumorWidow {
					t.Fatalf("the widow's rumour was heard on a road that passes no Ford")
				}
			}
		}
	}
}

// TestDiscoveryDaysStayOffTheRoad: a discovery names a child location the
// route itself does not walk through — the shrine, not the Ford.
func TestDiscoveryDaysStayOffTheRoad(t *testing.T) {
	f := buildFixture(t)
	onPath := map[string]bool{f.havenID: true, f.fordID: true, f.deepID: true}
	for seed := int64(0); seed < 200; seed++ {
		in := f.inputs()
		in.Seed = seed
		res, err := Plan(in)
		if err != nil {
			t.Fatalf("plan seed %d: %v", seed, err)
		}
		for _, d := range res.DayTable {
			if d.EventKind == EventDiscovery && onPath[d.EntityID] {
				t.Fatalf("day %d discovered %q, which the route walks through", d.Index, d.EntityID)
			}
		}
	}
}

// TestPlanRefusesTheImpossible: no route and no day count is a refusal
// with the reason, not a guess; a zero-day override is refused; unknown
// endpoints are 404s.
func TestPlanRefusesTheImpossible(t *testing.T) {
	f := buildFixture(t)
	in := f.inputs()
	in.To = "nowhere"
	if _, err := Plan(in); err == nil {
		t.Fatal("an unknown destination planned")
	}
	// A location the road does not reach: the seed's Monastery has no
	// route to Haven.
	in = f.inputs()
	in.To = f.shrineID
	if _, err := Plan(in); err == nil {
		t.Fatal("an unreachable destination planned without a day count")
	}
	zero := int64(0)
	in.DaysOverride = &zero
	if _, err := Plan(in); err == nil {
		t.Fatal("a zero-day journey planned")
	}
	bad := f.inputs()
	bad.Density = "crushing"
	if _, err := Plan(bad); err == nil {
		t.Fatal("an unknown density planned")
	}
}
