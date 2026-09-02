package downtime

// Downtime resolution tests (MAD-368). The pure tests pin the acceptance
// criteria: byte-identical results for the same inputs and seed, findings
// plus world movement over a plan's window, travel without a route refused
// with the reason, and the leak test — a result for character X carries no
// fact X has no path to. The store tests pin the rest: a request writes
// nothing but its row, staging goes through the review gate, accepting
// grants awareness to the requesting character and to nobody else, and the
// clock moves by exactly the window exactly once under reason 'downtime'.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	_ "modernc.org/sqlite" // same pure-Go driver the app opens the real file with
)

/* ---------- the harness ---------- */

// openDB opens a scratch database the way the app does and applies the
// migrations — the downtime tables exist only through the runner.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "downtime.db")
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

// downtimeFixture is the world the tests run on: the campaign seed's graph
// and knowledge layer, Thalia positioned in Blackwater, routes out of
// Blackwater, cult facts with and without reachable sources, a rival
// public fact, and the cult's plan — which a 21-day window completes.
type downtimeFixture struct {
	db        *sql.DB
	campaigns *campaign.Store
	factions  *faction.Store
	canon     *canon.Store
	knowledge *knowledge.Store
	store     *Store

	campaignID string
	fx         *campaign.Fixture

	greyfallID string // 2 days from Blackwater by road
	farholdID  string // no route at all

	// Cult-touching facts, by their relationship to Thalia's path.
	FactRecruits   string // public, held by Tom at Blackwater — findable
	FactRiteKey    string // secret, held by nobody — never a candidate
	FactHermit     string // secret, held only by an unreachable hermit
	FactWorship    string // public, the party already knows it
	FactProposed   string // proposed — invisible to every path
	FactSuperseded string // superseded — not current truth
	FactOffSubject string // about the Duke, not the cult

	planID string
}

func ptr[T any](v T) *T { return &v }

