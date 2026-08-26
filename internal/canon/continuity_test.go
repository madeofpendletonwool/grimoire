package canon

// The pre-session continuity check (MAD-312): one unit test per deterministic
// rule, pure over hand-built snapshots, then the acceptance fixture over a
// real database — the campaign with three planted continuity errors, all
// caught by the deterministic pass alone — and the model residue pass against
// a fake client.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/* ---------- pure rule tests ---------- */

// prepGraph builds a graph with the entities the continuity tests need: a
// party, a dead npc, a live npc the party believes dead, an unheard faction,
// a location, and an item with a holder.
func prepGraph() *Snapshot {
	g := &campaign.Snapshot{
		CampaignID: "c1",
		Entities: []campaign.Entity{
			{ID: "thalia", CampaignID: "c1", Kind: campaign.KindPC, Name: "Thalia", Status: campaign.StatusActive},
			{ID: "vane", CampaignID: "c1", Kind: campaign.KindNPC, Name: "Lord Vane", Status: campaign.StatusDead},
			{ID: "elara", CampaignID: "c1", Kind: campaign.KindNPC, Name: "Lady Elara", Status: campaign.StatusActive},
			{ID: "ashen", CampaignID: "c1", Kind: campaign.KindFaction, Name: "The Ashen Court", Status: campaign.StatusActive},
			{ID: "vault", CampaignID: "c1", Kind: campaign.KindLocation, Name: "The Old Vault", Status: campaign.StatusActive},
			{ID: "key", CampaignID: "c1", Kind: campaign.KindItem, Name: "The Silver Key", Status: campaign.StatusActive},
		},
		ProvenanceCount: map[string]int{},
		CoveredFacts:    map[string]bool{},
	}
	s := &Snapshot{Snapshot: g, Bestiary: map[string]bool{}, IntroducedSession: map[string]string{}}
	s.Sessions = []SessionRef{{ID: "s1", Ordinal: 1, Status: "done", CreatedAt: t0}}
	mk := func(id, subject, predicate, objectEntity, objectLiteral, statement string) campaign.Fact {
		f := campaign.Fact{
			ID: id, CampaignID: "c1", SubjectEntity: subject, Predicate: predicate,
			ObjectEntity: objectEntity, ObjectLiteral: objectLiteral, Statement: statement,
			Confidence: campaign.ConfidenceCanon, Visibility: campaign.VisibilityPublic,
		}
		g.Facts = append(g.Facts, f)
		g.ProvenanceCount[id] = 1
		return f
	}
	mk("f-elara-dead", "elara", "died", "", "at the falls", "Lady Elara died at the falls.")
	mk("f-key-held", "key", "held_by", "thalia", "", "Thalia carries the Silver Key.")
	mk("f-ashen-secret", "ashen", "operates_from", "vault", "", "The Ashen Court operates out of the Old Vault.")
	g.Facts[len(g.Facts)-1].Visibility = campaign.VisibilitySecret
	return s
}

