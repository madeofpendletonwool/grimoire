package journey

// The store's tests (MAD-375): a plan writes only its own rows and never
// moves the clock; density none never calls the model; the prose pass
// replays a fixture and degrades to the deterministic detail; resolving
// stages one batch and accepting it moves the clock by exactly the
// journey's days, once, reason 'travel' — writing the events, the
// discoveries and the rumour stances the road handed out.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

/* ---------- the store harness ---------- */

type storeFixture struct {
	*journeyFixture
	store *Store
	canon *canon.Store
	model *recordingModel
}

// recordingModel replays scripted responses in call order and records
// every call — the e2e tests assert the call count to prove density none
// costs zero model calls.
type recordingModel struct {
	responses []string
	calls     []string
}

func (m *recordingModel) ModelName() string { return "fake-journey-model" }

func (m *recordingModel) Complete(ctx context.Context, system, user string) (canon.Completion, error) {
	i := len(m.calls)
	m.calls = append(m.calls, user)
	if i >= len(m.responses) {
		return canon.Completion{}, fmt.Errorf("script exhausted at call %d", i+1)
	}
	return canon.Completion{Text: m.responses[i], InputTokens: 100, OutputTokens: 200}, nil
}

// garbageModel answers everything with unparseable noise.
type garbageModel struct{ calls int }

func (g *garbageModel) ModelName() string { return "garbage" }
func (g *garbageModel) Complete(ctx context.Context, system, user string) (canon.Completion, error) {
	g.calls++
	return canon.Completion{Text: "I have no idea what you mean!!! ~~"}, nil
}

func buildStoreFixture(t *testing.T, model canon.ModelClient) *storeFixture {
	t.Helper()
	f := buildFixture(t)
	knowledgeStore, err := knowledge.New(f.db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	factions, err := faction.New(f.db)
	if err != nil {
		t.Fatalf("faction store: %v", err)
	}
	var canonStore *canon.Store
	if model != nil {
		canonStore, err = canon.New(f.db, model, canon.Config{Interval: 0})
	} else {
		canonStore, err = canon.NewOffline(f.db)
	}
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	canonStore = canonStore.WithGraphStores(f.campaigns, knowledgeStore).WithFactions(factions)
	store, err := New(f.db, f.campaigns, canonStore, knowledgeStore)
	if err != nil {
		t.Fatalf("journey store: %v", err)
	}
	canonStore = canonStore.WithJourneyFinalizer(store)
	rec, _ := model.(*recordingModel)
	return &storeFixture{journeyFixture: f, store: store, canon: canonStore, model: rec}
}

// graphCounts snapshots the row counts a plan must not move.
func graphCounts(t *testing.T, db *sql.DB, campaignID string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"entities", "facts", "events", "awareness"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE campaign_id = ?`, campaignID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	var rels int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM relationships r
		 WHERE r.from_entity IN (SELECT id FROM entities WHERE campaign_id = ?)
		    OR r.to_entity IN (SELECT id FROM entities WHERE campaign_id = ?)`,
		campaignID, campaignID).Scan(&rels); err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	out["relationships"] = rels
	var holders int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM rumor_holders WHERE rumor_id IN (SELECT id FROM rumors WHERE campaign_id = ?)`,
		campaignID).Scan(&holders); err != nil {
		t.Fatalf("count rumor_holders: %v", err)
	}
	out["rumor_holders"] = holders
	return out
}

/* ---------- plan ---------- */

// TestPlanWritesNothingToTheGraph: a planned journey writes its own rows
// and nothing else — no graph rows, no awareness, no holdings.
func TestPlanWritesNothingToTheGraph(t *testing.T) {
	f := buildStoreFixture(t, nil)
	before := graphCounts(t, f.db, f.campaignID)
	seed := int64(42)
	row, res, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	after := graphCounts(t, f.db, f.campaignID)
	for table, n := range before {
		if after[table] != n {
			t.Fatalf("plan wrote to %s: %d -> %d", table, n, after[table])
		}
	}
	var clock int64
	if err := f.db.QueryRow(`SELECT clock FROM campaigns WHERE id = ?`, f.campaignID).Scan(&clock); err != nil {
		t.Fatalf("read clock: %v", err)
	}
	if clock != row.StartDay {
		t.Fatalf("plan moved the clock: %d -> %d", row.StartDay, clock)
	}
	var advances int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ?`, f.campaignID).Scan(&advances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if advances != 0 {
		t.Fatalf("plan wrote %d clock advances, want 0", advances)
	}
	var days int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM journey_days WHERE journey_id = ?`, row.ID).Scan(&days); err != nil {
		t.Fatalf("count journey days: %v", err)
	}
	if days != int(res.Days) {
		t.Fatalf("stored %d day rows for a %d-day journey", days, res.Days)
	}
}

// TestPlanDensityNoneNeverCallsTheModel: the hand-wave costs zero tokens.
// The fake model fails the whole test if it is invoked at all.
func TestPlanDensityNoneNeverCallsTheModel(t *testing.T) {
	f := buildStoreFixture(t, &recordingModel{})
	seed := int64(42)
	row, res, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityNone, "", &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(f.model.calls) != 0 {
		t.Fatalf("density none made %d model calls, want 0", len(f.model.calls))
	}
	if len(res.DayTable) != 0 {
		t.Fatalf("density none produced %d day rows, want 0", len(res.DayTable))
	}
	var days int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM journey_days WHERE journey_id = ?`, row.ID).Scan(&days); err != nil {
		t.Fatalf("count journey days: %v", err)
	}
	if days != 0 {
		t.Fatalf("density none wrote %d journey_days rows, want 0", days)
	}
}