func buildFixture(t *testing.T) *downtimeFixture {
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
	canonStore, err := canon.NewOffline(db)
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	canonStore = canonStore.WithGraphStores(campaigns, knowledgeStore).WithFactions(factions)
	store, err := New(db, campaigns, factions, canonStore)
	if err != nil {
		t.Fatalf("downtime store: %v", err)
	}
	canonStore = canonStore.WithDowntimeFinalizer(store)

	f := &downtimeFixture{
		db: db, campaigns: campaigns, factions: factions, canon: canonStore,
		knowledge: knowledgeStore, store: store,
		campaignID: fx.Campaign.ID, fx: fx,
	}

	mk := func(kind, name, summary string, payload map[string]any) string {
		t.Helper()
		e, err := campaigns.CreateEntity(ctx, fx.Campaign.ID, kind, name, summary, payload)
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
	mkFact := func(subject, predicate, objectLiteral, statement, visibility, confidence string) string {
		t.Helper()
		fact, err := campaigns.CreateFact(ctx, fx.Campaign.ID, subject, predicate, "", objectLiteral,
			statement, confidence, visibility, "keeper",
			[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, Quote: statement, AcceptedBy: "keeper"}})
		if err != nil {
			t.Fatalf("create fact %q: %v", statement, err)
		}
		return fact.ID
	}

	// The geography: Thalia stands in Blackwater; Greyfall is two days out
	// by road; Far Hold is beyond any road at all.
	link(fx.Thalia, "located_in", fx.Blackwater)
	greyfall := mk("location", "Greyfall Crossing", "", map[string]any{
		"travel": map[string]any{"routes": []map[string]any{{"to": fx.Blackwater, "days": 2}}},
	})
	farhold := mk("location", "Far Hold", "", nil)
	f.greyfallID, f.farholdID = greyfall, farhold

	// A hermit at Far Hold who knows a cult secret nobody reachable does:
	// an unreachable source grants nothing.
	hermit := mk("npc", "The Greyfall Hermit", "", nil)
	link(hermit, "located_in", farhold)

	// Cult facts, one per path Thalia does or does not have.
	f.FactRecruits = mkFact(fx.Cult, "recruits", "among the mining camps",
		"The Cult of the Root recruits among the mining camps.",
		campaign.VisibilityPublic, campaign.ConfidenceCanon)
	f.FactRiteKey = mkFact(fx.Cult, "rite_needs", "the Silver Key",
		"The Cult's great rite requires the Silver Key.",
		campaign.VisibilitySecret, campaign.ConfidenceCanon)
	f.FactHermit = mkFact(fx.Cult, "funds", "the Vane charter",
		"The Cult is funded by the Vane family charter.",
		campaign.VisibilitySecret, campaign.ConfidenceCanon)
	f.FactWorship = mkFact(fx.Cult, "worships_literal", "the Verdant God",
		"The Cult of the Root worships the Verdant God.",
		campaign.VisibilityPublic, campaign.ConfidenceCanon)
	f.FactOffSubject = mkFact(fx.Duke, "is", "pale and precise",
		"The Duke is pale, precise, never seen eating.",
		campaign.VisibilityPublic, campaign.ConfidenceCanon)

	// The proposed cult fact: doubly invisible — proposed facts never
	// surface through any path.
	proposed, err := campaigns.CreateFact(ctx, fx.Campaign.ID, fx.Cult, "plans", "", "a second rite",
		"The Cult plans a second rite at Greyfall.",
		campaign.ConfidenceProposed, campaign.VisibilityPublic, "keeper",
		[]campaign.ProvenanceInput{{
			SessionID: "seed", SourceID: "seed-transcript", SpanStart: 4, SpanEnd: 40,
			Quote: "they will hold a second rite at the falls", Method: campaign.MethodExtracted,
		}})
	if err != nil {
		t.Fatalf("create proposed fact: %v", err)
	}
	f.FactProposed = proposed.ID

	// The superseded cult fact: retconned history is not current truth.
	old := mkFact(fx.Cult, "meets", "at the Waystone",
		"The Cult meets at the Waystone inn.",
		campaign.VisibilityPublic, campaign.ConfidenceCanon)
	replacement, err := campaigns.CreateFact(ctx, fx.Campaign.ID, fx.Cult, "meets", "", "in the flooded quarry",
		"The Cult meets in the flooded quarry.",
		campaign.ConfidenceCanon, campaign.VisibilityPublic, "keeper",
		[]campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, Quote: "quarry", AcceptedBy: "keeper"}})
	if err != nil {
		t.Fatalf("create replacement fact: %v", err)
	}
	if err := campaigns.SupersedeFact(ctx, fx.Campaign.ID, old, replacement.ID); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	f.FactSuperseded = old

	// The sources' knowledge: Tom — standing in Blackwater, cost zero —
	// knows the recruiting fact and suspects the Duke's pallor; the party
	// already knows the worship fact; the hermit alone knows the charter
	// funding.
	if _, err := knowledgeStore.SetAwareness(ctx, fx.Campaign.ID, fx.Tom, f.FactRecruits,
		knowledge.StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("tom knows recruits: %v", err)
	}
	if _, err := knowledgeStore.SetAwareness(ctx, fx.Campaign.ID, fx.Tom, f.FactOffSubject,
		knowledge.StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("tom knows duke: %v", err)
	}
	if _, err := knowledgeStore.SetAwareness(ctx, fx.Campaign.ID, campaign.PartyKnower, f.FactWorship,
		knowledge.StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("party knows worship: %v", err)
	}
	if _, err := knowledgeStore.SetAwareness(ctx, fx.Campaign.ID, hermit, f.FactHermit,
		knowledge.StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("hermit knows funding: %v", err)
	}

	// The cult's plan: 3 days of gathering then 5 more to the rite — a
	// 21-day window completes it, publicly.
	plan, err := factions.CreatePlan(ctx, fx.Campaign.ID, fx.Cult, faction.PlanInput{
		Name: "The Vernal Rite",
		Machine: campaign.StateMachine{
			Initial: "gathering",
			States:  campaign.States("gathering", "ritual", "ascension"),
			Edges: []campaign.StateEdge{
				{From: "gathering", To: "ritual"},
				{From: "ritual", To: "ascension"},
			},
		},
		Steps: []faction.Step{
			{State: "ritual", Name: "Prepare the rite", Cost: 3},
			{State: "ascension", Name: "Complete it", Cost: 5},
		},
		RatePerDay: 1, Status: faction.PlanActive, Visibility: campaign.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := factions.UpdatePlan(ctx, fx.Campaign.ID, plan.ID, faction.UpdatePlanInput{}); err != nil {
		t.Fatalf("start plan: %v", err)
	}
	f.planID = plan.ID
	return f
}

// resolve is the test-facing pure call over the fixture's loaded inputs.
func (f *downtimeFixture) resolve(t *testing.T, character, activity, subject string, days int, seed int64) *Result {
	t.Helper()
	in, err := f.store.loadInputs(context.Background(), f.campaignID)
	if err != nil {
		t.Fatalf("load inputs: %v", err)
	}
	in.Character, in.Activity, in.Subject, in.Days, in.Seed = character, activity, subject, days, seed
	res, err := Resolve(*in)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return res
}

/* ---------- the activity vocabulary ---------- */

func TestReadActivity(t *testing.T) {
	direct := map[string]string{
		"research":  ActivityResearch,
		"Travel":    ActivityTravel,
		"carousing": ActivityCarouse,
	}
	for text, want := range direct {
		got, err := ReadActivity(text)
		if err != nil || got != want {
			t.Fatalf("ReadActivity(%q) = %q, %v; want %q", text, got, err, want)
		}
	}
	free := map[string]string{
		"I spend three weeks researching the cult": ActivityResearch,
		"study the cult in the library":            ActivityResearch,
		"I want to drink at the tavern":            ActivityCarouse,
		"practice with the sword":                  ActivityTrain,
		"earn wages at the docks":                  ActivityWork,
		"rest and heal up":                         ActivityRecuperate,
		"sail to Greyfall":                         ActivityTravel,
		"plot against the Duke":                    ActivityScheme,
		"forge a silver blade":                     ActivityCraft,
	}
	for text, want := range free {
		got, err := ReadActivity(text)
		if err != nil || got != want {
			t.Fatalf("ReadActivity(%q) = %q, %v; want %q", text, got, err, want)
		}
	}
	// Unmappable: a question, not a guess.
	var clarify *ClarifyError
	if _, err := ReadActivity("I meditate upon the void"); !errors.As(err, &clarify) || len(clarify.Candidates) != 0 {
		t.Fatalf("unknown activity must clarify with no candidates: %v", err)
	}
	if _, err := ReadActivity("I drink and study"); !errors.As(err, &clarify) || len(clarify.Candidates) != 2 {
		t.Fatalf("ambiguous activity must clarify with both candidates: %v", err)
	}
	if _, err := ReadActivity("  "); !errors.As(err, &clarify) {
		t.Fatalf("empty activity must clarify: %v", err)
	}
}

/* ---------- determinism ---------- */

// TestSameSeedSameResult is MAD-368's acceptance criterion: two identical
// downtime requests with the same seed produce the same result — the same
// bytes, findings, scores and tick.
func TestSameSeedSameResult(t *testing.T) {
	f := buildFixture(t)
	a := f.resolve(t, f.fx.Thalia, "I spend three weeks researching the cult", f.fx.Cult, 21, 42)
	b := f.resolve(t, f.fx.Thalia, "research", f.fx.Cult, 21, 42)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("the same question phrased twice produced different bytes:\n%s\n%s", ja, jb)
	}
	c := f.resolve(t, f.fx.Thalia, "research", f.fx.Cult, 21, 43)
	if c.Digest == a.Digest {
		t.Fatal("a different seed produced the same digest")
	}
	d := f.resolve(t, f.fx.Bran, "research", f.fx.Cult, 21, 42)
	if d.Digest == a.Digest {
		t.Fatal("a different character produced the same digest")
	}

	// Through the store too: two recorded requests under one explicit seed.
	ctx := context.Background()
	row1, res1, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "research the cult", f.fx.Cult, 21, ptr[int64](7), "keeper")
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	_, res2, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "dig into the cult", f.fx.Cult, 21, ptr[int64](7), "keeper")
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	j1, _ := json.Marshal(res1)
	j2, _ := json.Marshal(res2)
	if string(j1) != string(j2) {
		t.Fatalf("two requests under one seed diverged:\n%s\n%s", j1, j2)
	}
	if row1.Seed != 7 || res1.Seed != 7 {
		t.Fatalf("explicit seed not honored: %+v %+v", row1, res1)
	}
}