func TestPrepDeadOnStage(t *testing.T) {
	build := func(castRef, state string) *Prep {
		return &Prep{Scenes: []PrepScene{{
			Name: "The Dinner", Cast: []PrepCast{{Ref: castRef, State: state}},
		}}}
	}

	// Canon says dead (entity status): the scene contradicts the graph.
	s := prepGraph()
	findings, _ := CheckPrep(s, build("vane", PrepAlive))
	if n := has(findings, CheckPrepDeadOnStage, "The Dinner/0"); n != 1 {
		t.Fatalf("dead npc on stage: got %d findings, want 1", n)
	}
	if findings[0].Severity != campaign.SeverityError {
		t.Fatalf("status conflict must be error severity, got %s", findings[0].Severity)
	}

	// A dead-state cast member (the scene shows a body) conflicts with nothing.
	findings, _ = CheckPrep(s, build("vane", PrepDead))
	if n := has(findings, CheckPrepDeadOnStage, "The Dinner/0"); n != 0 {
		t.Fatalf("corpse scene: got %d findings, want 0", n)
	}

	// The belief tier: canon says alive, but the party believes them dead —
	// review, not error, because the reveal may be the point.
	s2 := prepGraph()
	s2.Awareness = []AwarenessRow{
		{Knower: campaign.PartyKnower, FactID: "f-elara-dead", Stance: "knows", CreatedAt: t1, UpdatedAt: t1},
	}
	findings, _ = CheckPrep(s2, build("elara", PrepAlive))
	if n := has(findings, CheckPrepDeadOnStage, "The Dinner/0"); n != 1 {
		t.Fatalf("believed-dead npc on stage: got %d findings, want 1", n)
	}
	if findings[0].Severity != campaign.SeverityReview {
		t.Fatalf("belief conflict must be review severity, got %s", findings[0].Severity)
	}
	if !strings.Contains(findings[0].Message, "twist") {
		t.Fatalf("the message should frame the twist question: %q", findings[0].Message)
	}

	// An NPC nobody has beliefs about walks in freely.
	findings, _ = CheckPrep(prepGraph(), build("elara", PrepAlive))
	if n := has(findings, CheckPrepDeadOnStage, "The Dinner/0"); n != 0 {
		t.Fatalf("live npc on stage: got %d findings, want 0", n)
	}

	// An npc-only belief does not speak for the party.
	s3 := prepGraph()
	s3.Awareness = []AwarenessRow{
		{Knower: "vane", FactID: "f-elara-dead", Stance: "knows", CreatedAt: t1, UpdatedAt: t1},
	}
	findings, _ = CheckPrep(s3, build("elara", PrepAlive))
	if n := has(findings, CheckPrepDeadOnStage, "The Dinner/0"); n != 0 {
		t.Fatalf("npc belief: got %d findings, want 0", n)
	}
}

func TestPrepUnheardName(t *testing.T) {
	build := func(withSessions bool, cast []PrepCast, location string) *Snapshot {
		s := prepGraph()
		if !withSessions {
			s.Sessions = nil
		}
		return s
	}
	prep := &Prep{Scenes: []PrepScene{{
		Name: "The summons", Cast: []PrepCast{{Ref: "The Ashen Court", State: PrepMentioned}},
	}}}

	// The faction exists, the party has never heard a word about it.
	findings, _ := CheckPrep(build(true, prep.Scenes[0].Cast, ""), prep)
	if n := has(findings, CheckPrepUnheardName, "The summons/theashencourt"); n != 1 {
		t.Fatalf("unheard faction: got %d findings, want 1", n)
	}

	// The party heard about it: one granting awareness row on a touching
	// fact, and the name is known.
	s := prepGraph()
	s.Awareness = []AwarenessRow{
		{Knower: campaign.PartyKnower, FactID: "f-ashen-secret", Stance: "suspects", CreatedAt: t1, UpdatedAt: t1},
	}
	findings, _ = CheckPrep(s, prep)
	if n := countCheck(findings, CheckPrepUnheardName); n != 0 {
		t.Fatalf("heard faction: got %d findings, want 0", n)
	}

	// Before any session exists nothing is "unheard" — no table history.
	findings, _ = CheckPrep(build(false, prep.Scenes[0].Cast, ""), prep)
	if n := countCheck(findings, CheckPrepUnheardName); n != 0 {
		t.Fatalf("no sessions: got %d findings, want 0", n)
	}

	// The party's own members are never unheard.
	roster := &Prep{Scenes: []PrepScene{{
		Name: "Camp", Cast: []PrepCast{{Ref: "Thalia", State: PrepAlive}},
	}}}
	findings, _ = CheckPrep(prepGraph(), roster)
	if n := countCheck(findings, CheckPrepUnheardName); n != 0 {
		t.Fatalf("pc on stage: got %d findings, want 0", n)
	}
}

