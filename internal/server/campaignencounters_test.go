package server

// The campaign-scoped encounter and party surfaces (MAD-378), asserted on
// HTTP responses: the DM-only rule is a 403 (ADR 6 — a player gets a
// different read path, never a filtered roster), a create with no party
// falls back to the campaign's declared levels, and the budget call carries
// the campaign's party with the "from your campaign" line the builder shows.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newCampaignEncounterServer boots a gated server with the campaign graph
// and the encounter store wired. The catalog points at a dead URL: nothing
// here syncs the bestiary — the budget is pure arithmetic — and a dead
// mirror is the state a fresh install is in until its first design.
func newCampaignEncounterServer(t *testing.T) (*Server, *fixture) {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
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
	encounters, err := encounter.New(store.DB())
	if err != nil {
		t.Fatalf("open encounter store: %v", err)
	}
	catalog, err := encounter.NewCatalog(store.DB(), "http://127.0.0.1:1/unused")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).
		WithEncounters(encounters, encounter.NewBestiaryWithBase("http://127.0.0.1:1/unused"), catalog)
	f := buildFixture(t, s)
	return s, &f
}

// partyFixture declares the party block the prefill reads: two levelled pcs
// (one with the fuller sheet), an unlevelled one, and an npc with a level
// that must not join the party.
func partyFixture(t *testing.T, s *Server, f *fixture, dm *http.Cookie) {
	t.Helper()
	mk := func(kind, name, payload string) {
		body := `{"kind":` + quote(kind) + `,"name":` + quote(name)
		if payload != "" {
			body += `,"payload":` + payload
		}
		body += `}`
		if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", body, dm); r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
	}
	mk("pc", "Bran", `{"level":3,"class":"rogue"}`)
	mk("pc", "Mira", `{"level":5,"class":"wizard","ac":15,"max_hp":32,
		"resources":{"spell_slots":{"3":2},"hit_dice":5}}`)
	mk("pc", "Keth", `{"class":"cleric"}`)
	mk("npc", "Aaron", `{"level":9}`)
}