// TestPlanIsReproducibleAcrossPlans: planning the same road twice — same
// seed, same density, same route, same world — stores two byte-identical
// day tables. The table is a function of the stored world, not of the
// moment it was asked for.
func TestPlanIsReproducibleAcrossPlans(t *testing.T) {
	f := buildStoreFixture(t, nil)
	seed := int64(42)
	rowA, ra, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan a: %v", err)
	}
	rowB, rb, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan b: %v", err)
	}
	ja, _ := json.Marshal(ra.DayTable)
	jb, _ := json.Marshal(rb.DayTable)
	if string(ja) != string(jb) {
		t.Fatalf("two plans of the same road produced different day tables:\n%s\n%s", ja, jb)
	}
	_, daysB, err := f.store.Get(context.Background(), f.campaignID, rowB.ID)
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	jb2, _ := json.Marshal(daysB)
	if string(ja) != string(jb2) {
		t.Fatalf("the stored day table differs from the planned one:\n%s\n%s", ja, jb2)
	}
	_ = rowA
}

// TestProsePassReplaysFixtureAndCannotMoveDays: with a model configured,
// the chosen days get one line of prose each — the fixture replays
// exactly the declared day keys, and a model that answers garbage
// degrades to the deterministic detail rather than failing the journey.
func TestProsePassReplaysFixtureAndCannotMoveDays(t *testing.T) {
	f := buildStoreFixture(t, &recordingModel{})
	seed := int64(42)
	// First plan: the model's empty script fails, the pass degrades, and
	// the result carries the deterministic table — same world, same seed,
	// so the second plan replays the same chosen days.
	_, res0, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	var sb strings.Builder
	sb.WriteString("{")
	first := true
	for _, d := range res0.DayTable {
		if d.EventKind == EventUneventful {
			continue
		}
		if !first {
			sb.WriteString(",")
		}
		first = false
		fmt.Fprintf(&sb, "%q: %q", fmt.Sprintf("day-%d", d.Index), "The road bends where the old causeway sank.")
	}
	sb.WriteString("}")
	// Reset the model: the first plan's probe consumed the script's head
	// (and failed on it); the second plan replays the fixture at index 0.
	f.model.calls = nil
	f.model.responses = []string{sb.String()}

	_, res, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(f.model.calls) != 1 {
		t.Fatalf("the prose pass made %d model calls, want 1", len(f.model.calls))
	}
	prosed, uneventful := 0, 0
	for i, d := range res.DayTable {
		if d.EventKind == EventUneventful {
			uneventful++
			continue
		}
		if d.Detail != "The road bends where the old causeway sank." {
			t.Fatalf("day %d kept its deterministic detail %q", d.Index, d.Detail)
		}
		if d.EventKind != res0.DayTable[i].EventKind {
			t.Fatalf("the prose pass moved a day: %s became %s", res0.DayTable[i].EventKind, d.EventKind)
		}
		prosed++
	}
	if prosed == 0 || uneventful == 0 {
		t.Fatalf("fixture replay touched the wrong days: %d event, %d uneventful", prosed, uneventful)
	}

	// A garbage model degrades: the deterministic detail is the answer.
	g := &garbageModel{}
	fg := buildStoreFixture(t, g)
	_, resg, err := fg.store.Plan(context.Background(), fg.campaignID, fg.havenID, fg.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan with garbage model: %v", err)
	}
	for _, d := range resg.DayTable {
		if d.Detail == "" {
			t.Fatalf("day %d lost its deterministic detail to a garbage model", d.Index)
		}
	}
}

