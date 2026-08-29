package server

// Location surface tests (MAD-370). The load-bearing assertions are on HTTP
// response bodies, in the shape the knowledge leak test uses: a player's
// dossier must not contain the private truth, a secret fact, or an NPC the
// party has not met — not "hidden", absent from the body entirely. The
// read-only rule is asserted the way the faction tests assert it: the
// dossier renders a complete location while the entity row never moves.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// locationFixture is a town with an authored place block (public half plus a
// private truth), a route out to a child monastery, a resident NPC the party
// has met, one they have not, an item, a secret the party has been granted
// anyway, a witnessed event, and quests sited here — one public, one secret.
type locationFixture struct {
	campaignID    string
	townID        string
	monasteryID   string
	metNPCID      string
	unmetNPCID    string
	secretFactID  string
	eventID       string
	publicQuestID string
	secretQuestID string
}

const placeBody = `{
	"kind": "town",
	"scale": "large village",
	"population": "about 900",
	"government": "a merchant council",
	"services": ["inn", "market"],
	"defences": "a palisade and a watch of twelve",
	"climate": "temperate",
	"senses": ["gull noise", "damp wool"],
	"state": "flooding after the rains",
	"danger": 2,
	"private_truth": "the root's tendrils reach the town well"
}`

func buildLocationFixture(t *testing.T, s *Server) locationFixture {
	t.Helper()
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	mk := func(kind, name string) string {
		body := `{"kind":` + quote(kind) + `,"name":` + quote(name) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	town := mk("location", "Blackwater")
	monastery := mk("location", "Greyfall Monastery")
	metNPC := mk("npc", "Tom the Innkeeper")
	unmetNPC := mk("npc", "The Hermit of the Falls")
	key := mk("item", "The Silver Key")

	// The authored interior plus the route out, on the same payload: the
	// place block and the travel block are neighbours, not rivals.
	payload := `{"payload":{"place":` + placeBody + `,"travel":{"routes":[{"to":` + quote(monastery) + `,"days":1,"terrain":"road"}]}}}`
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/entities/"+town, payload, dm); r.Code != http.StatusOK {
		t.Fatalf("author the town: status %d, body %s", r.Code, r.Body)
	}

	// The monastery sits inside the town's bounds; the residents and the
	// key are placed here by edge.
	rel := func(from, relType, to string) {
		body := `{"from":` + quote(from) + `,"rel_type":` + quote(relType) + `,"to":` + quote(to) + `}`
		if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/relationships", body, dm); r.Code != http.StatusCreated {
			t.Fatalf("edge %s %s %s: status %d, body %s", from, relType, to, r.Code, r.Body)
		}
	}
	rel(monastery, "located_in", town)
	rel(metNPC, "located_in", town)
	rel(unmetNPC, "located_in", town)
	rel(key, "located_in", town)

	// Tom is met: a public fact about him the party holds.
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
	tomFact := mkFact(metNPC, "keeps", "the Waystone", "Tom the innkeeper keeps the Waystone in Blackwater.", "public")
	grant := `{"knower":"party","fact_id":` + quote(tomFact) + `,"stance":"knows"}`
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/awareness", grant, dm); r.Code != http.StatusOK {
		t.Fatalf("grant tom fact: status %d, body %s", r.Code, r.Body)
	}

	// A secret about the town — granted to the party, which must still not
	// reach a player dossier (the PlayerView rule).
	secret := mkFact(town, "hides", "a root-choked crypt", "Blackwater hides a root-choked crypt beneath the well.", "secret")
	grantSecret := `{"knower":"party","fact_id":` + quote(secret) + `,"stance":"knows"}`
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/awareness", grantSecret, dm); r.Code != http.StatusOK {
		t.Fatalf("grant secret: status %d, body %s", r.Code, r.Body)
	}

	// A witnessed event at the town (which is also how the party has met
	// the place itself).
	day := int64(40)
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID, `{"clock":`+day2json(day)+`}`, dm); r.Code != http.StatusOK {
		t.Fatalf("move the clock: status %d, body %s", r.Code, r.Body)
	}
	ev := `{"summary":"The party shelters in Blackwater from the floods.","clock_at":` + day2json(day) +
		`,"location_entity":` + quote(town) +
		`,"participants":[{"entity_id":` + quote(f.pcID) + `,"role":"party"}]}`
	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/events", ev, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create event: status %d, body %s", r.Code, r.Body)
	}
	eventID := idFrom(t, r, "event")

	// Quests sited here: one public, one secret (the default).
	machine := `{"initial":"start","states":[{"key":"start"},{"key":"done","terminal":"success"}],"edges":[{"from":"start","to":"done"}]}`
	mkQuest := func(name, visibility string) string {
		body := `{"name":` + quote(name) + `,"summary":"…","state_machine":` + machine
		if visibility != "" {
			body += `,"visibility":` + quote(visibility)
		}
		body += `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/quests", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create quest %s: status %d, body %s", name, r.Code, r.Body)
		}
		qid := idFrom(t, r, "quest")
		site := `{"entity_id":` + quote(town) + `,"role":"site"}`
		if rr := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/quests/"+qid+"/entities", site, dm); rr.Code != http.StatusCreated {
			t.Fatalf("site quest %s: status %d, body %s", name, rr.Code, rr.Body)
		}
		return qid
	}
	publicQuest := mkQuest("The Wrecked Caravan", "public")
	secretQuest := mkQuest("Silence the Hermit", "")

	return locationFixture{
		campaignID: f.campaignID, townID: town, monasteryID: monastery,
		metNPCID: metNPC, unmetNPCID: unmetNPC, secretFactID: secret,
		eventID: eventID, publicQuestID: publicQuest, secretQuestID: secretQuest,
	}
}

// TestLocationDossierDMIsCompleteAndWritesNothing: a location with routes,
// resident NPCs, a secret, an item, an event and sited quests renders a
// complete dossier with no new rows written anywhere — the entity row does
// not move across the read.
func TestLocationDossierDMIsCompleteAndWritesNothing(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildLocationFixture(t, s)
	dm := dmSession(t, s)

	before := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/entities/"+f.townID, "", dm)
	var beforeBody struct {
		Entity struct {
			UpdatedAt string         `json:"updated_at"`
			Payload   map[string]any `json:"payload"`
		} `json:"entity"`
	}
	if err := json.Unmarshal(before.Body.Bytes(), &beforeBody); err != nil {
		t.Fatal(err)
	}

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations/"+f.townID, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dossier: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Location struct {
			Place struct {
				Kind         string   `json:"kind"`
				Scale        string   `json:"scale"`
				Climate      string   `json:"climate"`
				Services     []string `json:"services"`
				PrivateTruth string   `json:"private_truth"`
			} `json:"place"`
			Present  []struct{ ID, Name string } `json:"present"`
			Children []struct{ ID string }       `json:"children"`
			Routes   []struct {
				To      string `json:"to"`
				Days    int64  `json:"days"`
				Terrain string `json:"terrain"`
			} `json:"routes"`
			Items   []struct{ ID string } `json:"items"`
			Secrets []struct{ ID string } `json:"secrets"`
			Events  []struct{ ID string } `json:"events"`
			Quests  []struct{ ID string } `json:"quests"`
			Rumours []string              `json:"rumours"`
		} `json:"location"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode dossier: %v (%s)", err, rec.Body)
	}
	l := body.Location
	if l.Place.Kind != "town" || l.Place.Scale != "large village" || l.Place.Climate != "temperate" {
		t.Fatalf("block: %+v", l.Place)
	}
	if l.Place.PrivateTruth != "the root's tendrils reach the town well" {
		t.Fatalf("the DM reads the private truth: %+v", l.Place)
	}
	if len(l.Routes) != 1 || l.Routes[0].To != f.monasteryID || l.Routes[0].Days != 1 || l.Routes[0].Terrain != "road" {
		t.Fatalf("routes: %+v", l.Routes)
	}
	if len(l.Present) != 2 || l.Present[0].Name != "The Hermit of the Falls" {
		t.Fatalf("present: %+v", l.Present)
	}
	if len(l.Children) != 1 || l.Children[0].ID != f.monasteryID {
		t.Fatalf("children: %+v", l.Children)
	}
	if len(l.Items) != 1 {
		t.Fatalf("items: %+v", l.Items)
	}
	if len(l.Secrets) != 1 || l.Secrets[0].ID != f.secretFactID {
		t.Fatalf("secrets: %+v", l.Secrets)
	}
	if len(l.Events) != 1 || l.Events[0].ID != f.eventID {
		t.Fatalf("events: %+v", l.Events)
	}
	if len(l.Quests) != 2 {
		t.Fatalf("quests (public and secret): %+v", l.Quests)
	}
	if len(l.Rumours) != 0 {
		t.Fatalf("rumours stay empty until MAD-374: %+v", l.Rumours)
	}

	// The read wrote nothing: the entity row did not move.
	after := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/entities/"+f.townID, "", dm)
	var afterBody struct {
		Entity struct {
			UpdatedAt string         `json:"updated_at"`
			Payload   map[string]any `json:"payload"`
		} `json:"entity"`
	}
	if err := json.Unmarshal(after.Body.Bytes(), &afterBody); err != nil {
		t.Fatal(err)
	}
	if afterBody.Entity.UpdatedAt != beforeBody.Entity.UpdatedAt {
		t.Fatalf("the location entity moved: %s -> %s", beforeBody.Entity.UpdatedAt, afterBody.Entity.UpdatedAt)
	}
	if len(afterBody.Entity.Payload) != len(beforeBody.Entity.Payload) {
		t.Fatal("the dossier must not touch the payload")
	}
}

