package sim

// Simulation tick tests (MAD-367). The pure tests pin the acceptance
// criteria: byte-identical results for the same inputs, fourteen 1-day
// windows agreeing with one 14-day window on the final plan state, missed
// schedules reported rather than fired. The store tests pin the rest: a
// preview writes nothing, staging goes through the review gate, accepting
// moves the clock exactly once by exactly the window, and the offline and
// garbage-model paths still answer.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/clock"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite" // same pure-Go driver the app opens the real file with
)

/* ---------- the harness ---------- */

// openDB opens a scratch database the way the app does and applies the
// migrations — the sim tables exist only through the runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sim.db")
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

// tickFixture is the world the tests run on: the campaign seed's graph, two
// rival factions joined by enemy_of, a cultist NPC with a goal who belongs
// to one of them, an active plan with a public rival, and a schedule.
type tickFixture struct {
	db        *sql.DB
	campaigns *campaign.Store
	factions  *faction.Store
	canon     *canon.Store
	sim       *Store

	campaignID string
	cultID     string // the faction with the plan
	orderID    string // its enemy
	cultistID  string // a cultist with a goal
	townID     string // a location

	planID   string
	entryID  string
	missedID string
}

// machine is the fixture plan's machine: gathering -> ritual -> ascension.
func machine() campaign.StateMachine {
	return campaign.StateMachine{
		Initial: "gathering",
		States:  campaign.States("gathering", "ritual", "ascension"),
		Edges: []campaign.StateEdge{
			{From: "gathering", To: "ritual"},
			{From: "ritual", To: "ascension"},
		},
	}
}

