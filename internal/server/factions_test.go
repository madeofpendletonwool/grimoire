package server

// Faction surface tests (MAD-366). The load-bearing assertions are on HTTP
// response bodies, in the shape the knowledge leak test uses: a player's
// dossier must not contain the private truth, a plan, or a secret fact —
// not "hidden", absent from the body entirely. The read-from-graph rule is
// asserted through the API: adding an owns edge changes the dossier's
// territory with no write to the faction entity.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// factionFixture is a faction with an authored interior, a shrine it owns,
// a public deed the party knows, a secret the party has not earned, and a
// witnessed confrontation that established the ownership in the party's
// sight.
type factionFixture struct {
	campaignID    string
	cultID        string
	shrineID      string
	pcID          string
	secretFactID  string
	publicFactID  string
	witnessedEvID string
}

func buildFactionFixture(t *testing.T, s *Server) factionFixture {
	t.Helper()
	f := buildFixture(t, s) // campaign, a pc, awareness plumbing; claims the install
	dm := dmSession(t, s)

	mk := func(kind, name string) string {
		body := `{"kind":` + quote(kind) + `,"name":` + quote(name) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	cult := mk("faction", "Cult of the Root")
	shrine := mk("location", "The Northern Shrine")

	// The authored interior: public face, private truth, reputation.
	agent := `{"public_face":"A burial society for the poor","private_truth":"the root's own church","reputation":"mourners with muddy spades","goals":["reopen the crypt"],"military":1,"economic":3,"reach":2}`
	patch := `{"payload":{"agent":` + agent + `}}`
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/entities/"+cult, patch, dm); r.Code != http.StatusOK {
		t.Fatalf("author the faction agent block: status %d, body %s", r.Code, r.Body)
	}

	mkFact := func(subject, predicate, object, statement, visibility string) string {
		body := `{"subject":` + quote(subject) + `,"predicate":` + quote(predicate) +
			`,"object_literal":` + quote(object) + `,"statement":` + quote(statement) +
			`,"visibility":` + quote(visibility) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/facts", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create fact: status %d, body %s", r.Code, r.Body)
		}
		return idFrom(t, r, "fact")
	}
	public := mkFact(cult, "buries", "the poor of Blackwater", "The cult buries the poor of Blackwater for free.", "public")
	secret := mkFact(cult, "serves", "the sleeping god", "The cult serves the Verdant God in its sleep.", "secret")

	// The party knows the public deed; the secret they have not earned.
	body := `{"knower":"party","fact_id":` + quote(public) + `,"stance":"knows"}`
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/awareness", body, dm); r.Code != http.StatusOK {
		t.Fatalf("grant public deed: status %d, body %s", r.Code, r.Body)
	}

	// A witnessed confrontation at the shrine establishes the ownership in
	// the party's sight (an edge is player-visible when set at an event the
	// knower witnessed).
	day := int64(40)
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID, `{"clock":`+day2json(day)+`}`, dm); r.Code != http.StatusOK {
		t.Fatalf("move the clock to the confrontation's day: status %d, body %s", r.Code, r.Body)
	}
	ev := `{"summary":"Robed figures turn the party away from the shrine.","clock_at":` + day2json(day) +
		`,"location_entity":` + quote(shrine) +
		`,"participants":[{"entity_id":` + quote(f.pcID) + `,"role":"party"},{"entity_id":` + quote(cult) + `,"role":"present"}]}`
	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/events", ev, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create confrontation: status %d, body %s", r.Code, r.Body)
	}
	eventID := idFrom(t, r, "event")

	rel := `{"from":` + quote(cult) + `,"rel_type":"owns","to":` + quote(shrine) + `,"since_event":` + quote(eventID) + `}`
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/relationships", rel, dm); r.Code != http.StatusCreated {
		t.Fatalf("create owns edge: status %d, body %s", r.Code, r.Body)
	}

	return factionFixture{
		campaignID: f.campaignID, cultID: cult, shrineID: shrine, pcID: f.pcID,
		secretFactID: secret, publicFactID: public, witnessedEvID: eventID,
	}
}