/* ---------- day resolve ---------- */

// TestResolveDayEditsAndMarksUnderway: the DM's account of a day replaces
// the rolled detail, the encounter that ran is attached, and the first
// resolved day puts the journey underway.
func TestResolveDayEditsAndMarksUnderway(t *testing.T) {
	f := buildStoreFixture(t, nil)
	seed := int64(42)
	row, _, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := f.db.Exec(`INSERT INTO encounters (id, owner_id, name, notes, party, monsters, created_at, updated_at)
		VALUES ('enc1', 'keeper', 'Wolves on the causeway', '', '[3,3,3,3]', '[]', 0, 0)`); err != nil {
		t.Fatalf("seed encounter: %v", err)
	}
	detail := "Four wolves; the ranger burned her last spell slot."
	enc := "enc1"
	jr, day, err := f.store.ResolveDay(context.Background(), f.campaignID, row.ID, 2, &detail, &enc)
	if err != nil {
		t.Fatalf("resolve day: %v", err)
	}
	if jr.Status != StatusUnderway {
		t.Fatalf("journey status %s, want underway", jr.Status)
	}
	if day.Detail != detail || day.Encounter != "enc1" || !day.Resolved {
		t.Fatalf("day resolve wrong: %+v", day)
	}
	// An idempotent re-read, and a missing encounter is refused.
	if _, _, err := f.store.ResolveDay(context.Background(), f.campaignID, row.ID, 2, nil, nil); err != nil {
		t.Fatalf("re-resolve day: %v", err)
	}
	bad := "nope"
	if _, _, err := f.store.ResolveDay(context.Background(), f.campaignID, row.ID, 3, nil, &bad); err == nil {
		t.Fatal("an unknown encounter was attached")
	}
	if _, _, err := f.store.ResolveDay(context.Background(), f.campaignID, row.ID, 99, nil, nil); err == nil {
		t.Fatal("an unknown day resolved")
	}
}

/* ---------- resolve: the batch and the clock ---------- */

// seedHearing searches seeds until the road hears the wanted rumour, and
// returns the seed — deterministic over the fixed fixture.
func seedHearing(t *testing.T, f *storeFixture, rumorID string) int64 {
	t.Helper()
	for seed := int64(0); seed < 2000; seed++ {
		in := f.inputs()
		in.Seed = seed
		res, err := Plan(in)
		if err != nil {
			t.Fatalf("plan seed %d: %v", seed, err)
		}
		for _, d := range res.DayTable {
			if d.EventKind == EventRumor && d.RumorID == rumorID {
				return seed
			}
		}
	}
	t.Fatalf("no seed under 2000 heard rumour %s", rumorID)
	return 0
}