/* ---------- findings and the world's movement ---------- */

// TestFindingsAndWorldMovement is the acceptance criterion: downtime over a
// window in which the faction's plan completed reports both the character's
// findings and the world's movement.
func TestFindingsAndWorldMovement(t *testing.T) {
	f := buildFixture(t)
	res := f.resolve(t, f.fx.Thalia, "research", f.fx.Cult, 21, 42)

	if len(res.Findings) == 0 {
		t.Fatal("three weeks of research with a knowing source at cost zero found nothing")
	}
	found := map[string]Finding{}
	for _, fd := range res.Findings {
		found[fd.FactID] = fd
	}
	if fd, ok := found[f.FactRecruits]; !ok {
		t.Fatalf("the recruiting fact did not land: %+v", res.Findings)
	} else if fd.Stance != "knows" && fd.Stance != "suspects" {
		t.Fatalf("landing stance %q", fd.Stance)
	}
	// The Duke's pallor is public and Tom knows it, but it is not about the
	// subject: the activity's subject bounds the candidates.
	if _, ok := found[f.FactOffSubject]; ok {
		t.Fatal("a fact off the subject landed")
	}

	// The world moved: the cult's plan completes inside 21 days.
	if res.Tick == nil || len(res.Tick.Plans) != 1 || !res.Tick.Plans[0].Moved {
		t.Fatalf("the window's tick is missing or dormant: %+v", res.Tick)
	}
	if res.Tick.Plans[0].Advanced.Status != faction.PlanComplete {
		t.Fatalf("the plan did not complete: %+v", res.Tick.Plans[0].Advanced)
	}

	// Reachability: Blackwater at 0, Greyfall at 2, Far Hold never.
	days := map[string]int64{}
	for _, loc := range res.Reachable {
		days[loc.ID] = loc.Days
	}
	if days[f.fx.Blackwater] != 0 || days[f.greyfallID] != 2 {
		t.Fatalf("reachable set wrong: %+v", res.Reachable)
	}
	if _, ok := days[f.farholdID]; ok {
		t.Fatal("Far Hold is reachable without a route")
	}
	// And Blackwater carries its source.
	for _, loc := range res.Reachable {
		if loc.ID == f.fx.Blackwater && !loc.Source {
			t.Fatal("Blackwater carries its NPC source flag")
		}
	}
}