func day2json(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// planBody is a legal two-step plan over a linear machine.
func planBody() string {
	return `{
		"name": "The Root Takes Hold",
		"rate_per_day": 1,
		"state_machine": {
			"initial": "mustering",
			"states": ["mustering", "infiltrated", "bloomed"],
			"edges": [{"from": "mustering", "to": "infiltrated"}, {"from": "infiltrated", "to": "bloomed"}]
		},
		"steps": [
			{"state": "infiltrated", "name": "Infiltrate the mines", "cost": 10},
			{"state": "bloomed", "name": "Bloom beneath Blackwater", "cost": 10}
		]
	}`
}

func TestFactionDossierDMCarriesTheFullInterior(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFactionFixture(t, s)
	dm := dmSession(t, s)

	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID+"/plans", planBody(), dm); r.Code != http.StatusCreated {
		t.Fatalf("create plan: status %d, body %s", r.Code, r.Body)
	}

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dossier: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Faction struct {
			Agent struct {
				PrivateTruth string `json:"private_truth"`
				PublicFace   string `json:"public_face"`
			} `json:"agent"`
			Edges struct {
				Territory []string `json:"territory"`
			} `json:"edges"`
			Facts []struct {
				Visibility string `json:"visibility"`
			} `json:"facts"`
			Plans []struct {
				Name    string `json:"name"`
				Percent float64
			} `json:"plans"`
		} `json:"faction"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode dossier: %v (%s)", err, rec.Body)
	}
	if body.Faction.Agent.PrivateTruth != "the root's own church" {
		t.Fatalf("the DM reads the private truth: %+v", body.Faction.Agent)
	}
	if len(body.Faction.Edges.Territory) != 1 || body.Faction.Edges.Territory[0] != f.shrineID {
		t.Fatalf("territory reads the owns edge: %+v", body.Faction.Edges.Territory)
	}
	secretSeen := false
	for _, fact := range body.Faction.Facts {
		if fact.Visibility == "secret" {
			secretSeen = true
		}
	}
	if !secretSeen {
		t.Fatal("the DM dossier carries the faction's secret facts")
	}
	if len(body.Faction.Plans) != 1 || body.Faction.Plans[0].Name != "The Root Takes Hold" {
		t.Fatalf("the DM dossier carries the plan: %+v", body.Faction.Plans)
	}
}

// TestFactionDossierPlayerScopeIsThePublicFace is the acceptance rule: a
// player-scope dossier read contains no plan, no PrivateTruth and no secret
// fact — asserted on the raw body, the way the leak test asserts.
func TestFactionDossierPlayerScopeIsThePublicFace(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFactionFixture(t, s)
	dm := dmSession(t, s)

	// A plan exists; the player must not learn that.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID+"/plans", planBody(), dm); r.Code != http.StatusCreated {
		t.Fatalf("create plan: status %d, body %s", r.Code, r.Body)
	}

	player := addPlayerMember(t, s, fixture{campaignID: f.campaignID, pcID: f.pcID}, "thalia", true)
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player dossier: status %d, body %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	for _, forbidden := range []string{
		"private_truth", "the root's own church",
		"The cult serves the Verdant God",
		"\"plans\"", "The Root Takes Hold", "goals",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("LEAK: player dossier contains %q: %s", forbidden, raw)
		}
	}
	var body struct {
		Faction struct {
			PublicFace string `json:"public_face"`
			Reputation string `json:"reputation"`
			Edges      struct {
				Territory []string `json:"territory"`
			} `json:"edges"`
			Roster map[string]struct {
				Name string `json:"name"`
			} `json:"roster"`
			Facts []struct {
				ID         string `json:"id"`
				Visibility string `json:"visibility"`
			} `json:"facts"`
		} `json:"faction"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode dossier: %v", err)
	}
	if body.Faction.PublicFace != "A burial society for the poor" {
		t.Fatalf("the player reads the public face: %q", body.Faction.PublicFace)
	}
	if body.Faction.Reputation == "" {
		t.Fatal("the player reads the reputation")
	}
	if len(body.Faction.Edges.Territory) != 1 || body.Faction.Edges.Territory[0] != f.shrineID {
		t.Fatalf("the witnessed ownership is aware territory: %+v", body.Faction.Edges.Territory)
	}
	if name := body.Faction.Roster[f.shrineID].Name; name != "The Northern Shrine" {
		t.Fatalf("the roster names what the scope can read: %+v", body.Faction.Roster)
	}
	for _, fact := range body.Faction.Facts {
		if fact.Visibility == "secret" {
			t.Fatalf("LEAK: secret fact %s in the player dossier", fact.ID)
		}
		if fact.ID == f.secretFactID {
			t.Fatal("LEAK: the unearned secret reached the player dossier")
		}
	}
}