func TestCampaignPartyBlockRead(t *testing.T) {
	s, f := newCampaignEncounterServer(t)
	dm := dmSession(t, s)
	partyFixture(t, s, f, dm)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/party", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dm read: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Party struct {
			CampaignID string                  `json:"campaign_id"`
			Members    []campaign.PartyMember  `json:"members"`
			Levels     []int                   `json:"levels"`
			Label      string                  `json:"label"`
			Problems   []campaign.PartyProblem `json:"problems"`
		} `json:"party"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Name order, pcs only, levels declared by name: Bran 3, the fixture's
	// own Mira Thorn (no level), Keth none, Mira 5.
	if len(body.Party.Members) != 4 {
		t.Fatalf("members = %+v, want Bran, Keth, Mira and the fixture's Mira Thorn", body.Party.Members)
	}
	if body.Party.Members[0].Name != "Bran" || body.Party.Members[2].Name != "Mira" || body.Party.Members[3].Name != "Mira Thorn" {
		t.Fatalf("members out of name order: %+v", body.Party.Members)
	}
	if got := body.Party.Levels; len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Fatalf("levels = %v, want [3 5]", got)
	}
	if body.Party.Label != "from your campaign — 2 characters, levels 3, 5" {
		t.Fatalf("label = %q", body.Party.Label)
	}
	// The fuller sheet came through, not just the levels.
	if body.Party.Members[2].Block.AC != 15 || body.Party.Members[2].Block.Resources.HitDice != 5 {
		t.Fatalf("Mira's block did not round-trip: %+v", body.Party.Members[2].Block)
	}
	if len(body.Party.Problems) != 0 {
		t.Fatalf("problems on a well-formed party: %+v", body.Party.Problems)
	}

	// A malformed block key is reported, never fatal.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"pc","name":"Broken","payload":{"level":"not a level","ac":["x"]}}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("create broken pc: status %d, body %s", r.Code, r.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/party", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("a malformed block must not fail the read: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Broken") || !strings.Contains(rec.Body.String(), "level") {
		t.Fatalf("the malformed key must be reported: %s", rec.Body)
	}
}

// A player is refused outright — a party sheet names what the characters
// carry and what they can still cast. ADR 6's rule on a new surface.
func TestCampaignEncountersAreDMOnly(t *testing.T) {
	s, f := newCampaignEncounterServer(t)
	dm := dmSession(t, s)
	partyFixture(t, s, f, dm)
	player := addPlayerMember(t, s, *f, "encplayer", true)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"party read", http.MethodGet, "/api/campaigns/" + f.campaignID + "/party", ""},
		{"list", http.MethodGet, "/api/campaigns/" + f.campaignID + "/encounters", ""},
		{"create", http.MethodPost, "/api/campaigns/" + f.campaignID + "/encounters",
			`{"name":"No","party":[5],"monsters":[{"name":"Goblin","cr":"1/4","count":1}]}`},
		{"budget prefill", http.MethodPost, "/api/encounter/budget",
			`{"campaign_id":` + quote(f.campaignID) + `,"difficulty":"Medium"}`},
	} {
		if r := hit(t, s, tc.method, tc.path, tc.body, player); r.Code != http.StatusForbidden {
			t.Errorf("%s as player: status %d, want 403 (body %s)", tc.name, r.Code, r.Body)
		}
	}

	// A stranger learns nothing: same as a missing campaign.
	inv := createInvite(t, s, dmSession(t, s), "for a stranger")
	code, _ := inv["code"].(string)
	stranger := sessionFrom(t, hit(t, s, http.MethodPost, "/api/auth/register",
		registerJSON("awanderer", "a-fine-passphrase", code)))
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/encounters", "", stranger); r.Code != http.StatusNotFound {
		t.Errorf("stranger list: status %d, want 404", r.Code)
	}
}

// A create with no party is the whole point of the surface: the DM who
// wrote their party down once does not type it again to save a fight
// against it.
func TestCampaignEncounterCreateAndList(t *testing.T) {
	s, f := newCampaignEncounterServer(t)
	dm := dmSession(t, s)
	partyFixture(t, s, f, dm)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/encounters",
		`{"name":"The Zorblat Pit","monsters":[{"name":"Goblin","cr":"1/4","count":4}]}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body)
	}
	var created struct {
		Encounter encounterView `json:"encounter"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	e := created.Encounter
	// The party fell back to the campaign's declared levels, in name order.
	if len(e.Party) != 2 || e.Party[0] != 3 || e.Party[1] != 5 {
		t.Fatalf("party fallback = %v, want [3 5]", e.Party)
	}
	if e.CampaignID != f.campaignID || e.Status != "planned" {
		t.Fatalf("campaign fields: %+v", e)
	}
	if e.Monsters[0].XP != 50 {
		t.Fatalf("XP not derived from CR: %+v", e.Monsters[0])
	}

	// An explicit party wins over the campaign's.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/encounters",
		`{"name":"Tailored","party":[8],"monsters":[{"name":"Ogre","cr":"2","count":1}]}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with party: status %d, body %s", rec.Code, rec.Body)
	}

	// The campaign lists both, newest first; the builder's own picker —
	// the owner list — carries them too, because they are still the DM's.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/encounters", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var list struct {
		Encounters []encounterView `json:"encounters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Encounters) != 2 || list.Encounters[0].Name != "Tailored" {
		t.Fatalf("campaign list = %+v", list.Encounters)
	}
	rec = hit(t, s, http.MethodGet, "/api/encounters", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "The Zorblat Pit") {
		t.Fatalf("owner list lost the campaign encounter: %d %s", rec.Code, rec.Body)
	}

	// A campaign with no declared levels falls back to an empty party —
	// a roster-only encounter, which is what the builder has always let
	// the DM save before filling a party in.
	rec = hit(t, s, http.MethodPost, "/api/campaigns", `{"name":"The Quiet Table","system":"D&D 5e"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second campaign: status %d, body %s", rec.Code, rec.Body)
	}
	quiet := idFrom(t, rec, "campaign")
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+quiet+"/encounters",
		`{"name":"Roster only","monsters":[{"name":"Goblin","cr":"1/4","count":1}]}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create in a levelless campaign: status %d, body %s", rec.Code, rec.Body)
	}
	created.Encounter = encounterView{}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode levelless: %v", err)
	}
	if len(created.Encounter.Party) != 0 {
		t.Fatalf("party = %v, want none — nothing was declared", created.Encounter.Party)
	}
}

// The budget call carries the campaign's party and the line the builder
// prints above the boxes; without a campaign it is exactly the call
// MAD-299 shipped.
func TestEncounterBudgetCampaignPrefill(t *testing.T) {
	s, f := newCampaignEncounterServer(t)
	dm := dmSession(t, s)
	partyFixture(t, s, f, dm)

	rec := hit(t, s, http.MethodPost, "/api/encounter/budget",
		`{"campaign_id":`+quote(f.campaignID)+`,"difficulty":"Medium"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("prefill: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Budget      map[string]any `json:"budget"`
		Party       []int          `json:"party"`
		PartySource string         `json:"party_source"`
		PartyLabel  string         `json:"party_label"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Party) != 2 || body.Party[0] != 3 || body.Party[1] != 5 {
		t.Fatalf("party = %v, want [3 5]", body.Party)
	}
	if body.PartySource != "campaign" || body.PartyLabel == "" {
		t.Fatalf("source = %q label = %q, want the campaign and its line", body.PartySource, body.PartyLabel)
	}
	if body.Budget == nil {
		t.Fatal("the budget itself went missing")
	}

	// An explicit party wins over the campaign's — the DM edited the boxes.
	rec = hit(t, s, http.MethodPost, "/api/encounter/budget",
		`{"campaign_id":`+quote(f.campaignID)+`,"party":[9],"difficulty":"Medium"}`, dm)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode explicit: %v", err)
	}
	if len(body.Party) != 1 || body.Party[0] != 9 || body.PartySource != "" {
		t.Fatalf("explicit party = %v source %q, want the DM's own", body.Party, body.PartySource)
	}

	// Without a campaign: the MAD-299 call, plus the same party echoed back.
	rec = hit(t, s, http.MethodPost, "/api/encounter/budget",
		`{"party":[1,1,1,1],"difficulty":"Easy"}`, dm)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode plain: %v", err)
	}
	if body.PartySource != "" || body.PartyLabel != "" {
		t.Fatalf("a bare budget grew a campaign: %+v", body)
	}
	if len(body.Party) != 4 {
		t.Fatalf("party = %v, want the four that were sent", body.Party)
	}
}