// TestLocationDossierPlayerScopeLeaksNothing is the acceptance rule: no
// secret fact, no private truth, no unmet NPC — asserted on the raw body,
// the way the leak test asserts.
func TestLocationDossierPlayerScopeLeaksNothing(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildLocationFixture(t, s)
	player := addPlayerMember(t, s, fixture{campaignID: f.campaignID}, "thalia", false)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations/"+f.townID, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player dossier: status %d, body %s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	for _, forbidden := range []string{
		"private_truth", "the root's tendrils reach the town well",
		"root-choked crypt",       // the granted secret, still withheld
		"The Hermit of the Falls", // the unmet resident
		"Silence the Hermit",      // the secret quest
		"\"routes\"",              // the travel block is DM payload
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("LEAK: player dossier contains %q: %s", forbidden, raw)
		}
	}
	var body struct {
		Location struct {
			Place struct {
				Kind         string `json:"kind"`
				PrivateTruth string `json:"private_truth"`
			} `json:"place"`
			Present []struct{ ID string } `json:"present"`
			Quests  []struct{ ID string } `json:"quests"`
			Secrets []struct{ ID string } `json:"secrets"`
		} `json:"location"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Location.Place.Kind != "town" {
		t.Fatalf("the public half of the block crossed: %+v", body.Location.Place)
	}
	if len(body.Location.Present) != 1 || body.Location.Present[0].ID != f.metNPCID {
		t.Fatalf("only the met resident is present: %+v", body.Location.Present)
	}
	if len(body.Location.Quests) != 1 || body.Location.Quests[0].ID != f.publicQuestID {
		t.Fatalf("only the public quest is sited: %+v", body.Location.Quests)
	}
	if len(body.Location.Secrets) != 0 {
		t.Fatalf("LEAK: secrets in the player dossier: %+v", body.Location.Secrets)
	}

	// An unmet location is indistinguishable from a missing one.
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations/"+f.monasteryID, "", player); r.Code != http.StatusNotFound {
		t.Fatalf("unmet location: status %d, body %s", r.Code, r.Body)
	}
}

// TestPlaceBlockRoundTrip: PUT the block, read it back whole; the travel
// block beside it survives. Refusals: a player cannot write it, and an
// entity that is not a location is a 400 on both read and write.
func TestPlaceBlockRoundTrip(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildLocationFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, fixture{campaignID: f.campaignID}, "bran", false)

	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/locations/"+f.townID+"/place", placeBody, player); r.Code != http.StatusForbidden {
		t.Fatalf("player place write: status %d", r.Code)
	}

	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/locations/"+f.townID+"/place", placeBody, dm); r.Code != http.StatusOK {
		t.Fatalf("place write: status %d, body %s", r.Code, r.Body)
	}
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations/"+f.townID, "", dm)
	var body struct {
		Location struct {
			Place struct {
				Kind     string   `json:"kind"`
				Services []string `json:"services"`
				Danger   int      `json:"danger"`
				Senses   []string `json:"senses"`
			} `json:"place"`
			Routes []struct{ To string } `json:"routes"`
		} `json:"location"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Location.Place.Kind != "town" || len(body.Location.Place.Services) != 2 ||
		body.Location.Place.Danger != 2 || len(body.Location.Place.Senses) != 2 {
		t.Fatalf("round trip lost the block: %+v", body.Location.Place)
	}
	if len(body.Location.Routes) != 1 {
		t.Fatalf("the travel block must survive a place write: %+v", body.Location.Routes)
	}

	// A malformed block on the entity (hand-edited payload) reads as the
	// zero block, not an error.
	garbage := `{"payload":{"place":"nonsense"}}`
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/entities/"+f.townID, garbage, dm); r.Code != http.StatusOK {
		t.Fatalf("plant garbage: status %d, body %s", r.Code, r.Body)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations/"+f.townID, "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"place":{`) {
		t.Fatalf("malformed block yields the zero block: status %d, body %s", rec.Code, rec.Body)
	}
	var zero struct {
		Location struct {
			Place map[string]any `json:"place"`
		} `json:"location"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &zero); err != nil {
		t.Fatal(err)
	}
	if v, ok := zero.Location.Place["private_truth"].(string); ok && v != "" {
		t.Fatalf("the zero block carries no private truth: %+v", zero.Location.Place)
	}
	if v, _ := zero.Location.Place["kind"].(string); v != "" {
		t.Fatalf("the malformed block read as the zero block: %+v", zero.Location.Place)
	}

	// Not a location: both surfaces refuse.
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations/"+f.metNPCID, "", dm); r.Code != http.StatusBadRequest {
		t.Fatalf("npc read: status %d", r.Code)
	}
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/locations/"+f.metNPCID+"/place", placeBody, dm); r.Code != http.StatusBadRequest {
		t.Fatalf("npc write: status %d", r.Code)
	}
}