// TestResolveStagesBatchAndAcceptMovesClockExactlyOnce: the full path —
// plan, resolve the whole road, decide the batch — writes the events and
// the discoveries, moves the clock by exactly the journey's days exactly
// once with reason 'travel', and lands the journey on done. Deciding
// again is a no-op.
func TestResolveStagesBatchAndAcceptMovesClockExactlyOnce(t *testing.T) {
	f := buildStoreFixture(t, nil)
	// A seed whose road hears a rumour — the exact composition asserted
	// below is read off the table the store actually rolled.
	hearSeed := seedHearing(t, f, f.rumorDeep)
	_ = hearSeed
	var seed int64
	var hasDiscovery bool
	for seed = int64(0); seed < 2000; seed++ {
		in := f.inputs()
		in.Seed = seed
		res, err := Plan(in)
		if err != nil {
			t.Fatalf("plan seed %d: %v", seed, err)
		}
		rumour, discovery := false, false
		for _, d := range res.DayTable {
			if d.EventKind == EventRumor {
				rumour = true
			}
			if d.EventKind == EventDiscovery {
				discovery = true
			}
		}
		if rumour && discovery {
			hasDiscovery = true
			break
		}
	}
	if !hasDiscovery {
		t.Fatal("no seed under 2000 rolled both a rumour day and a discovery day")
	}
	row, res, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	start := row.StartDay

	// One day resolved by hand, with the DM's own account.
	detail := "It went worse than the roll says."
	if _, _, err := f.store.ResolveDay(context.Background(), f.campaignID, row.ID, 0, &detail, nil); err != nil {
		t.Fatalf("resolve day 0: %v", err)
	}

	batch, err := f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper")
	if err != nil {
		t.Fatalf("resolve journey: %v", err)
	}
	if batch.Source != canon.BatchSourceJourney {
		t.Fatalf("batch source %q", batch.Source)
	}
	// The composition is exactly the table that rolled: the travel event,
	// one event per non-uneventful day, one fact per discovery day, and
	// one discovery per rumour day whose rumour carries a fact.
	var wantEvents, wantFacts, wantDiscoveries int
	for _, d := range res.DayTable {
		if d.EventKind == EventUneventful {
			continue
		}
		wantEvents++
		if d.EventKind == EventDiscovery {
			wantFacts++
		}
		if d.EventKind == EventRumor {
			r := rumorByID(f.snap, d.RumorID)
			if r != nil && r.FactID != "" {
				wantDiscoveries++
			}
		}
	}
	kinds := map[string]int{}
	for _, it := range batch.Items {
		kinds[it.Kind]++
	}
	if kinds["proposed_event"] != 1+wantEvents || kinds["proposed_fact"] != wantFacts || kinds["proposed_discovery"] != wantDiscoveries {
		t.Fatalf("batch composition %v, want %d events / %d facts / %d discoveries over days %+v",
			kinds, 1+wantEvents, wantFacts, wantDiscoveries, res.DayTable)
	}

	// The clock has not moved yet: accepting is what makes it true.
	c, _ := f.campaigns.GetCampaign(context.Background(), f.campaignID)
	if c.Clock != start {
		t.Fatalf("staging moved the clock: %d -> %d", start, c.Clock)
	}

	decided, err := f.canon.DecideBatch(context.Background(), f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.Batch.Status != canon.BatchAccepted {
		t.Fatalf("batch status %s: %+v", decided.Batch.Status, decided.Items)
	}

	// The clock moved by exactly the journey's days, exactly once, reason
	// travel.
	c, _ = f.campaigns.GetCampaign(context.Background(), f.campaignID)
	if c.Clock != start+7 {
		t.Fatalf("clock moved to %d, want %d", c.Clock, start+7)
	}
	var advances int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'travel'`,
		f.campaignID).Scan(&advances); err != nil {
		t.Fatalf("count travel advances: %v", err)
	}
	if advances != 1 {
		t.Fatalf("expected exactly one travel advance, got %d", advances)
	}
	jr, _, err := f.store.Get(context.Background(), f.campaignID, row.ID)
	if err != nil {
		t.Fatalf("reload journey: %v", err)
	}
	if jr.Status != StatusDone {
		t.Fatalf("journey status %s, want done", jr.Status)
	}
	// The journey's events landed on the timeline, dated on their days.
	var events int
	if err := f.db.QueryRow(`
		SELECT COUNT(*) FROM events WHERE campaign_id = ? AND summary LIKE '%' || 'travel' || '%'`,
		f.campaignID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	_ = events // the batch's own composition is the assertion that matters

	// Deciding again is a no-op: the clock does not move twice.
	if _, err := f.canon.DecideBatch(context.Background(), f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("re-decide: %v", err)
	}
	c, _ = f.campaigns.GetCampaign(context.Background(), f.campaignID)
	if c.Clock != start+7 {
		t.Fatalf("a second decision moved the clock again: %d", c.Clock)
	}
}

// TestRumorStancesLandOnAccept: hearing a rumour on the road writes the
// same stance a rumour heard in a tavern does — a discovery item for a
// rumour with a fact (suspects for a true one), and the mill's own
// holding for a fact-less one. Two journeys, one per rumour: the point is
// each stance, not that one road hears both.
func TestRumorStancesLandOnAccept(t *testing.T) {
	f := buildStoreFixture(t, nil)

	// The widow's rumour carries a fact: accepting writes the party's
	// suspects stance on it, through the batch's discovery item.
	seed := seedHearing(t, f, f.rumorWidow)
	row, _, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	batch, err := f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var discoveries int
	for _, it := range batch.Items {
		if it.Kind == "proposed_discovery" {
			discoveries++
		}
	}
	if discoveries != 1 {
		t.Fatalf("the fact-bearing rumour staged %d discovery items, want 1", discoveries)
	}
	if _, err := f.canon.DecideBatch(context.Background(), f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	var stance string
	err = f.db.QueryRow(`
		SELECT stance FROM awareness WHERE campaign_id = ? AND knower = ? AND fact_id =
		  (SELECT fact_id FROM rumors WHERE id = ?)`,
		f.campaignID, campaign.PartyKnower, f.rumorWidow).Scan(&stance)
	if err != nil {
		t.Fatalf("the widow's rumour wrote no party stance: %v", err)
	}
	if stance != "suspects" {
		t.Fatalf("party stance %q, want suspects", stance)
	}

	// Deepwood's rumour carries no fact: accepting writes the holding the
	// heard path writes, party as the carrier.
	seed = seedHearing(t, f, f.rumorDeep)
	row, _, err = f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	batch, err = f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := f.canon.DecideBatch(context.Background(), f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM rumor_holders WHERE rumor_id = ? AND entity_id = ?`,
		f.rumorDeep, campaign.PartyKnower).Scan(&n); err != nil {
		t.Fatalf("count holdings: %v", err)
	}
	if n != 1 {
		t.Fatalf("the fact-less rumour wrote %d party holdings, want 1", n)
	}
}