func buildFixture(t *testing.T, model canon.ModelClient) *tickFixture {
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
	factions, err := faction.New(db)
	if err != nil {
		t.Fatalf("faction store: %v", err)
	}
	knowledgeStore, err := knowledge.New(db)
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	var canonStore *canon.Store
	if model != nil {
		canonStore, err = canon.New(db, model, canon.Config{Interval: 0})
	} else {
		canonStore, err = canon.NewOffline(db)
	}
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	canonStore = canonStore.WithGraphStores(campaigns, knowledgeStore).WithFactions(factions)
	simStore, err := New(db, campaigns, factions, canonStore)
	if err != nil {
		t.Fatalf("sim store: %v", err)
	}
	canonStore = canonStore.WithTickFinalizer(simStore)

	// The Ashen Order opposes the seed's cult; a cultist belongs to the
	// cult and pursues a goal.
	mk := func(kind, name, payload string) string {
		t.Helper()
		e, err := campaigns.CreateEntity(ctx, fx.Campaign.ID, kind, name, "", jsonPayload(payload))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return e.ID
	}
	order := mk("faction", "The Ashen Order", "")
	cultist := mk("npc", "Sister Vela", `{"agent":{"goals":["free the Duke's bound heir","destroy the Ashen Order"]}}`)
	town := mk("location", "Greyfall", "")
	link := func(from, relType, to string) {
		t.Helper()
		if _, err := campaigns.CreateRelationship(ctx, fx.Campaign.ID, from, relType, to, 0, "", ""); err != nil {
			t.Fatalf("link %s %s %s: %v", from, relType, to, err)
		}
	}
	link(fx.Cult, "enemy_of", order)
	link(cultist, "member_of", fx.Cult)

	// The cult's plan: 3 days of gathering, then 5 more to the ritual. Its
	// moves are public — the order reacts.
	plan, err := factions.CreatePlan(ctx, fx.Campaign.ID, fx.Cult, faction.PlanInput{
		Name:    "The Vernal Rite",
		Machine: machine(),
		Steps: []faction.Step{
			{State: "ritual", Name: "Prepare the rite", Cost: 3},
			{State: "ascension", Name: "Complete it", Cost: 5},
		},
		RatePerDay: 1,
		Status:     faction.PlanActive,
		Visibility: campaign.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := factions.UpdatePlan(ctx, fx.Campaign.ID, plan.ID, faction.UpdatePlanInput{}); err != nil {
		t.Fatalf("start plan: %v", err)
	}

	// The schedule: a festival due inside the window, and a pending entry
	// the clock has already passed — missed, reported, never fired.
	entry, err := campaigns.CreateScheduledEvent(ctx, fx.Campaign.ID, campaign.ScheduleInput{
		Name: ptr("The Lamplight Festival"), Day: ptr[int64](5), Visibility: ptr(campaign.VisibilityPublic),
	})
	if err != nil {
		t.Fatalf("create schedule entry: %v", err)
	}
	missed, err := campaigns.CreateScheduledEvent(ctx, fx.Campaign.ID, campaign.ScheduleInput{
		Name: ptr("The old moot"), Day: ptr[int64](-2), Visibility: ptr(campaign.VisibilityPublic),
	})
	if err != nil {
		t.Fatalf("create missed entry: %v", err)
	}
	_ = town
	return &tickFixture{
		db: db, campaigns: campaigns, factions: factions, canon: canonStore, sim: simStore,
		campaignID: fx.Campaign.ID, cultID: fx.Cult, orderID: order, cultistID: cultist, townID: town,
		planID: plan.ID, entryID: entry.ID, missedID: missed.ID,
	}
}

func jsonPayload(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func ptr[T any](v T) *T { return &v }

// loadInputs is the test-facing load the store uses.
func (f *tickFixture) loadInputs(t *testing.T) *tickInputs {
	t.Helper()
	in, err := f.sim.loadInputs(context.Background(), f.campaignID)
	if err != nil {
		t.Fatalf("load inputs: %v", err)
	}
	return in
}

/* ---------- the pure function ---------- */

// TestTickIsByteIdenticalForSameInputs: two runs over the same snapshot,
// day count and seed produce identical JSON — across calls, and (given the
// same loaded rows) across processes.
func TestTickIsByteIdenticalForSameInputs(t *testing.T) {
	f := buildFixture(t, nil)
	in := f.loadInputs(t)
	a := Tick(in.snapshot, in.calendar, in.plans, in.entries, 14, 42)
	b := Tick(in.snapshot, in.calendar, in.plans, in.entries, 14, 42)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("same inputs produced different bytes:\n%s\n%s", ja, jb)
	}
	// A different seed is a different question: the digest differs (and so
	// may the seeded days and reaction modes).
	c := Tick(in.snapshot, in.calendar, in.plans, in.entries, 14, 43)
	if c.Digest == a.Digest {
		t.Fatal("different seed produced the same digest")
	}
	// A changed snapshot is a changed campaign: the digest differs.
	in.snapshot.Clock = 99
	d := Tick(in.snapshot, in.calendar, in.plans, in.entries, 14, 42)
	if d.Digest == a.Digest {
		t.Fatal("a changed snapshot produced the same digest")
	}
}

// TestFourteenOneDayTicksAgreeWithOneFourteenDayTick is MAD-367's
// acceptance criterion: overflow carries, so the final plan state is the
// same whether the window moves one day at a time or all at once.
func TestFourteenOneDayTicksAgreeWithOneFourteenDayTick(t *testing.T) {
	f := buildFixture(t, nil)
	in := f.loadInputs(t)

	one := Tick(in.snapshot, in.calendar, in.plans, in.entries, 14, 7)
	if !one.Plans[0].Moved {
		t.Fatal("the 14-day window should move the plan")
	}

	// Fourteen 1-day windows, chaining the advanced plans forward and
	// moving the clock with them, under the same seed.
	plans := in.plans
	snap := in.snapshot
	for i := 0; i < 14; i++ {
		res := Tick(snap, in.calendar, plans, in.entries, 1, 7)
		next := make([]faction.Plan, 0, len(plans))
		for j := range res.Plans {
			next = append(next, res.Plans[j].Advanced)
		}
		plans = next
		snap = copyClock(snap, plans, res.ToDay)
	}
	if got := plans[0].CurrentState; got != one.Plans[0].Advanced.CurrentState {
		t.Fatalf("final plan state disagrees: 14x1day = %s, 1x14day = %s", got, one.Plans[0].Advanced.CurrentState)
	}
	if got := plans[0].Progress; got != one.Plans[0].Advanced.Progress {
		t.Fatalf("final plan progress disagrees: %v vs %v", got, one.Plans[0].Advanced.Progress)
	}
	if got, want := strings.Join(plans[0].Reached, ","), strings.Join(one.Plans[0].Advanced.Reached, ","); got != want {
		t.Fatalf("reached states disagree: %q vs %q", got, want)
	}
}

// copyClock returns the snapshot with the clock moved — the chaining state
// a 1-day-at-a-time caller carries forward.
func copyClock(snap *canon.Snapshot, plans []faction.Plan, day int64) *canon.Snapshot {
	next := *snap
	next.Clock = day
	return &next
}

// TestTickReportsMissedScheduleNotFiredLate: a pending entry behind the
// clock appears in Missed and never in Due.
func TestTickReportsMissedScheduleNotFiredLate(t *testing.T) {
	f := buildFixture(t, nil)
	in := f.loadInputs(t)
	res := Tick(in.snapshot, in.calendar, in.plans, in.entries, 14, 1)
	found := false
	for _, m := range res.Missed {
		if m.EntryID == f.missedID {
			found = true
		}
	}
	if !found {
		t.Fatal("the passed pending entry was not reported as missed")
	}
	for _, d := range res.Due {
		if d.EntryID == f.missedID {
			t.Fatal("the passed pending entry fired inside the window")
		}
	}
	// The festival, due on day 5, does come due.
	due := false
	for _, d := range res.Due {
		if d.EntryID == f.entryID && d.Day == 5 {
			due = true
		}
	}
	if !due {
		t.Fatal("the in-window festival did not come due")
	}
}

// TestTickNPCActionsAndConsequences: the cultist acts on the first goal
// (order is priority), once; the enemy order reacts to the public move.
func TestTickNPCActionsAndConsequences(t *testing.T) {
	f := buildFixture(t, nil)
	in := f.loadInputs(t)
	res := Tick(in.snapshot, in.calendar, in.plans, in.entries, 4, 9)
	if len(res.Actions) != 1 || res.Actions[0].NPC != f.cultistID {
		t.Fatalf("expected one cultist action, got %+v", res.Actions)
	}
	if goal := res.Actions[0].Goal; goal != "free the Duke's bound heir" {
		t.Fatalf("the first goal is the priority, got %q", goal)
	}
	if res.Actions[0].Day < res.FromDay || res.Actions[0].Day >= res.ToDay {
		t.Fatalf("action day outside the window: %d", res.Actions[0].Day)
	}
	if len(res.Consequences) != 1 || res.Consequences[0].Reactor != f.orderID {
		t.Fatalf("expected one reaction from the enemy order, got %+v", res.Consequences)
	}
	if res.Consequences[0].Mode == "" || !strings.Contains(res.Consequences[0].Summary, "Ashen Order") {
		t.Fatalf("the reaction carries no mode/summary: %+v", res.Consequences[0])
	}

	// A dormant plan moves nothing: advance the plan to completion first is
	// overkill — a fresh fixture with a dormant plan triggers nobody.
	res2 := Tick(in.snapshot, in.calendar, dormant(in.plans), in.entries, 4, 9)
	if len(res2.Actions) != 0 || len(res2.Consequences) != 0 {
		t.Fatalf("a dormant plan should trigger no actions or reactions: %+v %+v", res2.Actions, res2.Consequences)
	}
}

func dormant(plans []faction.Plan) []faction.Plan {
	out := make([]faction.Plan, len(plans))
	copy(out, plans)
	for i := range out {
		out[i].Status = faction.PlanDormant
	}
	return out
}

// TestTickSecretPlanMoveIsNotPublicized: only a public plan's move becomes
// a fact candidate.
func TestTickSecretPlanMoveIsNotPublicized(t *testing.T) {
	f := buildFixture(t, nil)
	in := f.loadInputs(t)
	plans := make([]faction.Plan, len(in.plans))
	copy(plans, in.plans)
	plans[0].Visibility = campaign.VisibilitySecret
	res := Tick(in.snapshot, in.calendar, plans, in.entries, 4, 9)
	items := batchItems(&res, "")
	for _, it := range items {
		if it.Kind == "fact" {
			t.Fatalf("a secret plan move was publicized: %+v", it)
		}
	}
}

/* ---------- the store ---------- */

// graphCounts snapshots the row counts a preview must not move.
func graphCounts(t *testing.T, db *sql.DB, campaignID string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"entities", "facts", "events"} {
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
	return out
}

// TestPreviewWritesNothingToTheGraph: a preview is a question. Only the
// sim_ticks row appears.
func TestPreviewWritesNothingToTheGraph(t *testing.T) {
	f := buildFixture(t, nil)
	before := graphCounts(t, f.db, f.campaignID)
	pv, err := f.sim.Preview(context.Background(), f.campaignID, 14, nil, "keeper")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	after := graphCounts(t, f.db, f.campaignID)
	for table, n := range before {
		if after[table] != n {
			t.Fatalf("preview wrote to %s: %d -> %d", table, n, after[table])
		}
	}
	var ticks int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sim_ticks WHERE campaign_id = ?`, f.campaignID).Scan(&ticks); err != nil {
		t.Fatalf("count sim_ticks: %v", err)
	}
	if ticks != 1 {
		t.Fatalf("expected exactly one sim_ticks row, got %d", ticks)
	}
	if pv.Tick.Status != TickPreview || pv.Result.FromDay != 0 || pv.Result.ToDay != 14 {
		t.Fatalf("preview row/result wrong: %+v %+v", pv.Tick, pv.Result)
	}
	if pv.Flavour != nil {
		t.Fatal("an offline store must not produce flavour")
	}
}

// TestPreviewWithGarbageModelStillAnswers: a model that returns junk
// degrades to the deterministic summary rather than failing the tick.
func TestPreviewWithGarbageModelStillAnswers(t *testing.T) {
	f := buildFixture(t, &garbageModel{})
	pv, err := f.sim.Preview(context.Background(), f.campaignID, 14, nil, "keeper")
	if err != nil {
		t.Fatalf("preview with garbage model: %v", err)
	}
	if len(pv.Result.Plans) == 0 || !pv.Result.Plans[0].Moved {
		t.Fatalf("deterministic outcomes missing: %+v", pv.Result.Plans)
	}
	if pv.Flavour != nil {
		t.Fatal("a failed flavour pass must degrade to no flavour")
	}
}

// garbageModel answers everything with unparseable noise.
type garbageModel struct{}

func (g *garbageModel) ModelName() string { return "garbage" }
func (g *garbageModel) Complete(ctx context.Context, system, user string) (canon.Completion, error) {
	return canon.Completion{Text: "I have no idea what you mean!!! ~~"}, nil
}

// TestStageAndAcceptMovesClockExactlyOnce: the full path — preview, stage,
// decide through the ordinary batch endpoint — moves the clock by exactly
// the window exactly once, applies the plan advance, and lands the row on
// applied. Deciding again is a no-op.
func TestStageAndAcceptMovesClockExactlyOnce(t *testing.T) {
	f := buildFixture(t, nil)
	ctx := context.Background()
	pv, err := f.sim.Preview(ctx, f.campaignID, 14, nil, "keeper")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	batch, _, err := f.sim.StageTick(ctx, f.campaignID, pv.Tick.ID, "keeper")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if batch.Source != canon.BatchSourceTick {
		t.Fatalf("batch source %q", batch.Source)
	}
	kinds := map[string]int{}
	for _, it := range batch.Items {
		kinds[it.Kind]++
	}
	if kinds["proposed_plan_transition"] != 1 || kinds["proposed_event"] == 0 || kinds["proposed_fact"] == 0 {
		t.Fatalf("unexpected batch composition: %v", kinds)
	}

	res, err := f.canon.DecideBatch(ctx, f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if res.Batch.Status != canon.BatchAccepted {
		t.Fatalf("batch status %s: %+v", res.Batch.Status, res.Items)
	}

	// The clock moved exactly once, by exactly the window, reason tick.
	var advances int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'tick'`, f.campaignID).Scan(&advances); err != nil {
		t.Fatalf("count advances: %v", err)
	}
	if advances != 1 {
		t.Fatalf("expected exactly one tick advance, got %d", advances)
	}
	c, err := f.campaigns.GetCampaign(ctx, f.campaignID)
	if err != nil {
		t.Fatalf("reload campaign: %v", err)
	}
	if c.Clock != 14 {
		t.Fatalf("clock moved to %d, want 14", c.Clock)
	}

	// The plan applied: the ritual was entered (14 days x rate 1 pays the
	// 3-cost step and carries 11 toward the 5-cost one... the second step
	// costs 5, so ascension is entered too, with carry 6 and nowhere to
	// spend it — the plan completes).
	plan, err := f.factions.GetPlan(ctx, campaign.ScopeDM, f.campaignID, f.planID)
	if err != nil {
		t.Fatalf("reload plan: %v", err)
	}
	if plan.CurrentState != "ascension" || plan.Status != faction.PlanComplete {
		t.Fatalf("plan did not apply: %s %s", plan.CurrentState, plan.Status)
	}
	var transitions int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM faction_plan_transitions WHERE plan_id = ?`, f.planID).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if transitions != 2 {
		t.Fatalf("expected 2 recorded moves, got %d", transitions)
	}

	// The row landed on applied; a re-decision moves nothing.
	row, err := f.sim.tickInCampaign(ctx, pv.Tick.ID, f.campaignID)
	if err != nil {
		t.Fatalf("reload tick: %v", err)
	}
	if row.Status != TickApplied {
		t.Fatalf("tick status %s, want applied", row.Status)
	}
	if _, err := f.canon.DecideBatch(ctx, f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("re-decide: %v", err)
	}
	c2, _ := f.campaigns.GetCampaign(ctx, f.campaignID)
	if c2.Clock != 14 {
		t.Fatalf("a re-decision moved the clock to %d", c2.Clock)
	}
}

// TestDismissedTickDiscardsTime: a dismissed batch leaves the graph alone
// and the clock unmoved — time passes only when the DM says it did.
func TestDismissedTickDiscardsTime(t *testing.T) {
	f := buildFixture(t, nil)
	ctx := context.Background()
	before := graphCounts(t, f.db, f.campaignID)
	pv, err := f.sim.Preview(ctx, f.campaignID, 14, nil, "keeper")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	batch, _, err := f.sim.StageTick(ctx, f.campaignID, pv.Tick.ID, "keeper")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := f.canon.DecideBatch(ctx, f.campaignID, batch.ID, canon.DecisionDismiss, nil, "keeper"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	c, _ := f.campaigns.GetCampaign(ctx, f.campaignID)
	if c.Clock != 0 {
		t.Fatalf("a dismissed tick moved the clock to %d", c.Clock)
	}
	after := graphCounts(t, f.db, f.campaignID)
	for table, n := range before {
		if after[table] != n {
			t.Fatalf("a dismissed tick wrote to %s: %d -> %d", table, n, after[table])
		}
	}
	row, _ := f.sim.tickInCampaign(ctx, pv.Tick.ID, f.campaignID)
	if row.Status != TickDiscarded {
		t.Fatalf("tick status %s, want discarded", row.Status)
	}
}

// TestStageRefusesStaleAndMovedPreviews: the digest makes a stale preview
// detectable, and a moved clock invalidates the window.
func TestStageRefusesStaleAndMovedPreviews(t *testing.T) {
	f := buildFixture(t, nil)
	ctx := context.Background()
	pv, err := f.sim.Preview(ctx, f.campaignID, 14, nil, "keeper")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	// Change the world: a new entity shifts the snapshot digest.
	if _, err := f.campaigns.CreateEntity(ctx, f.campaignID, "npc", "A Stranger", "", nil); err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if _, _, err := f.sim.StageTick(ctx, f.campaignID, pv.Tick.ID, "keeper"); err == nil {
		t.Fatal("staging a stale preview must fail")
	} else if !strings.Contains(err.Error(), "changed since this preview") {
		t.Fatalf("unexpected stale error: %v", err)
	}

	// A fresh preview whose clock moves before staging is refused on the
	// window, not the digest.
	pv2, err := f.sim.Preview(ctx, f.campaignID, 7, nil, "keeper")
	if err != nil {
		t.Fatalf("preview 2: %v", err)
	}
	if _, _, err := f.campaigns.AdvanceClockBy(ctx, f.campaignID, 1, campaign.AdvanceManual, "a day passes", "", "keeper"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, _, err := f.sim.StageTick(ctx, f.campaignID, pv2.Tick.ID, "keeper"); err == nil {
		t.Fatal("staging a preview whose clock moved must fail")
	} else if !strings.Contains(err.Error(), "run a new simulation") {
		t.Fatalf("unexpected moved-clock error: %v", err)
	}

	// A preview stages exactly once.
	pv3, err := f.sim.Preview(ctx, f.campaignID, 7, nil, "keeper")
	if err != nil {
		t.Fatalf("preview 3: %v", err)
	}
	if _, _, err := f.sim.StageTick(ctx, f.campaignID, pv3.Tick.ID, "keeper"); err != nil {
		t.Fatalf("stage 3: %v", err)
	}
	if _, _, err := f.sim.StageTick(ctx, f.campaignID, pv3.Tick.ID, "keeper"); err == nil {
		t.Fatal("staging a staged tick must fail")
	}
}

// TestStageRequiresOutcomes: a window where nothing happens is a report,
// not a proposal.
func TestStageRequiresOutcomes(t *testing.T) {
	f := buildFixture(t, nil)
	ctx := context.Background()
	// Suspend the plan and point the schedule outside the window.
	if _, err := f.factions.UpdatePlan(ctx, f.campaignID, f.planID, faction.UpdatePlanInput{Status: ptr(faction.PlanDormant)}); err != nil {
		t.Fatalf("suspend plan: %v", err)
	}
	if _, err := f.campaigns.UpdateScheduledEvent(ctx, f.campaignID, f.entryID, campaign.ScheduleInput{
		Day: ptr[int64](500),
	}); err != nil {
		t.Fatalf("move entry: %v", err)
	}
	pv, err := f.sim.Preview(ctx, f.campaignID, 3, nil, "keeper")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if _, _, err := f.sim.StageTick(ctx, f.campaignID, pv.Tick.ID, "keeper"); err == nil {
		t.Fatal("staging an empty window must fail")
	}
}

/* ---------- the calendar is only the reckoning ---------- */

// TestTickUsesTheCalendarForRecurrence pins that Due's expansion — not a
// naive day compare — decides what falls due: a yearly festival lands on
// the same month/day of the next year.
func TestTickUsesTheCalendarForRecurrence(t *testing.T) {
	f := buildFixture(t, nil)
	ctx := context.Background()
	yearly, err := f.campaigns.CreateScheduledEvent(ctx, f.campaignID, campaign.ScheduleInput{
		Name: ptr("The Turning"), Day: ptr[int64](400), Recurrence: ptr("yearly"),
	})
	if err != nil {
		t.Fatalf("create yearly: %v", err)
	}
	in := f.loadInputs(t)
	// Default calendar: 360-day years. The Turning is declared on day 400
	// (Secondmonth 10 of year 2); yearly recurrence re-fires it on the same
	// month/day of year 1 — day 40 — inside the [0, 60) window.
	res := Tick(in.snapshot, in.calendar, in.plans, in.entries, 60, 1)
	found := false
	for _, d := range res.Due {
		if d.EntryID == yearly.ID && d.Day == 40 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the yearly entry did not expand onto day 40: %+v", res.Due)
	}
	// And the calendar reckons it: day 40 of the Common Reckoning is in
	// Secondmonth of year 1.
	if got := in.calendar.FormatShort(40); !strings.Contains(got, "Secondmonth 1") {
		t.Fatalf("day 40 should land in Secondmonth of year 1, got %q", got)
	}
	_ = clock.Default()
}
