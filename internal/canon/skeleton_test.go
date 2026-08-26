package canon

// The campaign skeleton generator's tests (MAD-361): the deterministic
// premise read, the act-band arithmetic, the faction web's legality, and the
// end-to-end exchange over a temp SQLite database with a fake LLM client
// replaying a fixture (ADR 8) — generation, staging, acceptance and the canon
// check's verdict on what landed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/story"
)

/* ---------- the deterministic read ---------- */

func TestReadPremise(t *testing.T) {
	h := ReadPremise("Dark fantasy: a kingdom consumed by an ancient forest, political intrigue and occasional nasty horror")
	for _, tone := range []string{"dark", "horror", "intrigue"} {
		if !contains(h.Tones, tone) {
			t.Errorf("tones %v missing %q", h.Tones, tone)
		}
	}
	if !contains(h.Threats, "ancient forest") {
		t.Errorf("threats %v missing the forest", h.Threats)
	}
	if !contains(h.Settings, "kingdom") {
		t.Errorf("settings %v missing the kingdom", h.Settings)
	}
	if !h.political() {
		t.Error("a political-intrigue premise must read as political")
	}

	plain := ReadPremise("A straightforward dungeon crawl")
	if plain.political() {
		t.Error("a dungeon crawl is not political intrigue")
	}
	if len(plain.Tones) != 0 {
		t.Errorf("a plain premise should carry no tones, got %v", plain.Tones)
	}

	// The guild/archipelago/city vocabularies, plural-tolerant.
	h = ReadPremise("rival guilds and a drowned archipelago of cities")
	if !contains(h.Archetypes, "guild") || !contains(h.Threats, "the drowned sea") || !contains(h.Settings, "city") {
		t.Errorf("guilds/archipelago read wrong: %+v", h)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

/* ---------- the arithmetic ---------- */

func TestActBands_TwelveLevelsFourActsTileExactly(t *testing.T) {
	bands := ActBands(story.Pace(1, 12, 4))
	want := []Band{{1, 3}, {4, 6}, {7, 9}, {10, 12}}
	if len(bands) != len(want) {
		t.Fatalf("bands = %v, want %v", bands, want)
	}
	for i, b := range bands {
		if b != want[i] {
			t.Fatalf("band %d = %v, want %v (all: %v)", i, b, want[i], bands)
		}
	}
}

func TestActBands_EveryShapeTilesExactly(t *testing.T) {
	// Our arithmetic, asserted with the model faked out of the picture: for
	// every band and every legal act count the band can sustain, the acts
	// tile the range exactly — no gap, no overlap.
	for start := 1; start <= 20; start++ {
		for end := start; end <= 20; end++ {
			span := end - start + 1
			for _, n := range []int{3, 4, 5} {
				if n > span {
					continue
				}
				bands := ActBands(story.Pace(start, end, n))
				if len(bands) != n {
					t.Fatalf("%d-%d in %d acts: got %d bands", start, end, n, len(bands))
				}
				if bands[0].Start != start || bands[n-1].End != end {
					t.Fatalf("%d-%d in %d acts: ends do not cover the range: %v", start, end, n, bands)
				}
				for i := 1; i < n; i++ {
					if bands[i].Start != bands[i-1].End+1 {
						t.Fatalf("%d-%d in %d acts: band %d (%v) does not chain band %d (%v)",
							start, end, n, i, bands[i], i-1, bands[i-1])
					}
					if bands[i].Start > bands[i].End {
						t.Fatalf("%d-%d in %d acts: band %d is inverted: %v", start, end, n, i, bands[i])
					}
				}
			}
		}
	}
}

func TestWebEdges_LegalAndReachingTheSecret(t *testing.T) {
	seeded := map[string]bool{
		"knows": true, "allied_with": true, "enemy_of": true, "related_to": true,
		"serves": true, "served_by": true, "member_of": true, "has_member": true,
		"leads": true, "led_by": true, "owns": true, "owned_by": true,
		"located_in": true, "contains": true, "worships": true, "worshipped_by": true,
		"betrayed": true, "betrayed_by": true, "secretly_controls": true,
		"secretly_controlled_by": true, "killed": true, "killed_by": true,
	}
	for n := 3; n <= 5; n++ {
		edges := webEdges(n)
		if len(edges) < 2 {
			t.Fatalf("n=%d: too few edges %v", n, edges)
		}
		reaches := false
		seen := map[string]bool{}
		for _, e := range edges {
			if !seeded[e.RelType] {
				t.Errorf("n=%d: edge type %q is not in the controlled vocabulary", n, e.RelType)
			}
			if e.FromRole < 0 || e.FromRole >= n || e.ToRole < 0 || e.ToRole >= n {
				t.Errorf("n=%d: edge %v points outside the web", n, e)
			}
			if e.FromRole == e.ToRole {
				t.Errorf("n=%d: edge %v is a self-loop", n, e)
			}
			k := fmt.Sprintf("%d->%d", e.FromRole, e.ToRole)
			if seen[k] {
				t.Errorf("n=%d: duplicate edge %v", n, e)
			}
			seen[k] = true
			if e.RelType == "secretly_controls" || e.RelType == "betrayed" {
				reaches = true
			}
		}
		if !reaches {
			t.Errorf("n=%d: no secretly_controls/betrayed edge reaches the central secret", n)
		}
	}
}

/* ---------- the end-to-end exchange ---------- */

// skeletonFixturePremise is the issue's own front-door example.
const skeletonFixturePremise = "dark fantasy, a kingdom consumed by an ancient forest, levels 1-12, political intrigue and occasional nasty horror"

// skeletonModelResponse scripts the model's fill of a five-faction, four-act
// skeleton (the premise reads as political). NPC 1 deliberately reuses the
// seed campaign's Duke, exercising the reuse rule. Only the fields the
// requested parts declare are returned — the harness rejects undeclared
// keys, so a re-roll's script must be as narrow as its schema.
func skeletonModelResponse(factionCount int, parts ...string) string {
	want := map[string]bool{PartActs: true, PartFactions: true, PartSecret: true, PartHooks: true}
	if len(parts) > 0 {
		want = map[string]bool{}
		for _, p := range parts {
			want[p] = true
		}
	}
	m := map[string]any{}
	factions := []struct{ name, npc string }{
		{"The Ashen Crown", "Duke Aldric Vane"}, // reused: the seed's Duke
		{"The Pale Root", "Mother Yffre"},
		{"The Wayfarers' Compact", "Sergeant Bray"},
		{"The Sable Ledger", "Vess the Quiet"},
		{"The Verdant Choir", "Cantor Ilm"},
	}
	if want[PartFactions] {
		for i := 1; i <= factionCount; i++ {
			f := factions[i-1]
			m[fmt.Sprintf("faction_%d_name", i)] = f.name
			m[fmt.Sprintf("faction_%d_summary", i)] = fmt.Sprintf("%s wants the kingdom for itself and works through debts, not swords.", f.name)
			m[fmt.Sprintf("npc_%d_name", i)] = f.npc
			m[fmt.Sprintf("npc_%d_summary", i)] = "Precise, patient, and never where the blame lands."
		}
	}
	if want[PartSecret] {
		m["secret_statement"] = "The Pale Root secretly controls the Ashen Crown: the forest does not consume the kingdom, it is collecting it."
	}
	if want[PartHooks] {
		for i := 1; i <= hookCount; i++ {
			m[fmt.Sprintf("hook_%d_statement", i)] = "Carters have gone missing on the old road, and the Pale Root is behind the road disappearances."
			m[fmt.Sprintf("hook_%d_thread", i)] = "the road disappearances"
			m[fmt.Sprintf("hook_%d_lead", i)] = "A limping carter swears it at the Waystone, over his third cup."
		}
	}
	if want[PartActs] {
		for i := 1; i <= 4; i++ {
			m[fmt.Sprintf("act_%d_name", i)] = fmt.Sprintf("Act %d", i)
			m[fmt.Sprintf("act_%d_premise", i)] = "The forest leans closer and the court pretends not to notice."
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// skeletonStore builds a canon store with its graph stores wired, the shape
// the generator runs against.
func skeletonStore(t *testing.T, db *sql.DB, m ModelClient) *Store {
	t.Helper()
	s, err := New(db, m, testConfig())
	if err != nil {
		t.Fatalf("canon store: %v", err)
	}
	cs, err := campaign.New(db)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s.WithGraphStores(cs, ks)
}

// skeletonE2E boots the seeded stack and runs one full exchange.
func skeletonE2E(t *testing.T) (*Store, *campaign.Fixture, *SkeletonResult, string) {
	t.Helper()
	db, fx, _ := seeded(t)
	resp := skeletonModelResponse(5)
	s := skeletonStore(t, db, &fakeModel{responses: []string{resp}})
	res, err := s.GenerateSkeleton(context.Background(), SkeletonInput{
		CampaignID: fx.Campaign.ID,
		Premise:    skeletonFixturePremise,
		LevelStart: 1, LevelEnd: 12, ActCount: 4,
		CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("GenerateSkeleton: %v", err)
	}
	return s, fx, res, resp
}

func TestGenerateSkeleton_StagesTheWholeSkeleton(t *testing.T) {
	s, fx, res, _ := skeletonE2E(t)
	ctx := context.Background()

	if res.Batch == nil {
		t.Fatal("no batch staged")
	}
	if res.Batch.Source != BatchSourceSkeleton {
		t.Errorf("source = %q", res.Batch.Source)
	}
	// Five factions (the premise is political), one NPC reused, four web
	// edges, five leads, the secret with its clue, four hooks with leads.
	counts := map[string]int{}
	for _, it := range res.Batch.Items {
		counts[it.Kind]++
	}
	if counts[ReviewProposedEntity] != 5+4 { // 5 factions + 4 new NPCs
		t.Errorf("entity items = %d (5 factions + 4 new NPCs expected): %v", counts[ReviewProposedEntity], counts)
	}
	if counts[ReviewProposedRelationship] != 4+5 {
		t.Errorf("relationship items = %d (4 web + 5 leads expected)", counts[ReviewProposedRelationship])
	}
	if counts[ReviewProposedFact] != 1+hookCount {
		t.Errorf("fact items = %d (secret + %d hooks expected)", counts[ReviewProposedFact], hookCount)
	}
	if counts[ReviewProposedDiscovery] != 1+hookCount {
		t.Errorf("discovery items = %d (secret clue + %d hook leads expected)", counts[ReviewProposedDiscovery], hookCount)
	}

	// The reuse rule: the Duke is linked, not duplicated.
	dupe := false
	for _, r := range res.Reused {
		if r.Name == "Duke Aldric Vane" && r.ID == fx.Duke {
			dupe = true
		}
	}
	if !dupe {
		t.Errorf("the seed's Duke was not reused: %+v", res.Reused)
	}
	for _, it := range res.Batch.Items {
		if it.Kind == ReviewProposedEntity && strings.EqualFold(it.Subject, "Duke Aldric Vane") {
			t.Errorf("a second Duke was staged: %+v", it)
		}
	}

	// The acts: four acts tiling 1-12 exactly, with the paced session plans.
	if len(res.Acts) != 4 {
		t.Fatalf("acts = %d, want 4", len(res.Acts))
	}
	want := [][2]int{{1, 3}, {4, 6}, {7, 9}, {10, 12}}
	for i, a := range res.Acts {
		if a.LevelStart != want[i][0] || a.LevelEnd != want[i][1] {
			t.Errorf("act %d band = %d-%d, want %d-%d", i+1, a.LevelStart, a.LevelEnd, want[i][0], want[i][1])
		}
	}
	var total int
	for _, ap := range res.Pacing.PerAct {
		total += ap.Sessions
	}
	if len(res.Plans) != total {
		t.Errorf("plans = %d, want %d (the paced sessions)", len(res.Plans), total)
	}

	// The prompt carried the structure: roles, the web, and the existing
	// entities the model was told to reuse.
	prompt := s.model.(*fakeModel).calls[0]
	for _, marker := range []string{"hidden_hand", "secretly_controls", "Duke Aldric Vane", "\"level_start\":1"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}

	// Accept the batch whole, then hold the canon check to the issue's bar.
	before := findingSet(t, s, fx.Campaign.ID)
	dec, err := s.DecideBatch(ctx, fx.Campaign.ID, res.Batch.ID, DecisionAccept, nil, "keeper")
	if err != nil {
		t.Fatalf("DecideBatch: %v", err)
	}
	var secretFactID string
	for _, it := range dec.Items {
		if it.Status != ReviewAccepted && it.Status != ReviewModified {
			t.Fatalf("item %s ended %s: %s", it.ReviewID, it.Status, it.Reason)
		}
		if it.Subject == "The central secret" {
			secretFactID = it.ResultRef
		}
	}
	if secretFactID == "" {
		t.Fatal("the central secret item left no fact")
	}
	after := findingSet(t, s, fx.Campaign.ID)
	for key, f := range after {
		if _, had := before[key]; had {
			continue // the fixture's own findings are the fixture's own
		}
		if f.Severity == campaign.SeverityError {
			t.Errorf("accepted skeleton introduced an error finding: %s: %s", f.Check, f.Message)
		}
		if f.Check == CheckUnreachableSecret {
			t.Errorf("accepted skeleton introduced an unreachable secret: %s (fact %s)", f.Message, f.RecordID)
		}
	}
	if f, ok := after[CheckUnreachableSecret+"/"+secretFactID]; ok {
		t.Errorf("the central secret is unreachable: %s", f.Message)
	}
}

// findingSet loads the engine snapshot and keys its findings for delta
// comparison.
func findingSet(t *testing.T, s *Store, campaignID string) map[string]campaign.Finding {
	t.Helper()
	snap, err := LoadSnapshot(context.Background(), s.db, campaignID)
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	out := map[string]campaign.Finding{}
	for _, f := range CheckSnapshot(snap, CheckOptions{}) {
		out[f.Check+"/"+f.RecordID] = f
	}
	return out
}

func TestGenerateSkeleton_RefusesToRunActsOverActs(t *testing.T) {
	db, fx, _ := seeded(t)
	s := skeletonStore(t, db, &fakeModel{responses: []string{skeletonModelResponse(5), skeletonModelResponse(5, PartFactions)}})
	in := SkeletonInput{
		CampaignID: fx.Campaign.ID, Premise: skeletonFixturePremise,
		LevelStart: 1, LevelEnd: 12, ActCount: 4, CreatedBy: "keeper",
	}
	if _, err := s.GenerateSkeleton(context.Background(), in); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Re-roll the factions only: legal, and the acts stay.
	in.Parts = []string{PartFactions}
	if _, err := s.GenerateSkeleton(context.Background(), in); err != nil {
		t.Fatalf("faction re-roll should keep the acts: %v", err)
	}
	// Naming the acts part again refuses over the existing acts.
	in.Parts = []string{PartActs}
	if _, err := s.GenerateSkeleton(context.Background(), in); err == nil {
		t.Fatal("running the acts part over existing acts should refuse")
	}
}

func TestGenerateSkeleton_SecretRerunAnchorsToAcceptedWeb(t *testing.T) {
	db, fx, _ := seeded(t)
	s := skeletonStore(t, db, &fakeModel{responses: []string{
		skeletonModelResponse(5), `{"secret_statement": "The Pale Root answers to something older still, and the Crown signs what it is told."}`,
	}})
	ctx := context.Background()
	in := SkeletonInput{
		CampaignID: fx.Campaign.ID, Premise: skeletonFixturePremise,
		LevelStart: 1, LevelEnd: 12, ActCount: 4, CreatedBy: "keeper",
	}
	first, err := s.GenerateSkeleton(ctx, in)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := s.DecideBatch(ctx, fx.Campaign.ID, first.Batch.ID, DecisionAccept, nil, "keeper"); err != nil {
		t.Fatalf("accept: %v", err)
	}

	in.Parts = []string{PartSecret}
	res, err := s.GenerateSkeleton(ctx, in)
	if err != nil {
		t.Fatalf("secret re-roll: %v", err)
	}
	if len(res.Batch.Items) != 2 {
		t.Fatalf("secret re-roll staged %d items, want the fact and its clue", len(res.Batch.Items))
	}
	// The new secret's subject is the accepted hidden hand — by id, not by a
	// name the re-roll invented.
	var payload map[string]any
	for _, it := range res.Batch.Items {
		if it.Subject == "The central secret" {
			if err := json.Unmarshal([]byte(it.Detail), &payload); err != nil {
				t.Fatalf("payload: %v", err)
			}
		}
	}
	if payload == nil {
		t.Fatal("no central secret item in the re-rolled batch")
	}
	subject, _ := payload["subject"].(string)
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entities WHERE id = ? AND campaign_id = ?`, subject, fx.Campaign.ID).Scan(&count); err != nil || count != 1 {
		t.Errorf("secret subject %q is not an entity of this campaign (err %v)", subject, err)
	}
}

func TestGenerateSkeleton_ValidationAndOffline(t *testing.T) {
	db, fx, _ := seeded(t)
	s := skeletonStore(t, db, &fakeModel{responses: []string{skeletonModelResponse(5)}})
	ctx := context.Background()
	base := SkeletonInput{CampaignID: fx.Campaign.ID, Premise: skeletonFixturePremise,
		LevelStart: 1, LevelEnd: 12, CreatedBy: "keeper"}

	if _, err := s.GenerateSkeleton(ctx, SkeletonInput{CampaignID: fx.Campaign.ID}); err == nil {
		t.Error("an empty premise should refuse")
	}
	bad := base
	bad.ActCount = 6
	if _, err := s.GenerateSkeleton(ctx, bad); err == nil || !strings.Contains(err.Error(), "shapes") {
		t.Errorf("six acts should name the legal shapes, got %v", err)
	}
	narrow := base
	narrow.LevelStart, narrow.LevelEnd, narrow.ActCount = 1, 2, 3
	if _, err := s.GenerateSkeleton(ctx, narrow); err == nil || !strings.Contains(err.Error(), "wider band") {
		t.Errorf("three acts over two levels should refuse, got %v", err)
	}
	badParts := base
	badParts.Parts = []string{"colour"}
	if _, err := s.GenerateSkeleton(ctx, badParts); err == nil {
		t.Error("an unknown part should refuse")
	}
	// A model naming two factions alike is refused before anything stages.
	dup := strings.Replace(skeletonModelResponse(5), `"faction_3_name":"The Wayfarers' Compact"`,
		`"faction_3_name":"The Pale Root"`, 1)
	sd := skeletonStore(t, db, &fakeModel{responses: []string{dup}})
	if _, err := sd.GenerateSkeleton(ctx, base); err == nil || !strings.Contains(err.Error(), "named twice") {
		t.Errorf("duplicate names should refuse, got %v", err)
	}

	// The offline store refuses with the model-driven error.
	offline, err := NewOffline(db)
	if err != nil {
		t.Fatalf("offline store: %v", err)
	}
	offline = offline.WithGraphStores(s.campaigns, s.knowledge)
	if _, err := offline.GenerateSkeleton(ctx, base); err == nil {
		t.Error("the offline store should refuse to generate")
	}
}

// The acts part is optional on its own: a rerun of just the acts (after a
// delete) needs no faction web.
func TestGenerateSkeleton_ActsAloneNeedsNoWeb(t *testing.T) {
	db, fx, _ := seeded(t)
	actsOnly := func() string {
		m := map[string]any{}
		for i := 1; i <= 3; i++ {
			m[fmt.Sprintf("act_%d_name", i)] = fmt.Sprintf("Act %d", i)
			m[fmt.Sprintf("act_%d_premise", i)] = "The noose tightens."
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	s := skeletonStore(t, db, &fakeModel{responses: []string{actsOnly()}})
	res, err := s.GenerateSkeleton(context.Background(), SkeletonInput{
		CampaignID: fx.Campaign.ID, Premise: "a small desperate war", ActCount: 3,
		Parts: []string{PartActs}, CreatedBy: "keeper",
	})
	if err != nil {
		t.Fatalf("acts alone: %v", err)
	}
	if res.Batch != nil {
		t.Errorf("acts alone staged a batch of %d items; the spine needs none", len(res.Batch.Items))
	}
	if len(res.Acts) != 3 {
		t.Fatalf("acts = %d, want 3", len(res.Acts))
	}
}
