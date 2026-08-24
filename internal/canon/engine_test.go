package canon

// The deterministic rules, one unit test each, pure over hand-built snapshots:
// no DB, no clock, no network. This is the highest-value test file in the
// Stage 3 plan — an untested consistency rule is worse than no rule, because
// it is trusted.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
)

/* ---------- snapshot builders ---------- */

var (
	t0 = time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 1, 8, 18, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 1, 15, 18, 0, 0, 0, time.UTC)
)

// pureSnap builds the minimal engine snapshot around one graph snapshot: no
// knowledge, no sessions, no encounters, empty bestiary.
func pureSnap(graph *campaign.Snapshot) *Snapshot {
	return &Snapshot{
		Snapshot:          graph,
		Bestiary:          map[string]bool{},
		IntroducedSession: map[string]string{},
	}
}

// baseGraph builds a graph with the two entities the epistemic tests need —
// a pc and an npc — and no facts, so each test adds exactly the rows its rule
// reads.
func baseGraph() *campaign.Snapshot {
	return &campaign.Snapshot{
		CampaignID: "c1",
		Entities: []campaign.Entity{
			{ID: "thalia", CampaignID: "c1", Kind: campaign.KindPC, Name: "Thalia", Status: campaign.StatusActive},
			{ID: "duke", CampaignID: "c1", Kind: campaign.KindNPC, Name: "Duke", Status: campaign.StatusActive},
		},
		ProvenanceCount: map[string]int{},
		CoveredFacts:    map[string]bool{},
	}
}

// canonFact appends a live, canon, provenance-carrying fact to the graph.
func canonFact(g *campaign.Snapshot, id, visibility string) campaign.Fact {
	f := campaign.Fact{
		ID: id, CampaignID: g.CampaignID, SubjectEntity: "duke", Predicate: "knows_of",
		ObjectLiteral: "something", Statement: "fact " + id,
		Confidence: campaign.ConfidenceCanon, Visibility: visibility,
	}
	g.Facts = append(g.Facts, f)
	g.ProvenanceCount[id] = 1
	return f
}

// has reports whether the findings contain the given check on the given
// record, and how many times.
func has(findings []campaign.Finding, check, recordID string) int {
	n := 0
	for _, f := range findings {
		if f.Check == check && f.RecordID == recordID {
			n++
		}
	}
	return n
}

/* ---------- spoiler_leak ---------- */

func TestSpoilerLeak(t *testing.T) {
	build := func(partyStance, pcStance, npcStance string) *Snapshot {
		g := baseGraph()
		canonFact(g, "f1", campaign.VisibilityPublic)
		s := pureSnap(g)
		if partyStance != "" {
			s.Awareness = append(s.Awareness, AwarenessRow{Knower: campaign.PartyKnower, FactID: "f1", Stance: partyStance, CreatedAt: t1, UpdatedAt: t1})
		}
		if pcStance != "" {
			s.Awareness = append(s.Awareness, AwarenessRow{Knower: "thalia", FactID: "f1", Stance: pcStance, CreatedAt: t1, UpdatedAt: t1})
		}
		if npcStance != "" {
			s.Awareness = append(s.Awareness, AwarenessRow{Knower: "duke", FactID: "f1", Stance: npcStance, CreatedAt: t1, UpdatedAt: t1})
		}
		return s
	}

	// The leak: the party row explicitly denies knowledge while a pc's
	// surface renders the fact.
	if n := has(CheckSnapshot(build("unaware", "knows", ""), DefaultCheckOptions()), CheckSpoilerLeak, "f1"); n != 1 {
		t.Fatalf("party unaware + pc knows: got %d spoiler_leak findings, want 1", n)
	}
	// An NPC's knowledge is DM-side simulation, never a player surface.
	if n := has(CheckSnapshot(build("unaware", "", "knows"), DefaultCheckOptions()), CheckSpoilerLeak, "f1"); n != 0 {
		t.Fatalf("party unaware + npc knows: got %d spoiler_leak findings, want 0", n)
	}
	// A pc's solo knowledge with no party row at all is legitimate.
	if n := has(CheckSnapshot(build("", "knows", ""), DefaultCheckOptions()), CheckSpoilerLeak, "f1"); n != 0 {
		t.Fatalf("solo pc knowledge: got %d spoiler_leak findings, want 0", n)
	}
	// A secret nobody renders leaks nothing, whatever the party row says.
	if n := has(CheckSnapshot(build("unaware", "", ""), DefaultCheckOptions()), CheckSpoilerLeak, "f1"); n != 0 {
		t.Fatalf("unrendered fact: got %d spoiler_leak findings, want 0", n)
	}
	// Suspects grants a surface too.
	if n := has(CheckSnapshot(build("unaware", "suspects", ""), DefaultCheckOptions()), CheckSpoilerLeak, "f1"); n != 1 {
		t.Fatalf("party unaware + pc suspects: got %d spoiler_leak findings, want 1", n)
	}

	// Dead facts render nowhere: proposed and superseded facts cannot leak.
	g := baseGraph()
	f := canonFact(g, "f2", campaign.VisibilityPublic)
	f.Confidence = campaign.ConfidenceProposed
	g.Facts[0] = f
	s := pureSnap(g)
	s.Awareness = []AwarenessRow{
		{Knower: campaign.PartyKnower, FactID: "f2", Stance: "unaware", CreatedAt: t1, UpdatedAt: t1},
		{Knower: "thalia", FactID: "f2", Stance: "knows", CreatedAt: t1, UpdatedAt: t1},
	}
	if n := has(CheckSnapshot(s, DefaultCheckOptions()), CheckSpoilerLeak, "f2"); n != 0 {
		t.Fatalf("proposed fact: got %d spoiler_leak findings, want 0", n)
	}

	g = baseGraph()
	canonFact(g, "f3", campaign.VisibilityPublic)
	g.Facts[0].SupersededBy = "f4" // the retcon's replacement; dangling here is fine, the fact is dead either way
	s = pureSnap(g)
	s.Awareness = []AwarenessRow{
		{Knower: campaign.PartyKnower, FactID: "f3", Stance: "unaware", CreatedAt: t1, UpdatedAt: t1},
		{Knower: "thalia", FactID: "f3", Stance: "knows", CreatedAt: t1, UpdatedAt: t1},
	}
	if n := has(CheckSnapshot(s, DefaultCheckOptions()), CheckSpoilerLeak, "f3"); n != 0 {
		t.Fatalf("superseded fact: got %d spoiler_leak findings, want 0", n)
	}
}