// TestTravelNeedsARoute is the acceptance criterion: downtime naming a
// location with no route from the character's position is refused with the
// reason, not silently resolved.
func TestTravelNeedsARoute(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	in, err := f.store.loadInputs(ctx, f.campaignID)
	if err != nil {
		t.Fatalf("load inputs: %v", err)
	}
	in.Character, in.Activity, in.Subject, in.Days, in.Seed = f.fx.Thalia, "travel", f.farholdID, 21, 1
	_, err = Resolve(*in)
	if err == nil {
		t.Fatal("travel to an unrouted location resolved silently")
	}
	if !strings.Contains(err.Error(), "no route between Blackwater and Far Hold") {
		t.Fatalf("the refusal must name both ends: %v", err)
	}

	// A routed journey within the window resolves and records its cost.
	res := f.resolve(t, f.fx.Thalia, "travel", f.greyfallID, 21, 1)
	if res.TravelDays != 2 {
		t.Fatalf("travel days = %d, want 2", res.TravelDays)
	}

	// A journey longer than the window is refused with the reason too.
	long := map[string]any{"travel": map[string]any{"routes": []map[string]any{{"to": f.fx.Blackwater, "days": 30}}}}
	if _, err := f.campaigns.CreateEntity(ctx, f.campaignID, "location", "The Far Coast", "", long); err != nil {
		t.Fatalf("create far coast: %v", err)
	}
	in2, _ := f.store.loadInputs(ctx, f.campaignID)
	in2.Character, in2.Activity, in2.Subject, in2.Days, in2.Seed = f.fx.Thalia, "travel", "", 3, 1
	var coast string
	for _, e := range in2.Snapshot.Entities {
		if e.Name == "The Far Coast" {
			coast = e.ID
		}
	}
	in2.Subject = coast
	if _, err := Resolve(*in2); err == nil || !strings.Contains(err.Error(), "the road from") {
		t.Fatalf("an uncoverable journey must be refused with the reason: %v", err)
	}
}