func TestPrepItemMisplaced(t *testing.T) {
	prep := &Prep{Scenes: []PrepScene{{
		Name:  "The vault run",
		Items: []PrepItem{{Ref: "The Silver Key", AssumedAt: "The Old Vault"}},
	}}}

	// The party holds the key; the prep leaves it in the vault.
	s := prepGraph()
	s.Awareness = []AwarenessRow{
		{Knower: campaign.PartyKnower, FactID: "f-key-held", Stance: "knows", CreatedAt: t1, UpdatedAt: t1},
	}
	findings, _ := CheckPrep(s, prep)
	if n := has(findings, CheckPrepItemMisplaced, "The vault run/0"); n != 1 {
		t.Fatalf("misplaced key: got %d findings, want 1", n)
	}
	if findings[0].Severity != campaign.SeverityError {
		t.Fatalf("misplaced item must be error severity, got %s", findings[0].Severity)
	}
	if !strings.Contains(findings[0].Message, "Thalia") {
		t.Fatalf("the message should say who holds it: %q", findings[0].Message)
	}

	// The assumption matching the holder is fine.
	ok := &Prep{Scenes: []PrepScene{{
		Name:  "The handoff",
		Items: []PrepItem{{Ref: "The Silver Key", AssumedAt: "Thalia"}},
	}}}
	findings, _ = CheckPrep(s, ok)
	if n := has(findings, CheckPrepItemMisplaced, "The handoff/0"); n != 0 {
		t.Fatalf("key with its holder: got %d findings, want 0", n)
	}

	// Without party awareness of the holding fact, the join stays silent —
	// the DM may know things the table has not registered yet.
	findings, _ = CheckPrep(prepGraph(), prep)
	if n := has(findings, CheckPrepItemMisplaced, "The vault run/0"); n != 0 {
		t.Fatalf("unknown holding: got %d findings, want 0", n)
	}

	// A free-text assumption ("the vault" lowercase, resolves nowhere) is
	// the model's residue, never the join's guess.
	loose := &Prep{Scenes: []PrepScene{{
		Name:  "The vault run",
		Items: []PrepItem{{Ref: "The Silver Key", AssumedAt: "a locked vault somewhere"}},
	}}}
	findings, _ = CheckPrep(s, loose)
	if n := has(findings, CheckPrepItemMisplaced, "The vault run/0"); n != 0 {
		t.Fatalf("unresolvable assumption: got %d findings, want 0", n)
	}
}

func TestPrepNameResolution(t *testing.T) {
	s := prepGraph()
	s.Names = []campaign.EntityName{{ID: "n1", EntityID: "vane", Name: "Thomas Vane", Kind: campaign.NameAlias}}
	prep := &Prep{Scenes: []PrepScene{{
		Name: "The return", Cast: []PrepCast{{Ref: "Thomas Vane", State: PrepAlive}},
	}}}
	findings, names := CheckPrep(s, prep)
	if len(names) != 1 || !names[0].Resolved || names[0].EntityID != "vane" {
		t.Fatalf("alias resolution: %+v", names)
	}
	if n := has(findings, CheckPrepDeadOnStage, "The return/0"); n != 1 {
		t.Fatalf("the dead npc must resolve through the alias: got %d findings", n)
	}

	// An unresolvable ref is new content, not a conflict — but it is
	// reported in the names table so the DM can see what the checker read.
	prep = &Prep{Scenes: []PrepScene{{
		Name: "The stranger", Cast: []PrepCast{{Ref: "A Completely New Person", State: PrepAlive}},
	}}}
	findings, names = CheckPrep(s, prep)
	if len(names) != 1 || names[0].Resolved {
		t.Fatalf("new content must resolve nowhere: %+v", names)
	}
	if len(findings) != 0 {
		t.Fatalf("new content must not conflict: %+v", findings)
	}
}

func countCheck(findings []campaign.Finding, check string) int {
	n := 0
	for _, f := range findings {
		if f.Check == check {
			n++
		}
	}
	return n
}

/* ---------- the acceptance fixture: three planted errors, offline ---------- */