/* ---------- knowledge_before_discovery ---------- */

func TestKnowledgeBeforeDiscovery(t *testing.T) {
	build := func(awareAt, sessionStart time.Time, sessionID string) *Snapshot {
		g := baseGraph()
		canonFact(g, "f1", campaign.VisibilityPublic)
		s := pureSnap(g)
		s.Sessions = []SessionRef{{ID: sessionID, Ordinal: 1, Status: "done", StartedAt: sessionStart, CreatedAt: sessionStart}}
		s.Discoveries = []DiscoveryRow{{ID: "d1", FactID: "f1", DiscoveredBy: "thalia", SessionID: sessionID, CreatedAt: sessionStart}}
		s.Awareness = []AwarenessRow{{Knower: "thalia", FactID: "f1", Stance: "knows", DiscoveryID: "d1", CreatedAt: awareAt, UpdatedAt: awareAt}}
		return s
	}

	// Knowledge recorded before the session that produced its discovery.
	if n := has(CheckSnapshot(build(t0, t1, "s1"), DefaultCheckOptions()), CheckKnowledgeBeforeDiscovery, "thalia/f1"); n != 1 {
		t.Fatalf("awareness predates session: got %d findings, want 1", n)
	}
	// Knowledge recorded at or after the session is fine.
	if n := has(CheckSnapshot(build(t2, t1, "s1"), DefaultCheckOptions()), CheckKnowledgeBeforeDiscovery, "thalia/f1"); n != 0 {
		t.Fatalf("awareness after session: got %d findings, want 0", n)
	}
	// A discovery typed at the table (no session) cannot be session-dated.
	if n := has(CheckSnapshot(build(t0, t1, ""), DefaultCheckOptions()), CheckKnowledgeBeforeDiscovery, "thalia/f1"); n != 0 {
		t.Fatalf("sessionless discovery: got %d findings, want 0", n)
	}

	// A session that never started is dated by its creation.
	g := baseGraph()
	canonFact(g, "f2", campaign.VisibilityPublic)
	s := pureSnap(g)
	s.Sessions = []SessionRef{{ID: "s2", Ordinal: 2, Status: "planned", CreatedAt: t2}}
	s.Discoveries = []DiscoveryRow{{ID: "d2", FactID: "f2", DiscoveredBy: "party", SessionID: "s2", CreatedAt: t2}}
	s.Awareness = []AwarenessRow{{Knower: campaign.PartyKnower, FactID: "f2", Stance: "knows", DiscoveryID: "d2", CreatedAt: t1, UpdatedAt: t1}}
	if n := has(CheckSnapshot(s, DefaultCheckOptions()), CheckKnowledgeBeforeDiscovery, "party/f2"); n != 1 {
		t.Fatalf("planned-session dating: got %d findings, want 1", n)
	}
}