/* ---------- the leak test ---------- */

// The leak test (MAD-368's hard gate, shaped like internal/knowledge's):
// a downtime result for character X contains no fact X has no path to.
//
// The planted paths, and what each would leak if a filter regressed:
//
//   - FactRiteKey: a cult secret nobody holds — a candidate only if a
//     reachable source could plausibly carry it; there is none, so it must
//     never appear, not even as a failed candidate's statement.
//   - FactHermit: a cult secret only an unreachable hermit holds — source
//     quality outside the reachable set buys nothing.
//   - FactProposed: proposed facts are invisible to every path.
//   - FactSuperseded: retconned history is not current truth.
//   - FactWorship: the party holds it — a character-scope read already
//     returns it, so it is not a discovery.
//   - FactOffSubject: public, held by a reachable source, but about
//     something else — the subject bounds the candidates.
//
// The scan is structural: every string in the Result is walked, so a leak
// through a summary, a method line or a nested tick payload fails this test
// rather than shipping.
func TestDowntimeResultLeaksNothing(t *testing.T) {
	f := buildFixture(t)
	// Every seed is a different honest die; sweep a handful so the test
	// does not pass on a lucky roll.
	for seed := int64(0); seed < 6; seed++ {
		res := f.resolve(t, f.fx.Thalia, "I spend three weeks researching the cult", f.fx.Cult, 21, seed)
		forbidden := map[string]string{
			f.FactRiteKey:    "a secret with no source",
			f.FactHermit:     "a secret held only out of reach",
			f.FactProposed:   "a proposed fact",
			f.FactSuperseded: "a superseded fact",
			f.FactOffSubject: "a fact off the subject",
		}
		statements := forbiddenStatements(f)
		var leaks []string
		walkStrings(reflect.ValueOf(res), func(s string) {
			if reason, ok := forbidden[s]; ok {
				leaks = append(leaks, fmt.Sprintf("fact id %s (%s) appeared in the result", s, reason))
			}
			if why, ok := statements[s]; ok {
				leaks = append(leaks, fmt.Sprintf("statement %q (%s) appeared in the result", s, why))
			}
		})
		if len(leaks) > 0 {
			t.Fatalf("seed %d leaked:\n%s", seed, strings.Join(leaks, "\n"))
		}

		// Every finding that landed has a path: live, subject-touched, not
		// already held, and — when secret — carried by a reachable source.
		held := map[string]bool{f.FactWorship: true}
		for _, fd := range res.Findings {
			if forbidden[fd.FactID] != "" {
				t.Fatalf("seed %d: forbidden fact %s landed as a finding", seed, fd.FactID)
			}
			if held[fd.FactID] {
				t.Fatalf("seed %d: an already-held fact landed as a finding", seed)
			}
			if fd.Visibility == campaign.VisibilitySecret && len(fd.Sources) == 0 {
				t.Fatalf("seed %d: a secret landed with no reachable source", seed)
			}
		}

		// The one fact that should land, does — the test has teeth.
		landed := false
		for _, fd := range res.Findings {
			if fd.FactID == f.FactRecruits {
				landed = true
			}
		}
		if !landed {
			t.Fatalf("seed %d: the findable public fact never landed; the fixture has no teeth", seed)
		}
	}
}