// plantedContinuity builds the fixture campaign the issue's acceptance asks
// for: the seed campaign plus three planted continuity errors —
//
//  1. the party believes Lord Vane dead (canon status dead, party knows the
//     death fact), but the prep has him walk in alive;
//  2. the prep references "the Ashen Court", a faction no character has
//     ever heard a fact about;
//  3. the dungeon assumes the Silver Key sits in Greyfall Monastery, but
//     Thalia carries it and the party knows.
func plantedContinuity(t *testing.T) (*Store, *campaign.Fixture, string, error) {
	t.Helper()
	db, fx, sess1 := seeded(t)
	ctx := context.Background()

	// Error 1: the Duke is canon-dead and the party knows it.
	if _, err := db.Exec(`UPDATE entities SET status = 'dead' WHERE id = ?`, fx.Duke); err != nil {
		t.Fatalf("plant dead duke: %v", err)
	}
	campaigns, err := campaign.New(db)
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	deathFact, err := campaigns.CreateFact(ctx, fx.Campaign.ID, fx.Duke, "died", "", "at the falls",
		"Duke Aldric Vane died at the falls.", campaign.ConfidenceCanon, campaign.VisibilityPublic,
		"keeper", []campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, SessionID: sess1}})
	if err != nil {
		t.Fatalf("plant death fact: %v", err)
	}
	plantAwareness(t, db, fx.Campaign.ID, campaign.PartyKnower, deathFact.ID, "knows")

	// Error 2: a faction nobody has heard of.
	ashen, err := campaigns.CreateEntity(ctx, fx.Campaign.ID, campaign.KindFaction, "The Ashen Court",
		"A cabal nobody has ever mentioned at the table.", nil)
	if err != nil {
		t.Fatalf("plant ashen court: %v", err)
	}
	if _, err := campaigns.CreateFact(ctx, fx.Campaign.ID, ashen.ID, "operates_from", fx.Monastery, "",
		"The Ashen Court operates out of Greyfall Monastery.", campaign.ConfidenceCanon, campaign.VisibilitySecret,
		"keeper", []campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, SessionID: sess1}}); err != nil {
		t.Fatalf("plant ashen fact: %v", err)
	}

	// Error 3: Thalia carries the key, and the party knows.
	keyFact, err := campaigns.CreateFact(ctx, fx.Campaign.ID, fx.Key, "held_by", fx.Thalia, "",
		"Thalia carries the Silver Key.", campaign.ConfidenceCanon, campaign.VisibilityPublic,
		"keeper", []campaign.ProvenanceInput{{Method: campaign.MethodDMAuthored, SessionID: sess1}})
	if err != nil {
		t.Fatalf("plant holding fact: %v", err)
	}
	plantAwareness(t, db, fx.Campaign.ID, campaign.PartyKnower, keyFact.ID, "knows")

	s, err := NewOffline(db)
	if err != nil {
		t.Fatalf("offline store: %v", err)
	}
	return s, fx, sess1, nil
}

// plantAwareness writes one awareness row directly, the way flags_test does:
// the canon tests need the row, not the API path.
func plantAwareness(t *testing.T, db *sql.DB, campaignID, knower, factID, stance string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`
		INSERT INTO awareness (campaign_id, knower, fact_id, stance, confidence, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)`, campaignID, knower, factID, stance, now, now); err != nil {
		t.Fatalf("plant awareness %s/%s: %v", knower, factID, err)
	}
}

// plantedPrep is the prep document with the three planted errors in it.
func plantedPrep(fx *campaign.Fixture) *Prep {
	return &Prep{
		Title: "Session 2: The Waystone Dinner",
		Scenes: []PrepScene{{
			Name:    "The dinner",
			Summary: "Lord Vane walks into the Waystone alive and orders a drink.",
			Cast:    []PrepCast{{Ref: "Duke Aldric Vane", State: PrepAlive}},
		}, {
			Name:     "The summons",
			Summary:  "A messenger speaks of the Ashen Court's dues.",
			Location: "Blackwater",
			Cast:     []PrepCast{{Ref: "The Ashen Court", State: PrepMentioned}},
		}, {
			Name:    "The crypt run",
			Summary: "The dungeon assumes the Silver Key still sits in the crypt beneath the monastery.",
			Items:   []PrepItem{{Ref: "The Silver Key", AssumedAt: "Greyfall Monastery"}},
		}},
	}
}

