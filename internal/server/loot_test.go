package server

// The loot surface's HTTP tests (MAD-384): the reads are DM-only, the
// distribution folds from the event log and reconciles with the canon
// engine's own possession read on a seeded campaign, the power curve
// carries its arithmetic, the generator degrades to tier-only when the
// party block is empty, a reroll moves one line, and placing stages a
// proposal batch that writes nothing by itself.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/loot"
	"github.com/madeofpendletonwool/grimoire/internal/story"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// campaignFactID looks a fact up by its statement, for grants that need
// the fact's id.
func campaignFactID(t *testing.T, s *Server, campaignID, statement string) (string, error) {
	t.Helper()
	facts, err := s.campaigns.ListFacts(t.Context(), campaign.ScopeDM, campaignID, campaign.FactFilter{})
	if err != nil {
		return "", err
	}
	for _, f := range facts {
		if f.Statement == statement {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("fact %q not found", statement)
}

// registerOutsider signs a second user up with no campaign membership —
// the stranger a 404 is for.
func registerOutsider(t *testing.T, s *Server, name string) *http.Cookie {
	t.Helper()
	u, err := s.users.CreateUser(t.Context(), name, "a-fine-passphrase")
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	ses, err := s.users.StartSession(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("stranger session: %v", err)
	}
	return &http.Cookie{Name: "grimoire_session", Value: ses.Token}
}

// newLootServer boots a gated server with the campaign graph, the canon
// engine, the narrative spine and the item catalog wired, the catalog fed
// by a stubbed Open5e shelf with items across the rarities and tags the
// hoard tables' profiles need.
func newLootServer(t *testing.T) (*Server, *fixture) {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	shelf := `{"next":"","results":[
		{"key":"flame-tongue","name":"Flame Tongue","document":{"key":"srd-2014"},
		 "category":{"name":"Weapon","key":"weapon"},"rarity":{"name":"Rare","rank":2},
		 "desc":"You gain a +1 bonus to attack and damage rolls made with this magic weapon. When you hit with it, the target takes an extra 2d6 fire damage."},
		{"key":"bulwark-plate","name":"Armor of the Bulwark","document":{"key":"srd-2014"},
		 "category":{"name":"Armor","key":"armor"},"rarity":{"name":"Uncommon","rank":1},
		 "desc":"While wearing this armor, you have resistance to fire damage."},
		{"key":"shadow-orb","name":"Orb of Shadows","document":{"key":"srd-2014"},
		 "category":{"name":"Wondrous Item","key":"wondrous-item"},"rarity":{"name":"Legendary","rank":4},
		 "requires_attunement":true,
		 "desc":"While holding this orb, you can cast darkness. The target must succeed on a saving throw or be blinded."},
		{"key":"mending-potion","name":"Potion of Mending","document":{"key":"srd-2014"},
		 "category":{"name":"Potion","key":"potion"},"rarity":{"name":"Common","rank":0},
		 "desc":"You can use an action to drink this potion."},
		{"key":"warding-scroll","name":"Scroll of Warding","document":{"key":"srd-2014"},
		 "category":{"name":"Scroll","key":"scroll"},"rarity":{"name":"Very Rare","rank":3},
		 "desc":"You gain a +2 bonus to saving throws while holding this scroll."}
	]}`
	open5e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, shelf)
	}))
	t.Cleanup(open5e.Close)

	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("open auth store: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatalf("open campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	catalog, err := items.NewCatalog(store.DB(), open5e.URL)
	if err != nil {
		t.Fatalf("open item catalog: %v", err)
	}
	if err := catalog.Sync(t.Context()); err != nil {
		t.Fatalf("sync item catalog: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	stories, err := story.New(store.DB())
	if err != nil {
		t.Fatalf("open story store: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).
		WithCanon(engine).
		WithStory(stories).
		WithItems(catalog, items.NewHomebrewStore(store.DB()))
	f := buildFixture(t, s)
	return s, &f
}

// partyItemsFixture declares three pcs and the records the fold reads: a
// dated relationship (Bran owns the Flame Tongue since the event), a
// possession fact (the Orb is carried by Mira), and a party-block item on
// Bran's sheet.
func partyItemsFixture(t *testing.T, s *Server, f *fixture, dm *http.Cookie) (bran, mira, keth, tongue, orb string) {
	t.Helper()
	ctx := t.Context()
	mkPC := func(name, payload string) string {
		body := `{"kind":"pc","name":` + quote(name) + `,"payload":` + payload + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	mkItem := func(name string) string {
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
			`{"kind":"item","name":`+quote(name)+`}`, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create item %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	bran = mkPC("Bran", `{"level":3,"class":"rogue"}`)
	mira = mkPC("Mira", `{"level":5,"class":"wizard"}`)
	keth = mkPC("Keth", `{"level":4,"class":"cleric"}`)
	tongue = mkItem("Flame Tongue")
	orb = mkItem("Orb of Shadows")

	// The hand-out: an event in the log, dated by play order, and the
	// relationship that names Bran the tongue's owner since it.
	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/events",
		`{"summary":"Blan takes the tongue from the altar"}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create event: status %d, body %s", r.Code, r.Body)
	}
	eventID := idFrom(t, r, "event")
	cs := s.campaigns
	if _, err := cs.CreateRelationship(ctx, f.campaignID, bran, "owns", tongue, 0, "", eventID); err != nil {
		t.Fatalf("relationship: %v", err)
	}

	// The possession fact the canon engine also reads: the orb is carried
	// by Mira, authored by the DM.
	if _, err := cs.CreateFact(ctx, f.campaignID, orb, "carried_by", mira, "",
		"The Orb of Shadows is carried by Mira.", campaign.ConfidenceCanon,
		campaign.VisibilityPublic, "dm", []campaign.ProvenanceInput{
			{Quote: "The Orb of Shadows is carried by Mira.", Method: campaign.MethodDMAuthored},
		}); err != nil {
		t.Fatalf("possession fact: %v", err)
	}

	// The party block's declared sheet: Bran carries the tongue.
	if _, err := cs.UpdateEntity(ctx, f.campaignID, bran, nil, nil, nil,
		map[string]any{"level": 3, "class": "rogue", "items": []string{tongue}}); err != nil {
		t.Fatalf("party block: %v", err)
	}
	return bran, mira, keth, tongue, orb
}

/* ---------- the gates ---------- */

// The loot reads are DM material: a player is a 403, a stranger a 404.
func TestLootSurfacesAreDMOnly(t *testing.T) {
	s, f := newLootServer(t)
	dm := dmSession(t, s)
	partyItemsFixture(t, s, f, dm)
	player := addPlayerMember(t, s, *f, "loot-player", false)
	// A stranger: signed in, but with no standing in the campaign.
	stranger := registerOutsider(t, s, "loot-stranger")

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/loot/distribution"},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/loot/power-curve"},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/loot/hoard"},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/loot/hoard/place"},
	} {
		if r := hit(t, s, tc.method, tc.path, `{}`, player); r.Code != http.StatusForbidden {
			t.Errorf("%s %s as player: status %d, want 403", tc.method, tc.path, r.Code)
		}
		if r := hit(t, s, tc.method, tc.path, `{}`, stranger); r.Code != http.StatusNotFound {
			t.Errorf("%s %s as stranger: status %d, want 404", tc.method, tc.path, r.Code)
		}
	}
	// The DM reads.
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/loot/distribution", "", dm); r.Code != http.StatusOK {
		t.Fatalf("dm distribution: status %d, body %s", r.Code, r.Body)
	}
}

/* ---------- the distribution ---------- */

// The distribution is derived from the records the campaign already has —
// the dated relationship reads as received, the possession fact reads as
// recorded, the party block reads as declared — and nobody double-counts.
func TestLootDistributionFoldsTheRecords(t *testing.T) {
	s, f := newLootServer(t)
	dm := dmSession(t, s)
	partyItemsFixture(t, s, f, dm)

	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/loot/distribution", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("distribution: status %d, body %s", r.Code, r.Body)
	}
	var body struct {
		Distribution loot.Distribution `json:"distribution"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := body.Distribution
	if len(d.PCs) != 4 {
		t.Fatalf("distribution has %d pcs, want 4 (the fixture's pc included)", len(d.PCs))
	}
	byPC := map[string]loot.PCDistribution{}
	for _, p := range d.PCs {
		byPC[p.Name] = p
	}
	bran := byPC["Bran"]
	if bran.Total != 1 || len(bran.Timeline) != 1 {
		t.Fatalf("Bran holds %d items across %d rows, want one — the relationship and the party block are the same sword", bran.Total, len(bran.Timeline))
	}
	row := bran.Timeline[0]
	if row.Source != loot.SourceRelation || !row.Dated || row.Ordinal == nil || *row.Ordinal != 1 {
		t.Errorf("Bran's row = %+v, want the event-dated relationship at ordinal 1", row)
	}
	mira := byPC["Mira"]
	if mira.Total != 1 || mira.Timeline[0].Source != loot.SourceFact || mira.Timeline[0].Dated {
		t.Errorf("Mira's row = %+v, want the undated possession fact", mira.Timeline)
	}
	if !strings.Contains(strings.Join(d.Notes, "; "), "Keth") {
		t.Errorf("the notes never name Keth, who has received nothing: %v", d.Notes)
	}
}

/* ---------- the power curve ---------- */

// The power curve shows its arithmetic and its expectation — the XGE
// numbers, the floor, the counts — not a bare verdict.
func TestLootPowerCurveCarriesItsArithmetic(t *testing.T) {
	s, f := newLootServer(t)
	dm := dmSession(t, s)
	partyItemsFixture(t, s, f, dm)

	r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/loot/power-curve", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("power curve: status %d, body %s", r.Code, r.Body)
	}
	var body struct {
		Curve loot.PowerCurve `json:"power_curve"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := body.Curve
	if !c.Computable {
		t.Fatalf("the curve cannot compute: %s", c.Reason)
	}
	if c.Tier != loot.Tier1 || c.ExpectationTotal != 11 {
		t.Errorf("tier %d expectation %d, want tier 1 and 11", c.Tier, c.ExpectationTotal)
	}
	// Three holdings on the sheet, deduped to two items: the tongue the
	// relationship and the party block both name, and the orb the fact
	// carries. The tongue classifies Rare, the orb Legendary.
	if c.HeldTotal != 2 || c.Unclassified != 0 || c.Held["Rare"] != 1 || c.Held["Legendary"] != 1 {
		t.Errorf("held = %d (%v), unclassified %d — want 2, one Rare, one Legendary",
			c.HeldTotal, c.Held, c.Unclassified)
	}
	if len(c.Arithmetic) < 3 {
		t.Errorf("the curve carries %d arithmetic lines, want the tier, the holding and the expectation", len(c.Arithmetic))
	}
	if !strings.Contains(c.Verdict, "under-equipped") {
		t.Errorf("three items at tier 1 reads %q", c.Verdict)
	}
}

/* ---------- the generator ---------- */

// The generator rolls from the party's tier, every line carries its
// reason, and a reroll moves exactly the named line.
func TestLootGenerateAndReroll(t *testing.T) {
	s, f := newLootServer(t)
	dm := dmSession(t, s)
	partyItemsFixture(t, s, f, dm)

	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/loot/hoard", `{}`, dm)
	if r.Code != http.StatusOK {
		t.Fatalf("generate: status %d, body %s", r.Code, r.Body)
	}
	var body struct {
		Hoard loot.Hoard `json:"hoard"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	h := body.Hoard
	if h.Tier != loot.Tier1 || h.Degraded {
		t.Fatalf("tier %d degraded=%v, want tier 1 from the declared levels", h.Tier, h.Degraded)
	}
	if h.Seed == 0 {
		t.Fatal("the hoard carries no seed to reroll against")
	}
	for _, line := range h.Items {
		if strings.TrimSpace(line.Reason) == "" {
			t.Errorf("line %s (%s) carries no reason", line.Key, line.Name)
		}
	}

	// Reroll every line in turn against the same seed: the coins and the
	// other lines never move.
	for _, line := range h.Items {
		req := fmt.Sprintf(`{"seed":%d,"has_seed":true,"rerolls":{%q:42}}`,
			h.Seed, strings.ReplaceAll(line.Key, `"`, `\"`))
		rr := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/loot/hoard/reroll", req, dm)
		if rr.Code != http.StatusOK {
			t.Fatalf("reroll %s: status %d, body %s", line.Key, rr.Code, rr.Body)
		}
		var rb struct {
			Hoard loot.Hoard `json:"hoard"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &rb); err != nil {
			t.Fatalf("decode reroll: %v", err)
		}
		if rb.Hoard.Row != h.Row || len(rb.Hoard.Coins) != len(h.Coins) || len(rb.Hoard.Items) != len(h.Items) {
			t.Fatalf("rerolling %s reshaped the hoard", line.Key)
		}
		for i := range rb.Hoard.Coins {
			if rb.Hoard.Coins[i] != h.Coins[i] {
				t.Errorf("rerolling %s moved the coins", line.Key)
			}
		}
		for i := range rb.Hoard.Items {
			same := rb.Hoard.Items[i].Name == h.Items[i].Name
			if rb.Hoard.Items[i].Key == line.Key {
				continue // the rerolled line may differ
			}
			if !same {
				t.Errorf("rerolling %s also moved %s", line.Key, h.Items[i].Key)
			}
		}
	}

	// A campaign whose pcs declare no levels and an ask with no tier is a
	// 400 that says why.
	r = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/loot/hoard", `{"tier":9}`, dm)
	if r.Code != http.StatusBadRequest {
		t.Errorf("tier 9 generated anyway: status %d", r.Code)
	}
}

// A campaign with an empty party block still generates a hoard — degraded
// to tier-only, and saying so.
func TestLootGenerateDegradesToTierOnly(t *testing.T) {
	s, f := newLootServer(t)
	dm := dmSession(t, s)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/loot/hoard", `{"tier":2}`, dm); r.Code != http.StatusOK {
		t.Fatalf("degraded generate: status %d, body %s", r.Code, r.Body)
	} else {
		var body struct {
			Hoard loot.Hoard `json:"hoard"`
		}
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.Hoard.Degraded {
			t.Fatal("a hoard with no party read is not marked degraded")
		}
		said := false
		for _, n := range body.Hoard.Notes {
			if strings.Contains(n, "tier-only") {
				said = true
			}
		}
		if !said {
			t.Errorf("the degraded hoard does not say so: %v", body.Hoard.Notes)
		}
	}
	// And with neither a tier nor a level, the 400 names the gap.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/loot/hoard", `{}`, dm); r.Code != http.StatusBadRequest {
		t.Errorf("a level-less campaign with no tier generated anyway: status %d", r.Code)
	}
}

/* ---------- the placement ---------- */

// lenEntities and lenEvents count a campaign's graph rows for the
// write-nothing assertions.
func lenEntities(t *testing.T, s *Server, campaignID string) (int, error) {
	t.Helper()
	list, err := s.campaigns.ListEntities(t.Context(), campaign.ScopeDM, campaignID, "")
	return len(list), err
}

func lenEvents(t *testing.T, s *Server, campaignID string) (int, error) {
	t.Helper()
	list, err := s.campaigns.ListEvents(t.Context(), campaign.ScopeDM, campaignID)
	return len(list), err
}

// Placing stages the proposal batch and writes nothing by itself; the DM
// accepting the batch is the only thing that lands the items and the
// hand-out.
func TestLootPlaceStagesAProposal(t *testing.T) {
	s, f := newLootServer(t)
	dm := dmSession(t, s)
	bran, _, _, _, orb := partyItemsFixture(t, s, f, dm)

	count := func(fn func() (int, error)) int {
		n, err := fn()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	entitiesBefore := count(func() (int, error) { return lenEntities(t, s, f.campaignID) })
	eventsBefore := count(func() (int, error) { return lenEvents(t, s, f.campaignID) })

	body := fmt.Sprintf(`{"summary":"The crypt hoard is handed out.","items":[
		{"key":"item-1","slug":"flame-tongue","name":"Flame Tongue","rarity":"Rare",
		 "reason":"Magic Item Table C rides Uncommon-to-Rare; sits with Keth, who holds nothing."},
		{"key":"narrative","entity_id":%q,"name":"Orb of Shadows","reason":"carries the act's item"}],
		"participants":[%q]}`, orb, bran)
	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/loot/hoard/place", body, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("place: status %d, body %s", r.Code, r.Body)
	}
	var pr struct {
		Batch struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			ItemCount int    `json:"item_count"`
		} `json:"batch"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pr.Batch.Status != "open" || pr.Batch.ItemCount != 2 {
		t.Fatalf("batch = %s with %d items, want open with the entity and the hand-out", pr.Batch.Status, pr.Batch.ItemCount)
	}
	if got := count(func() (int, error) { return lenEntities(t, s, f.campaignID) }); got != entitiesBefore {
		t.Errorf("placing wrote %d entities before any approval", got-entitiesBefore)
	}
	if got := count(func() (int, error) { return lenEvents(t, s, f.campaignID) }); got != eventsBefore {
		t.Errorf("placing wrote %d events before any approval", got-eventsBefore)
	}

	// Accepting the batch is what writes — and only then.
	dm2 := dmSession(t, s)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+pr.Batch.ID+"/decision",
		`{"decision":"accept"}`, dm2); r.Code != http.StatusOK {
		t.Fatalf("accept: status %d, body %s", r.Code, r.Body)
	}
	if got := count(func() (int, error) { return lenEntities(t, s, f.campaignID) }); got != entitiesBefore+1 {
		t.Errorf("after acceptance the hoard's one generated item is missing: %d vs %d", got, entitiesBefore+1)
	}
	if got := count(func() (int, error) { return lenEvents(t, s, f.campaignID) }); got != eventsBefore+1 {
		t.Errorf("after acceptance the hand-out is missing: %d vs %d", got, eventsBefore+1)
	}
}

/* ---------- the reconciliation ---------- */

// The distribution reconciles with the canon engine's own possession read
// on a seeded campaign: the holder the fold names is the holder the
// continuity check accepts, and the continuity check flags a prep that
// assumes any other holder.
func TestLootDistributionReconcilesWithCanonCheck(t *testing.T) {
	s, _ := newLootServer(t)
	dm := dmSession(t, s)

	// The seed campaign, on the same database, owned by the keeper.
	keeperID, err := s.users.LookupUseridByName(t.Context(), "keeper")
	if err != nil {
		t.Fatalf("lookup keeper: %v", err)
	}
	fx, err := campaign.Seed(t.Context(), s.campaigns.DB(), keeperID, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The Silver Key is carried by Thalia, by DM-authored fact — the same
	// record a DM's table notes would produce.
	cs := s.campaigns
	if _, err := cs.CreateFact(t.Context(), fx.Campaign.ID, fx.Key, "carried_by", fx.Thalia, "",
		"The Silver Key is carried by Thalia.", campaign.ConfidenceCanon,
		campaign.VisibilityPublic, "dm", []campaign.ProvenanceInput{
			{Quote: "The Silver Key is carried by Thalia.", Method: campaign.MethodDMAuthored},
		}); err != nil {
		t.Fatalf("seed possession fact: %v", err)
	}
	// The fact is party-known — the continuity check reads holders only
	// through what the table has been told, so grant it.
	factID, err := campaignFactID(t, s, fx.Campaign.ID, "The Silver Key is carried by Thalia.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.knowledge.SetAwareness(t.Context(), fx.Campaign.ID, campaign.PartyKnower, factID,
		knowledge.StanceKnows, 1, "", ""); err != nil {
		t.Fatalf("grant awareness: %v", err)
	}

	// The fold agrees with the fact: Thalia holds the key.
	r := hit(t, s, http.MethodGet, "/api/campaigns/"+fx.Campaign.ID+"/loot/distribution", "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("seeded distribution: status %d, body %s", r.Code, r.Body)
	}
	var body struct {
		Distribution loot.Distribution `json:"distribution"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, p := range body.Distribution.PCs {
		if p.Name != "Thalia" {
			continue
		}
		for _, row := range p.Timeline {
			if row.ItemName == "The Silver Key" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the fold never shows Thalia holding the key")
	}

	// And the canon check agrees: a prep assuming the key with Thalia
	// raises no misplaced-item finding; assuming it with Bran does.
	engine := s.canon
	check := func(assumedAt string) []campaign.Finding {
		rep, err := engine.CheckContinuity(t.Context(), fx.Campaign.ID, &canon.Prep{
			Title: "The crypt",
			Scenes: []canon.PrepScene{{
				Name:  "The crypt door",
				Items: []canon.PrepItem{{Ref: "The Silver Key", AssumedAt: assumedAt}},
			}},
		})
		if err != nil {
			t.Fatalf("continuity: %v", err)
		}
		var out []campaign.Finding
		for _, fnd := range rep.Findings {
			if fnd.Check == canon.CheckPrepItemMisplaced {
				out = append(out, fnd)
			}
		}
		return out
	}
	if got := check("Thalia"); len(got) != 0 {
		t.Errorf("the continuity check disagrees with the fold: %v", got)
	}
	if got := check("Bran"); len(got) == 0 {
		t.Error("the continuity check accepts a holder the fold never saw")
	}
}