// forbiddenStatements collects the planted off-limits statements, so the
// scan catches a leak through prose, not just ids.
func forbiddenStatements(f *downtimeFixture) map[string]string {
	return map[string]string{
		"The Cult's great rite requires the Silver Key.": "secret, no source",
		"The Cult is funded by the Vane family charter.": "secret, source out of reach",
		"The Cult plans a second rite at Greyfall.":      "proposed",
		"The Cult meets at the Waystone inn.":            "superseded",
		"The Duke is pale, precise, never seen eating.":  "off subject",
	}
}

// walkStrings visits every string in a value, recursing through pointers,
// interfaces, slices, arrays, maps and structs.
func walkStrings(v reflect.Value, fn func(string)) {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			walkStrings(v.Elem(), fn)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkStrings(v.Index(i), fn)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			walkStrings(iter.Key(), fn)
			walkStrings(iter.Value(), fn)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			walkStrings(v.Field(i), fn)
		}
	case reflect.String:
		fn(v.String())
	}
}

/* ---------- the store ---------- */

// graphCounts snapshots the row counts a request must not move.
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
		 WHERE r.from_entity IN (SELECT id FROM entities WHERE campaign_id = ?)`, campaignID).Scan(&rels); err != nil {
		t.Fatalf("count relationships: %v", err)
	}
	out["relationships"] = rels
	return out
}

// TestRequestWritesNothingButItsRow: a request is a question. Only the
// downtime_requests row appears — and an unmappable activity records not
// even that.
func TestRequestWritesNothingButItsRow(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	before := graphCounts(t, f.db, f.campaignID)

	row, res, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "research the cult", f.fx.Cult, 21, nil, "keeper")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	after := graphCounts(t, f.db, f.campaignID)
	for table, n := range before {
		if after[table] != n {
			t.Fatalf("request wrote to %s: %d -> %d", table, n, after[table])
		}
	}
	var rows int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM downtime_requests WHERE campaign_id = ?`, f.campaignID).Scan(&rows); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected exactly one downtime_requests row, got %d", rows)
	}
	if row.Status != RequestRecorded || row.FromDay != 0 || row.ToDay != 21 || row.Activity != ActivityResearch {
		t.Fatalf("row wrong: %+v", row)
	}
	if row.Seed != res.Seed || row.SnapshotDigest != res.Digest {
		t.Fatal("the row and the result disagree on identity")
	}

	// An unmappable activity is a clarifying question: nothing recorded.
	countBefore := rows
	if _, _, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "I meditate upon the void", "", 21, nil, "keeper"); err == nil {
		t.Fatal("an unmappable activity must refuse")
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM downtime_requests WHERE campaign_id = ?`, f.campaignID).Scan(&rows); err != nil || rows != countBefore {
		t.Fatalf("a refused activity recorded a row: %d (%v)", rows, err)
	}

	// A subject that does not exist is a 404-shaped refusal.
	if _, _, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "research", "ghost", 21, nil, "keeper"); err == nil {
		t.Fatal("a missing subject must refuse")
	}
}

// TestStageAndAcceptGrantsAwarenessToCharacterOnly is the acceptance
// criterion: the findings' awareness goes to the requesting character and to
// nobody else, the batch carries both halves of the answer, and accepting it
// moves the clock by exactly the window exactly once, reason 'downtime'.
func TestStageAndAcceptGrantsAwarenessToCharacterOnly(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	awareBefore := graphCounts(t, f.db, f.campaignID)["awareness"]

	// Who held what before the batch: sources hold their own rows, so the
	// assertion is about what the DECISION granted, not who ever knew.
	knowersBefore := map[string]map[string]bool{}
	collect := func() map[string]map[string]bool {
		out := map[string]map[string]bool{}
		rows, err := f.db.Query(`SELECT fact_id, knower FROM awareness WHERE campaign_id = ?`, f.campaignID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var fact, knower string
			if err := rows.Scan(&fact, &knower); err != nil {
				t.Fatal(err)
			}
			if out[fact] == nil {
				out[fact] = map[string]bool{}
			}
			out[fact][knower] = true
		}
		return out
	}
	knowersBefore = collect()

	row, res, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "research the cult", f.fx.Cult, 21, nil, "keeper")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatal("fixture lost its teeth: no findings to grant")
	}
	batch, _, err := f.store.Stage(ctx, f.campaignID, row.ID, "keeper")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if batch.Source != canon.BatchSourceDowntime {
		t.Fatalf("batch source %q", batch.Source)
	}
	kinds := map[string]int{}
	for _, it := range batch.Items {
		kinds[it.Kind]++
	}
	if kinds["proposed_event"] < 2 || kinds["proposed_fact"] < 1 || kinds["proposed_discovery"] < 1 ||
		kinds["proposed_plan_transition"] != 1 {
		t.Fatalf("batch composition wrong (want the downtime half and the tick half): %v", kinds)
	}

	decided, err := f.canon.DecideBatch(ctx, f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.Batch.Status != canon.BatchAccepted {
		t.Fatalf("batch status %s: %+v", decided.Batch.Status, decided.Items)
	}

	// Awareness: the decision granted each finding to the requesting
	// character and to nobody else — the sources' own rows predate it.
	knowersAfter := collect()
	for _, fd := range res.Findings {
		before, after := knowersBefore[fd.FactID], knowersAfter[fd.FactID]
		if !after[f.fx.Thalia] {
			t.Fatalf("finding %s was not granted to the requesting character", fd.FactID)
		}
		for k := range after {
			if before[k] {
				continue
			}
			if k != f.fx.Thalia {
				t.Fatalf("the decision granted finding %s to %s as well as %s", fd.FactID, k, f.fx.Thalia)
			}
		}
	}
	var awareAfter int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM awareness WHERE campaign_id = ?`, f.campaignID).Scan(&awareAfter); err != nil {
		t.Fatal(err)
	}
	if want := awareBefore + len(res.Findings); awareAfter != want {
		t.Fatalf("awareness rows %d, want %d (the findings and nothing else)", awareAfter, want)
	}

	// The character scope can now read what was found; nobody else can.
	thaliaFacts, err := f.knowledge.Facts(ctx, knowledge.ScopeCharacter(f.fx.Thalia), f.campaignID, knowledge.FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	has := func(list []campaign.Fact) map[string]bool {
		out := map[string]bool{}
		for _, x := range list {
			out[x.ID] = true
		}
		return out
	}
	thaliaHas := has(thaliaFacts)
	for _, fd := range res.Findings {
		if !thaliaHas[fd.FactID] {
			t.Fatalf("the character scope cannot read the finding %s it was granted", fd.FactID)
		}
	}
	branFacts, err := f.knowledge.Facts(ctx, knowledge.ScopeCharacter(f.fx.Bran), f.campaignID, knowledge.FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	branHas := has(branFacts)
	for _, fd := range res.Findings {
		if branHas[fd.FactID] {
			t.Fatal("another character's scope can read a finding granted to Thalia alone")
		}
	}
	partyFacts, err := f.knowledge.Facts(ctx, knowledge.ScopeParty, f.campaignID, knowledge.FactFilter{})
	if err != nil {
		t.Fatal(err)
	}
	partyHas := has(partyFacts)
	for _, fd := range res.Findings {
		if partyHas[fd.FactID] {
			t.Fatal("the party scope can read a finding granted to Thalia alone")
		}
	}

	// The clock moved exactly once, by exactly the window, reason downtime.
	var advances int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM clock_advances WHERE campaign_id = ? AND reason = 'downtime'`, f.campaignID,
	).Scan(&advances); err != nil {
		t.Fatal(err)
	}
	if advances != 1 {
		t.Fatalf("expected exactly one downtime advance, got %d", advances)
	}
	c, err := f.campaigns.GetCampaign(ctx, f.campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Clock != 21 {
		t.Fatalf("clock at %d, want 21", c.Clock)
	}

	// The world's half applied too: the plan completed.
	plan, err := f.factions.GetPlan(ctx, campaign.ScopeDM, f.campaignID, f.planID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != faction.PlanComplete {
		t.Fatalf("the plan did not apply: %s", plan.Status)
	}

	// The row landed on applied; a re-decision moves nothing.
	stored, err := f.store.requestInCampaign(ctx, row.ID, f.campaignID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != RequestApplied {
		t.Fatalf("request status %s, want applied", stored.Status)
	}
	if _, err := f.canon.DecideBatch(ctx, f.campaignID, batch.ID, canon.DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("re-decide: %v", err)
	}
	c2, _ := f.campaigns.GetCampaign(ctx, f.campaignID)
	if c2.Clock != 21 {
		t.Fatalf("a re-decision moved the clock to %d", c2.Clock)
	}
}

// TestDismissedDowntimeDiscardsTime: a dismissed batch writes no awareness
// and the character's time does not pass.
func TestDismissedDowntimeDiscardsTime(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	before := graphCounts(t, f.db, f.campaignID)
	row, _, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "research the cult", f.fx.Cult, 21, nil, "keeper")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	batch, _, err := f.store.Stage(ctx, f.campaignID, row.ID, "keeper")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := f.canon.DecideBatch(ctx, f.campaignID, batch.ID, canon.DecisionDismiss, nil, "keeper"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	c, _ := f.campaigns.GetCampaign(ctx, f.campaignID)
	if c.Clock != 0 {
		t.Fatalf("a dismissed downtime moved the clock to %d", c.Clock)
	}
	after := graphCounts(t, f.db, f.campaignID)
	for table, n := range before {
		if after[table] != n {
			t.Fatalf("a dismissed downtime wrote to %s: %d -> %d", table, n, after[table])
		}
	}
	stored, _ := f.store.requestInCampaign(ctx, row.ID, f.campaignID)
	if stored.Status != RequestDiscarded {
		t.Fatalf("request status %s, want discarded", stored.Status)
	}
}

// TestStageRefusesStaleAndMovedRequests: the digest makes a stale request
// detectable, a moved clock invalidates the window, and a request stages
// exactly once.
func TestStageRefusesStaleAndMovedRequests(t *testing.T) {
	f := buildFixture(t)
	ctx := context.Background()
	row, _, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "research the cult", f.fx.Cult, 21, nil, "keeper")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Change the world: a new entity shifts the snapshot digest.
	if _, err := f.campaigns.CreateEntity(ctx, f.campaignID, "npc", "A Stranger", "", nil); err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if _, _, err := f.store.Stage(ctx, f.campaignID, row.ID, "keeper"); err == nil {
		t.Fatal("staging a stale request must fail")
	} else if !strings.Contains(err.Error(), "changed since this request") {
		t.Fatalf("unexpected stale error: %v", err)
	}

	// A fresh request whose clock moves before staging is refused on the
	// window, not the digest.
	row2, _, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "research the cult", f.fx.Cult, 7, nil, "keeper")
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if _, _, err := f.campaigns.AdvanceClockBy(ctx, f.campaignID, 1, campaign.AdvanceManual, "a day passes", "", "keeper"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, _, err := f.store.Stage(ctx, f.campaignID, row2.ID, "keeper"); err == nil {
		t.Fatal("staging a request whose clock moved must fail")
	} else if !strings.Contains(err.Error(), "record a new downtime request") {
		t.Fatalf("unexpected moved-clock error: %v", err)
	}

	// A request stages exactly once.
	row3, _, err := f.store.Request(ctx, f.campaignID, f.fx.Thalia, "carouse", "", 3, nil, "keeper")
	if err != nil {
		t.Fatalf("request 3: %v", err)
	}
	if _, _, err := f.store.Stage(ctx, f.campaignID, row3.ID, "keeper"); err != nil {
		t.Fatalf("stage 3: %v", err)
	}
	if _, _, err := f.store.Stage(ctx, f.campaignID, row3.ID, "keeper"); err == nil {
		t.Fatal("staging a staged request must fail")
	}
}