// TestContinuityFixtureCaughtOffline is the issue's first acceptance
// criterion: a fixture campaign with three planted continuity errors, all
// three caught by the deterministic pass alone, on an offline store.
func TestContinuityFixtureCaughtOffline(t *testing.T) {
	s, fx, _, err := plantedContinuity(t)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.CheckContinuity(context.Background(), fx.Campaign.ID, plantedPrep(fx))
	if err != nil {
		t.Fatalf("check continuity: %v", err)
	}
	if !rep.Offline {
		t.Fatal("an offline store must report Offline=true")
	}
	if len(rep.ModelFindings) != 0 {
		t.Fatalf("an offline store must not produce model findings: %+v", rep.ModelFindings)
	}
	for _, check := range []string{CheckPrepDeadOnStage, CheckPrepUnheardName, CheckPrepItemMisplaced} {
		if countCheck(rep.Findings, check) == 0 {
			t.Fatalf("planted error %s not caught; findings: %+v", check, rep.Findings)
		}
	}
	// The messages must speak in campaign terms, not ids.
	for _, f := range rep.Findings {
		if f.Check == CheckPrepDeadOnStage && !strings.Contains(f.Message, "Duke Aldric Vane") {
			t.Fatalf("dead-on-stage message must name the npc: %q", f.Message)
		}
		if f.Check == CheckPrepItemMisplaced && !strings.Contains(f.Message, "Silver Key") {
			t.Fatalf("misplaced-item message must name the item: %q", f.Message)
		}
	}
}

/* ---------- the model residue pass ---------- */

func TestContinuityResidueWithFakeModel(t *testing.T) {
	s, fx, _, err := plantedContinuity(t)
	if err != nil {
		t.Fatal(err)
	}
	prep := plantedPrep(fx)

	// A scripted conflict whose quotes are verbatim on both sides.
	response := `{"conflicts":[{"scene":"The dinner","quote":"Lord Vane walks into the Waystone alive and orders a drink.","conflict":"The party watched the Duke die; the scene plays it as unremarkable.","evidence":"Duke Aldric Vane died at the falls."}]}`
	store := newStore(t, s.db, &fakeModel{responses: []string{response}}, testConfig())
	rep, err := store.CheckContinuity(context.Background(), fx.Campaign.ID, prep)
	if err != nil {
		t.Fatalf("check continuity: %v", err)
	}
	if rep.Offline {
		t.Fatal("a model-wired store must report Offline=false")
	}
	if n := countCheck(rep.ModelFindings, CheckPrepModelConflict); n != 1 {
		t.Fatalf("residue finding: got %d, want 1 (problems: %v)", n, rep.Problems)
	}
	if rep.InputTokens == 0 || rep.OutputTokens == 0 {
		t.Fatalf("token accounting missing: %+v", rep)
	}

	// A fabricated quote is dropped and logged, never surfaced as a finding.
	bad := `{"conflicts":[{"scene":"The dinner","quote":"words that appear nowhere in the prep","conflict":"invented"}]}`
	store = newStore(t, s.db, &fakeModel{responses: []string{bad}}, testConfig())
	rep, err = store.CheckContinuity(context.Background(), fx.Campaign.ID, prep)
	if err != nil {
		t.Fatal(err)
	}
	if n := countCheck(rep.ModelFindings, CheckPrepModelConflict); n != 0 {
		t.Fatalf("fabricated quote must be dropped: %+v", rep.ModelFindings)
	}
	if len(rep.Problems) == 0 {
		t.Fatal("the drop must be logged as a problem")
	}
}

func TestContinuityEmptyPrep(t *testing.T) {
	s, fx, _, err := plantedContinuity(t)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.CheckContinuity(context.Background(), fx.Campaign.ID, &Prep{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("empty prep must conflict with nothing: %+v", rep.Findings)
	}
}