/* ---------- awareness_without_source ---------- */

func TestAwarenessWithoutSource(t *testing.T) {
	build := func(stance, discoveryID string, trail []DiscoveryRow) *Snapshot {
		g := baseGraph()
		canonFact(g, "f1", campaign.VisibilityPublic)
		s := pureSnap(g)
		s.Discoveries = trail
		s.Awareness = []AwarenessRow{{Knower: "thalia", FactID: "f1", Stance: stance, DiscoveryID: discoveryID, CreatedAt: t1, UpdatedAt: t1}}
		return s
	}

	// A granting stance with no trail behind it.
	if n := has(CheckSnapshot(build("knows", "", nil), DefaultCheckOptions()), CheckAwarenessWithoutSource, "thalia/f1"); n != 1 {
		t.Fatalf("no trail: got %d findings, want 1", n)
	}
	// The character's own discovery explains the row.
	if n := has(CheckSnapshot(build("knows", "", []DiscoveryRow{{ID: "d1", FactID: "f1", DiscoveredBy: "thalia"}}), DefaultCheckOptions()), CheckAwarenessWithoutSource, "thalia/f1"); n != 0 {
		t.Fatalf("own trail: got %d findings, want 0", n)
	}
	// The party's discovery explains a pc's row the same way the character
	// scope reads own-plus-party.
	if n := has(CheckSnapshot(build("knows", "", []DiscoveryRow{{ID: "d1", FactID: "f1", DiscoveredBy: campaign.PartyKnower}}), DefaultCheckOptions()), CheckAwarenessWithoutSource, "thalia/f1"); n != 0 {
		t.Fatalf("party trail: got %d findings, want 0", n)
	}
	// A linked discovery id is a trail (even a dangling one — the link is
	// what matters here; knowledge_before_discovery owns dating).
	if n := has(CheckSnapshot(build("suspects", "d1", nil), DefaultCheckOptions()), CheckAwarenessWithoutSource, "thalia/f1"); n != 0 {
		t.Fatalf("linked discovery: got %d findings, want 0", n)
	}
	// Not knowing something needs no audit trail.
	if n := has(CheckSnapshot(build("unaware", "", nil), DefaultCheckOptions()), CheckAwarenessWithoutSource, "thalia/f1"); n != 0 {
		t.Fatalf("unaware stance: got %d findings, want 0", n)
	}
	// Someone else's trail does not explain the row.
	if n := has(CheckSnapshot(build("knows", "", []DiscoveryRow{{ID: "d1", FactID: "f1", DiscoveredBy: "duke"}}), DefaultCheckOptions()), CheckAwarenessWithoutSource, "thalia/f1"); n != 1 {
		t.Fatalf("foreign trail: got %d findings, want 1", n)
	}
}

/* ---------- orphan_thread ---------- */