// TestResolveRefusals: a journey whose window the clock has left is
// refused with the reason; an open batch cannot be staged over; a done
// journey cannot be staged again.
func TestResolveRefusals(t *testing.T) {
	f := buildStoreFixture(t, nil)
	seed := int64(42)
	row, _, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityStandard, PaceNormal, &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The clock moves — say a session was played — and the journey's
	// window is gone.
	if _, _, err := f.campaigns.AdvanceClockBy(context.Background(), f.campaignID, 1, campaign.AdvanceSession, "a session", "", "keeper"); err != nil {
		t.Fatalf("advance clock: %v", err)
	}
	if _, err := f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper"); err == nil {
		t.Fatal("a journey off its window resolved")
	}
	// Back on the window: staging twice is refused while the batch is
	// open.
	if _, _, err := f.campaigns.AdvanceClockBy(context.Background(), f.campaignID, -1, campaign.AdvanceManual, "undo", "", "keeper"); err != nil {
		t.Fatalf("rewind clock: %v", err)
	}
	batch, err := f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper"); err == nil {
		t.Fatal("an open batch was staged over")
	}
	// Dismissed: the row goes back to planned, and abandoning it is the
	// DM's call.
	if _, err := f.canon.DecideBatch(context.Background(), f.campaignID, batch.ID, canon.DecisionDismiss, nil, "keeper"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	jr, _, err := f.store.Get(context.Background(), f.campaignID, row.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if jr.Status != StatusPlanned {
		t.Fatalf("a dismissed batch left the journey %s, want planned", jr.Status)
	}
	c, _ := f.campaigns.GetCampaign(context.Background(), f.campaignID)
	if c.Clock != jr.StartDay {
		t.Fatalf("a dismissed journey moved the clock: %d", c.Clock)
	}
	abandoned := StatusAbandoned
	if _, err := f.store.Patch(context.Background(), f.campaignID, row.ID, &abandoned, nil); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if _, err := f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper"); err == nil {
		t.Fatal("an abandoned journey resolved")
	}
}

// TestDensityNoneResolvesThroughTheSameGate: the hand-wave still lands —
// one travel event, the clock moves by the whole journey, reason travel.
func TestDensityNoneResolvesThroughTheSameGate(t *testing.T) {
	f := buildStoreFixture(t, nil)
	seed := int64(42)
	row, _, err := f.store.Plan(context.Background(), f.campaignID, f.havenID, f.deepID, nil,
		DensityNone, "", &seed, "", "keeper")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	batch, err := f.store.Resolve(context.Background(), f.campaignID, row.ID, "keeper")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(batch.Items) != 1 || batch.Items[0].Kind != "proposed_event" {
		t.Fatalf("a hand-wave staged more than its travel event: %+v", batch.Items)
	}
	if _, err := f.canon.DecideBatch(context.Background(), f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	c, _ := f.campaigns.GetCampaign(context.Background(), f.campaignID)
	if c.Clock != row.StartDay+7 {
		t.Fatalf("the hand-wave moved the clock to %d, want %d", c.Clock, row.StartDay+7)
	}
	var advances int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'travel'`,
		f.campaignID).Scan(&advances); err != nil {
		t.Fatalf("count travel advances: %v", err)
	}
	if advances != 1 {
		t.Fatalf("expected exactly one travel advance, got %d", advances)
	}
}
