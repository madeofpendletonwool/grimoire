package canon

// The campaign-health surfaces (MAD-312): one unit test per new deterministic
// rule, pure over hand-built snapshots, then the assembled report over a
// seeded campaign — offline, and with a fake model proving the narrative pass
// summarizes without discovering.

import (
	"context"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/* ---------- dormant_clue ---------- */

func TestDormantClue(t *testing.T) {
	build := func(age int64, laterDiscovery, covered, public bool) *Snapshot {
		g := baseGraph()
		f := canonFact(g, "f1", campaign.VisibilitySecret)
		if public {
			f.Visibility = campaign.VisibilityPublic
			g.Facts[0] = f
		}
		s := pureSnap(g)
		for i := int64(1); i <= age+1; i++ {
			s.Sessions = append(s.Sessions, SessionRef{ID: sid(i), Ordinal: i, Status: "done", CreatedAt: t0})
		}
		s.Discoveries = []DiscoveryRow{
			{ID: "d1", FactID: "f1", DiscoveredBy: campaign.PartyKnower, SessionID: sid(1), CreatedAt: t0},
		}
		s.Awareness = []AwarenessRow{
			{Knower: campaign.PartyKnower, FactID: "f1", Stance: "knows", DiscoveryID: "d1", CreatedAt: t0, UpdatedAt: t0},
		}
		if laterDiscovery {
			s.Discoveries = append(s.Discoveries,
				DiscoveryRow{ID: "d2", FactID: "f1", DiscoveredBy: "duke", SessionID: sid(2), CreatedAt: t1})
		}
		if covered {
			s.CoveredFacts["f1"] = true
		}
		return s
	}

	// Learned four sessions ago, nothing since: the clue went nowhere.
	if n := has(CheckSnapshot(build(4, false, false, false), DefaultCheckOptions()), CheckDormantClue, "f1"); n != 1 {
		t.Fatalf("dormant clue: got %d findings, want 1", n)
	}
	// Two sessions old: within the threshold.
	if n := has(CheckSnapshot(build(2, false, false, false), DefaultCheckOptions()), CheckDormantClue, "f1"); n != 0 {
		t.Fatalf("young clue: got %d findings, want 0", n)
	}
	// A later discovery on the same thread is development.
	if n := has(CheckSnapshot(build(4, true, false, false), DefaultCheckOptions()), CheckDormantClue, "f1"); n != 0 {
		t.Fatalf("developed clue: got %d findings, want 0", n)
	}
	// An open contradiction is development.
	if n := has(CheckSnapshot(build(4, false, true, false), DefaultCheckOptions()), CheckDormantClue, "f1"); n != 0 {
		t.Fatalf("contradicted clue: got %d findings, want 0", n)
	}
	// Public facts are lore, not clues.
	if n := has(CheckSnapshot(build(4, false, false, true), DefaultCheckOptions()), CheckDormantClue, "f1"); n != 0 {
		t.Fatalf("public fact: got %d findings, want 0", n)
	}
	// A secret the party never learned is unreachable_secret's problem.
	g := baseGraph()
	canonFact(g, "f2", campaign.VisibilitySecret)
	s := pureSnap(g)
	for i := int64(1); i <= 5; i++ {
		s.Sessions = append(s.Sessions, SessionRef{ID: sid(i), Ordinal: i, Status: "done", CreatedAt: t0})
	}
	if n := has(CheckSnapshot(s, DefaultCheckOptions()), CheckDormantClue, "f2"); n != 0 {
		t.Fatalf("unlearned secret: got %d findings, want 0", n)
	}
}

/* ---------- unused_npc and dormant_region ---------- */

func TestUnusedEntities(t *testing.T) {
	build := func(withSessions bool) *Snapshot {
		g := &campaign.Snapshot{
			CampaignID: "c1",
			Entities: []campaign.Entity{
				{ID: "thalia", CampaignID: "c1", Kind: campaign.KindPC, Name: "Thalia", Status: campaign.StatusActive},
				{ID: "used-npc", CampaignID: "c1", Kind: campaign.KindNPC, Name: "Used", Status: campaign.StatusActive},
				{ID: "spare-npc", CampaignID: "c1", Kind: campaign.KindNPC, Name: "Spare", Status: campaign.StatusActive},
				{ID: "dead-npc", CampaignID: "c1", Kind: campaign.KindNPC, Name: "Dead", Status: campaign.StatusDead},
				{ID: "live-loc", CampaignID: "c1", Kind: campaign.KindLocation, Name: "Live", Status: campaign.StatusActive},
				{ID: "dead-loc", CampaignID: "c1", Kind: campaign.KindLocation, Name: "Stalled", Status: campaign.StatusActive},
			},
			Facts: []campaign.Fact{{
				ID: "f1", CampaignID: "c1", SubjectEntity: "used-npc", Predicate: "knows_of",
				ObjectLiteral: "something", Statement: "the used npc appears in a fact",
				Confidence: campaign.ConfidenceCanon, Visibility: campaign.VisibilityPublic,
			}},
			Relationships:   []campaign.Relationship{{ID: "r1", FromEntity: "live-loc", RelType: "located_in", ToEntity: "used-npc"}},
			ProvenanceCount: map[string]int{"f1": 1},
			CoveredFacts:    map[string]bool{},
		}
		s := &Snapshot{Snapshot: g, Bestiary: map[string]bool{}, IntroducedSession: map[string]string{}}
		if withSessions {
			s.Sessions = []SessionRef{{ID: "s1", Ordinal: 1, Status: "done", CreatedAt: t0}}
		}
		return s
	}

	findings := CheckSnapshot(build(true), DefaultCheckOptions())
	if n := has(findings, CheckUnusedNPC, "spare-npc"); n != 1 {
		t.Fatalf("unused npc: got %d findings, want 1", n)
	}
	if n := has(findings, CheckUnusedNPC, "used-npc"); n != 0 {
		t.Fatalf("used npc flagged: got %d findings, want 0", n)
	}
	if n := has(findings, CheckUnusedNPC, "dead-npc"); n != 0 {
		t.Fatalf("dead npc flagged: got %d findings, want 0", n)
	}
	if n := has(findings, CheckDormantRegion, "dead-loc"); n != 1 {
		t.Fatalf("stalled region: got %d findings, want 1", n)
	}
	if n := has(findings, CheckDormantRegion, "live-loc"); n != 0 {
		t.Fatalf("live region flagged: got %d findings, want 0", n)
	}
	// pcs are never unused.
	if n := has(findings, CheckUnusedNPC, "thalia"); n != 0 {
		t.Fatalf("pc flagged: got %d findings, want 0", n)
	}
	// No sessions: the checks stay silent — nothing has had a chance to be used.
	findings = CheckSnapshot(build(false), DefaultCheckOptions())
	if n := countCheck(findings, CheckUnusedNPC) + countCheck(findings, CheckDormantRegion); n != 0 {
		t.Fatalf("sessionless campaign flagged: %v", findings)
	}
}

/* ---------- unfounded_relationship ---------- */

func TestUnfoundedRelationship(t *testing.T) {
	build := func(justifiedBy string, superseded bool) *Snapshot {
		g := baseGraph()
		g.Entities = append(g.Entities,
			campaign.Entity{ID: "cult", CampaignID: "c1", Kind: campaign.KindFaction, Name: "Cult", Status: campaign.StatusActive})
		canonFact(g, "f1", campaign.VisibilityPublic)
		if superseded {
			g.Facts[0].SupersededBy = "f2"
		}
		g.Relationships = []campaign.Relationship{{
			ID: "r1", FromEntity: "duke", RelType: "secretly_controls", ToEntity: "cult",
			JustifiedByFact: justifiedBy,
		}}
		return pureSnap(g)
	}

	// Grounded on a live fact: fine.
	if n := has(CheckSnapshot(build("f1", false), DefaultCheckOptions()), CheckUnfoundedRelationship, "r1"); n != 0 {
		t.Fatalf("grounded relationship flagged: got %d, want 0", n)
	}
	// No fact at all: unfounded.
	findings := CheckSnapshot(build("", false), DefaultCheckOptions())
	if n := has(findings, CheckUnfoundedRelationship, "r1"); n != 1 {
		t.Fatalf("unfounded relationship: got %d, want 1", n)
	}
	if !strings.Contains(findings[0].Message, "secretly_controls") {
		t.Fatalf("the message should carry the rel type: %q", findings[0].Message)
	}
	// Grounded on a fact that has since been superseded: equally unfounded.
	if n := has(CheckSnapshot(build("f1", true), DefaultCheckOptions()), CheckUnfoundedRelationship, "r1"); n != 1 {
		t.Fatalf("superseded grounding: got %d, want 1", n)
	}
}

/* ---------- the assembled report ---------- */

func TestHealthReportOffline(t *testing.T) {
	s, fx, _ := offlineStore(t)
	ctx := context.Background()

	// Session history: the seed's one done session plus two more, with
	// events so the pacing block has something to count.
	db := s.db
	for _, spec := range []struct {
		id, name string
		ordinal  int64
	}{
		{"sess-2", "Session 2", 2}, {"sess-3", "Session 3", 3},
	} {
		if _, err := db.Exec(`INSERT INTO game_sessions (id, campaign_id, ordinal, name, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'done', 1, 1)`, spec.id, fx.Campaign.ID, spec.ordinal, spec.name); err != nil {
			t.Fatalf("session %s: %v", spec.id, err)
		}
	}
	for _, ev := range []struct {
		session, id, kind string
		seq               int64
	}{
		{"sess-2", "e1", "encounter", 1}, {"sess-2", "e2", "encounter", 2}, {"sess-2", "e3", "qa", 3},
		{"sess-3", "e4", "discovery", 1}, {"sess-3", "e5", "ruling", 2},
	} {
		if _, err := db.Exec(`INSERT INTO session_events (id, session_id, seq, kind, summary, detail, payload, created_at)
			VALUES (?, ?, ?, ?, 's', '', '{}', 1)`, ev.id, ev.session, ev.seq, ev.kind); err != nil {
			t.Fatalf("event %s: %v", ev.id, err)
		}
	}

	rep, err := s.HealthReport(ctx, fx.Campaign.ID, DefaultHealthOptions())
	if err != nil {
		t.Fatalf("health report: %v", err)
	}
	if !rep.Offline {
		t.Fatal("an offline store must report Offline=true")
	}
	if rep.Narrative != "" {
		t.Fatal("an offline store must not produce a narrative")
	}
	if rep.CampaignName != "The Withering Kingdom" {
		t.Fatalf("campaign name: %q", rep.CampaignName)
	}
	// The seed's Silver Key secret has no awareness row at all: it is an
	// unreachable thread in the report.
	var sawUnreachable bool
	for _, th := range rep.Threads {
		if th.Kind == CheckUnreachableSecret && th.FactID == fx.FactKeyOpensCrypt {
			sawUnreachable = true
			if th.Statement == "" {
				t.Fatal("threads carry the fact statement")
			}
		}
	}
	if !sawUnreachable {
		t.Fatalf("the seed's unreachable secret must appear in the report: %+v", rep.Threads)
	}
	// The seed plants an npc nothing references (Brother Venn) and
	// relationships with no justifying fact.
	var sawNPC, sawRel bool
	for _, n := range rep.UnusedNPCs {
		if strings.Contains(n.Name, "Venn") {
			sawNPC = true
		}
	}
	for _, r := range rep.Unresolved {
		if r.RelType == "located_in" {
			sawRel = true
		}
	}
	if !sawNPC {
		t.Fatalf("the seed's unused npc must appear: %+v", rep.UnusedNPCs)
	}
	if !sawRel {
		t.Fatalf("the seed's unfounded relationships must appear: %+v", rep.Unresolved)
	}
	// Pacing: three done sessions, newest last, with the planted mix.
	if len(rep.Pacing) != 3 {
		t.Fatalf("pacing sessions: %d, want 3 (%+v)", len(rep.Pacing), rep.Pacing)
	}
	last := rep.Pacing[len(rep.Pacing)-1]
	if last.Ordinal != 3 || last.Discoveries != 1 || last.Rulings != 1 {
		t.Fatalf("session 3 pacing: %+v", last)
	}
	mid := rep.Pacing[1]
	if mid.Encounters != 2 || mid.QA != 1 {
		t.Fatalf("session 2 pacing: %+v", mid)
	}
	// The ledger was refreshed on the way in.
	if rep.OpenFlagCount == 0 {
		t.Fatal("the report must count the open flags its own findings opened")
	}
	flags, err := s.Flags(ctx, fx.Campaign.ID, FlagOpen)
	if err != nil {
		t.Fatal(err)
	}
	if len(flags) != rep.OpenFlagCount {
		t.Fatalf("open flag count %d disagrees with the ledger %d", rep.OpenFlagCount, len(flags))
	}
}

func TestHealthReportNarrativeWithFakeModel(t *testing.T) {
	db, fx, _ := seeded(t)
	store := newStore(t, db, &fakeModel{responses: []string{
		"Two threads have gone quiet: the Silver Key and the crypt beneath Greyfall. Consider a lead toward one of them.",
	}}, testConfig())

	rep, err := store.HealthReport(context.Background(), fx.Campaign.ID, DefaultHealthOptions())
	if err != nil {
		t.Fatalf("health report: %v", err)
	}
	if rep.Offline {
		t.Fatal("a model-wired store must report Offline=false")
	}
	if !strings.Contains(rep.Narrative, "Silver Key") {
		t.Fatalf("narrative must be the model's prose verbatim: %q", rep.Narrative)
	}

	// The model summarizes; it does not discover: the deterministic
	// sections are identical to the offline store's report on the same
	// campaign, whatever the model said.
	offline, err := NewOffline(db)
	if err != nil {
		t.Fatal(err)
	}
	base, err := offline.HealthReport(context.Background(), fx.Campaign.ID, DefaultHealthOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Threads) != len(base.Threads) || len(rep.UnusedNPCs) != len(base.UnusedNPCs) ||
		len(rep.Unresolved) != len(base.Unresolved) || len(rep.Clues) != len(base.Clues) ||
		len(rep.DormantRegions) != len(base.DormantRegions) {
		t.Fatalf("the narrative pass must not change the deterministic sections: %+v vs %+v", rep, base)
	}
}