func TestOrphanThread(t *testing.T) {
	build := func(age int64, visibility, confidence string, touch bool, sessionDated bool) *Snapshot {
		g := baseGraph()
		f := canonFact(g, "f1", visibility)
		f.Confidence = confidence
		g.Facts[0] = f
		s := pureSnap(g)
		for i := int64(1); i <= age+1; i++ {
			s.Sessions = append(s.Sessions, SessionRef{ID: sid(i), Ordinal: i, Status: "done", CreatedAt: t0.AddDate(0, 0, int(i))})
		}
		if sessionDated {
			s.IntroducedSession["f1"] = sid(1)
		}
		if touch {
			s.Awareness = append(s.Awareness, AwarenessRow{Knower: campaign.PartyKnower, FactID: "f1", Stance: "unaware", CreatedAt: t1, UpdatedAt: t1})
		}
		return s
	}

	// Introduced four sessions ago (default threshold 3), never referenced.
	if n := has(CheckSnapshot(build(4, campaign.VisibilitySecret, campaign.ConfidenceCanon, false, true), DefaultCheckOptions()), CheckOrphanThread, "f1"); n != 1 {
		t.Fatalf("aged untouched secret: got %d findings, want 1", n)
	}
	// Two sessions old is within the threshold.
	if n := has(CheckSnapshot(build(2, campaign.VisibilitySecret, campaign.ConfidenceCanon, false, true), DefaultCheckOptions()), CheckOrphanThread, "f1"); n != 0 {
		t.Fatalf("young secret: got %d findings, want 0", n)
	}
	// A threshold override is honored.
	if n := has(CheckSnapshot(build(2, campaign.VisibilitySecret, campaign.ConfidenceCanon, false, true), CheckOptions{OrphanSessions: 2}), CheckOrphanThread, "f1"); n != 1 {
		t.Fatalf("threshold override: got %d findings, want 1", n)
	}
	// Even a deliberate unaware row is a reference: the DM modeled the moment.
	if n := has(CheckSnapshot(build(4, campaign.VisibilitySecret, campaign.ConfidenceCanon, true, true), DefaultCheckOptions()), CheckOrphanThread, "f1"); n != 0 {
		t.Fatalf("touched secret: got %d findings, want 0", n)
	}
	// Public facts are world lore, not threads.
	if n := has(CheckSnapshot(build(4, campaign.VisibilityPublic, campaign.ConfidenceCanon, false, true), DefaultCheckOptions()), CheckOrphanThread, "f1"); n != 0 {
		t.Fatalf("public fact: got %d findings, want 0", n)
	}
	// Undated provenance cannot be session-aged.
	if n := has(CheckSnapshot(build(4, campaign.VisibilitySecret, campaign.ConfidenceCanon, false, false), DefaultCheckOptions()), CheckOrphanThread, "f1"); n != 0 {
		t.Fatalf("undated fact: got %d findings, want 0", n)
	}
	// A discovery is a reference too.
	g := baseGraph()
	canonFact(g, "f2", campaign.VisibilitySecret)
	s := pureSnap(g)
	for i := int64(1); i <= 5; i++ {
		s.Sessions = append(s.Sessions, SessionRef{ID: sid(i), Ordinal: i, Status: "done", CreatedAt: t0})
	}
	s.IntroducedSession["f2"] = sid(1)
	s.Discoveries = []DiscoveryRow{{ID: "d1", FactID: "f2", DiscoveredBy: "duke", SessionID: sid(2), CreatedAt: t0}}
	if n := has(CheckSnapshot(s, DefaultCheckOptions()), CheckOrphanThread, "f2"); n != 0 {
		t.Fatalf("discovered secret: got %d findings, want 0", n)
	}
}

func sid(ordinal int64) string { return fmt.Sprintf("sess-%d", ordinal) }

/* ---------- unreachable_secret ---------- */