// TestLocationsListing: the campaign's places with scale and a live child
// count; a player's listing carries no payloads.
func TestLocationsListing(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildLocationFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, fixture{campaignID: f.campaignID}, "mira", false)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("listing: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Locations []struct {
			ID       string `json:"id"`
			Scale    string `json:"scale"`
			Children int    `json:"children"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var town *struct {
		ID       string `json:"id"`
		Scale    string `json:"scale"`
		Children int    `json:"children"`
	}
	for i := range body.Locations {
		if body.Locations[i].ID == f.townID {
			town = &body.Locations[i]
		}
	}
	if town == nil {
		t.Fatalf("the town is in the listing: %+v", body.Locations)
	}
	if town.Scale != "large village" || town.Children != 1 {
		t.Fatalf("scale and child count: %+v", town)
	}

	playerList := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/locations", "", player)
	if playerList.Code != http.StatusOK || !strings.Contains(playerList.Body.String(), "Blackwater") {
		t.Fatalf("player listing: status %d, body %s", playerList.Code, playerList.Body)
	}
	if strings.Contains(playerList.Body.String(), "\"payload\"") {
		t.Fatalf("player listing must carry no payloads: %s", playerList.Body)
	}
}

// TestLocationSummaryIsSearchable: the description lives in entities.summary
// — the one place the prose index reads — and is findable through campaign
// search the moment it is written there.
func TestLocationSummaryIsSearchable(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildLocationFixture(t, s)
	dm := dmSession(t, s)

	patch := `{"summary":"A market town of gulls, damp wool and salt-crusted pilings."}`
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/entities/"+f.townID, patch, dm); r.Code != http.StatusOK {
		t.Fatalf("write the summary: status %d, body %s", r.Code, r.Body)
	}
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/search?q=gulls", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("search: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Hits []struct {
			Kind  string
			RefID string
		} `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range body.Hits {
		if h.Kind == "entity" && h.RefID == f.townID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the summary must be findable: %s", rec.Body)
	}
}