// TestFactionDossierTerritoryReadsLiveFromTheGraph: adding an owns edge
// changes the dossier's territory with no write to the faction entity — the
// entity view's payload and updated_at must be identical across the read.
func TestFactionDossierTerritoryReadsLiveFromTheGraph(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFactionFixture(t, s)
	dm := dmSession(t, s)

	before := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/entities/"+f.cultID, "", dm)
	var beforeBody struct {
		Entity struct {
			UpdatedAt string         `json:"updated_at"`
			Payload   map[string]any `json:"payload"`
		} `json:"entity"`
	}
	if err := json.Unmarshal(before.Body.Bytes(), &beforeBody); err != nil {
		t.Fatal(err)
	}

	// A second holding, added after the first dossier read.
	keep := `{"kind":"location","name":"The Old Crypt"}`
	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", keep, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create crypt: status %d, body %s", r.Code, r.Body)
	}
	crypt := idFrom(t, r, "entity")
	rel := `{"from":` + quote(f.cultID) + `,"rel_type":"owns","to":` + quote(crypt) + `}`
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/relationships", rel, dm); r.Code != http.StatusCreated {
		t.Fatalf("add owns edge: status %d, body %s", r.Code, r.Body)
	}

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID, "", dm)
	var body struct {
		Faction struct {
			Edges struct {
				Territory []string `json:"territory"`
			} `json:"edges"`
			UpdatedAt string         `json:"updated_at"`
			Payload   map[string]any `json:"payload"`
		} `json:"faction"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Faction.Edges.Territory) != 2 {
		t.Fatalf("the new edge changed the territory with no dossier write: %+v", body.Faction.Edges.Territory)
	}
	if body.Faction.UpdatedAt != beforeBody.Entity.UpdatedAt {
		t.Fatalf("the faction entity moved: %s -> %s", beforeBody.Entity.UpdatedAt, body.Faction.UpdatedAt)
	}
	if len(body.Faction.Payload) != len(beforeBody.Entity.Payload) {
		t.Fatalf("the faction payload moved: %v -> %v", beforeBody.Entity.Payload, body.Faction.Payload)
	}
}

func TestFactionPlanEndpoints(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFactionFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, fixture{campaignID: f.campaignID, pcID: f.pcID}, "bran", true)

	// Plans are DM material: the player cannot even list them.
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID+"/plans", "", player); r.Code != http.StatusForbidden {
		t.Fatalf("player plans read: status %d", r.Code)
	}
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID+"/plans", planBody(), player); r.Code != http.StatusForbidden {
		t.Fatalf("player plan create: status %d", r.Code)
	}

	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/factions/"+f.cultID+"/plans", planBody(), dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create plan: status %d, body %s", r.Code, r.Body)
	}
	planID := idFrom(t, r, "plan")

	// A plan for something that is not a faction is refused.
	shrinePlan := planBody()
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/factions/"+f.shrineID+"/plans", shrinePlan, dm); r.Code != http.StatusBadRequest {
		t.Fatalf("a shrine cannot own a plan: status %d, body %s", r.Code, r.Body)
	}

	// Activating stamps the started day.
	patch := `{"status":"active"}`
	patchRec := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/plans/"+planID, patch, dm)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("activate: status %d, body %s", patchRec.Code, patchRec.Body)
	}
	var activated struct {
		Plan struct {
			Status     string   `json:"status"`
			StartedDay *int64   `json:"started_day"`
			NextStates []string `json:"next_states"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(patchRec.Body.Bytes(), &activated); err != nil {
		t.Fatal(err)
	}
	if activated.Plan.Status != "active" || activated.Plan.StartedDay == nil || *activated.Plan.StartedDay != 40 {
		t.Fatalf("activation: %+v", activated.Plan)
	}
	if len(activated.Plan.NextStates) != 1 || activated.Plan.NextStates[0] != "infiltrated" {
		t.Fatalf("next states offer only legal moves: %+v", activated.Plan.NextStates)
	}

	// A transition along an undeclared edge is refused — the TransitionQuest
	// rule, reused.
	illegal := `{"to":"bloomed"}`
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/plans/"+planID+"/transition", illegal, dm); r.Code != http.StatusBadRequest {
		t.Fatalf("undeclared edge must be refused: status %d, body %s", r.Code, r.Body)
	}
	legal := `{"to":"infiltrated","reason":"the DM moved it by hand"}`
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/plans/"+planID+"/transition", legal, dm); r.Code != http.StatusOK {
		t.Fatalf("legal move: status %d, body %s", r.Code, r.Body)
	}

	// The faction listing narrows the browser to factions.
	list := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/factions", "", dm)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "Cult of the Root") {
		t.Fatalf("faction list: status %d, body %s", list.Code, list.Body)
	}
	playerList := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/factions", "", player)
	if playerList.Code != http.StatusOK || strings.Contains(playerList.Body.String(), "\"payload\"") {
		t.Fatalf("player faction list must carry no payloads: %s", playerList.Body)
	}
}