func TestUnreachableSecret(t *testing.T) {
	build := func(visibility, confidence string, rows []AwarenessRow) *Snapshot {
		g := baseGraph()
		f := canonFact(g, "f1", visibility)
		f.Confidence = confidence
		g.Facts[0] = f
		s := pureSnap(g)
		s.Awareness = rows
		return s
	}

	// A secret no awareness row even mentions has no path anyone can reach.
	if n := has(CheckSnapshot(build(campaign.VisibilitySecret, campaign.ConfidenceCanon, nil), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 1 {
		t.Fatalf("untouched secret: got %d findings, want 1", n)
	}
	// An unaware row is a modeled clue opportunity: reachable, not taken.
	rows := []AwarenessRow{{Knower: campaign.PartyKnower, FactID: "f1", Stance: "unaware", CreatedAt: t1, UpdatedAt: t1}}
	if n := has(CheckSnapshot(build(campaign.VisibilitySecret, campaign.ConfidenceCanon, rows), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 0 {
		t.Fatalf("marked-unaware secret: got %d findings, want 0", n)
	}
	// Someone holding it (even wrongly) is reaching it.
	rows = []AwarenessRow{{Knower: "duke", FactID: "f1", Stance: "suspects", CreatedAt: t1, UpdatedAt: t1}}
	if n := has(CheckSnapshot(build(campaign.VisibilitySecret, campaign.ConfidenceCanon, rows), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 0 {
		t.Fatalf("suspected secret: got %d findings, want 0", n)
	}
	// Public facts and non-authoritative secrets are not the check's problem.
	if n := has(CheckSnapshot(build(campaign.VisibilityPublic, campaign.ConfidenceCanon, nil), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 0 {
		t.Fatalf("public fact: got %d findings, want 0", n)
	}
	if n := has(CheckSnapshot(build(campaign.VisibilitySecret, campaign.ConfidenceContested, nil), DefaultCheckOptions()), CheckUnreachableSecret, "f1"); n != 0 {
		t.Fatalf("contested secret: got %d findings, want 0", n)
	}
}

/* ---------- stat_block_unresolved ---------- */

func TestStatBlockUnresolved(t *testing.T) {
	build := func(bestiary map[string]bool, monsters ...encounter.Monster) *Snapshot {
		s := pureSnap(baseGraph())
		if bestiary != nil {
			s.Bestiary = bestiary
		}
		s.Encounters = []EncounterRef{{
			EventID: "ev1", SessionID: "s1", Name: "Roadside ambush", Monsters: monsters,
		}}
		return s
	}
	goblin := encounter.Monster{Name: "Goblin", CR: "1/4", XP: 50, Count: 6}
	zorblat := encounter.Monster{Name: "Zorblat the Unfinished", CR: "5", XP: 1800, Count: 1}

	// A monster with no bestiary entry.
	findings := CheckSnapshot(build(map[string]bool{"goblin": true}, goblin, zorblat), DefaultCheckOptions())
	if n := has(findings, CheckStatBlockUnresolved, "ev1"); n != 1 {
		t.Fatalf("unresolved monster: got %d findings, want 1", n)
	}
	var found bool
	for _, f := range findings {
		if f.Check == CheckStatBlockUnresolved {
			found = strings.Contains(f.Message, "Zorblat the Unfinished")
		}
	}
	if !found {
		t.Fatal("the finding must name the unresolved monster")
	}
	// Everything resolves: clean.
	if n := has(CheckSnapshot(build(map[string]bool{"goblin": true}, goblin), DefaultCheckOptions()), CheckStatBlockUnresolved, "ev1"); n != 0 {
		t.Fatalf("resolved roster: got %d findings, want 0", n)
	}
	// An empty mirror means "cannot resolve", never "does not exist".
	if n := has(CheckSnapshot(build(nil, goblin, zorblat), DefaultCheckOptions()), CheckStatBlockUnresolved, "ev1"); n != 0 {
		t.Fatalf("empty bestiary: got %d findings, want 0", n)
	}
}

/* ---------- party_level_drift ---------- */

func TestPartyLevelDrift(t *testing.T) {
	// Six goblins against four 1st-level characters is a Deadly encounter;
	// against four 5th-level characters it is Trivial. Plan it, level the
	// party, and the drift must surface.
	roster := []encounter.Monster{{Name: "Goblin", CR: "1/4", XP: 50, Count: 6}}
	build := func(planned, current []int) *Snapshot {
		s := pureSnap(baseGraph())
		s.Party = current
		s.Encounters = []EncounterRef{{
			EventID: "ev1", SessionID: "s1", Name: "Roadside ambush",
			Party: planned, Monsters: roster,
		}}
		return s
	}
	findings := CheckSnapshot(build([]int{1, 1, 1, 1}, []int{5, 5, 5, 5}), DefaultCheckOptions())
	if n := has(findings, CheckPartyLevelDrift, "ev1"); n != 1 {
		t.Fatalf("drifted encounter: got %d findings, want 1", n)
	}
	var msg string
	for _, f := range findings {
		if f.Check == CheckPartyLevelDrift {
			msg = f.Message
		}
	}
	if !strings.Contains(msg, "Deadly") || !strings.Contains(msg, "Trivial") {
		t.Fatalf("the finding must name both bands, got %q", msg)
	}
	// Same party, same band: no drift.
	if n := has(CheckSnapshot(build([]int{5, 5, 5, 5}, []int{5, 5, 5, 5}), DefaultCheckOptions()), CheckPartyLevelDrift, "ev1"); n != 0 {
		t.Fatalf("matched encounter: got %d findings, want 0", n)
	}
	// An encounter that recorded no party cannot drift.
	if n := has(CheckSnapshot(build(nil, []int{5, 5, 5, 5}), DefaultCheckOptions()), CheckPartyLevelDrift, "ev1"); n != 0 {
		t.Fatalf("no planned party: got %d findings, want 0", n)
	}
	// A campaign whose pcs declare no levels has no "today" to drift from.
	if n := has(CheckSnapshot(build([]int{1, 1, 1, 1}, nil), DefaultCheckOptions()), CheckPartyLevelDrift, "ev1"); n != 0 {
		t.Fatalf("no current party: got %d findings, want 0", n)
	}
}

/* ---------- the union ---------- */

// CheckSnapshot must carry the campaign package's graph rules alongside the
// new ones: one engine, one finding list.
func TestCheckSnapshotIncludesGraphRules(t *testing.T) {
	g := baseGraph()
	canonFact(g, "f1", campaign.VisibilityPublic)
	g.ProvenanceCount["f1"] = 0 // fact_without_provenance: a bug by definition
	findings := CheckSnapshot(pureSnap(g), DefaultCheckOptions())
	if n := has(findings, campaign.CheckFactWithoutProvenance, "f1"); n != 1 {
		t.Fatalf("graph rules missing from the engine: got %d fact_without_provenance findings, want 1", n)
	}
}
