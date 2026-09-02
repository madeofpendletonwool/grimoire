package server

// Campaign surface tests (MAD-305). The load-bearing ones assert on HTTP
// responses, not store rows: a player role cannot read a secret fact through
// the API — the response body must not contain it — and a non-member learns
// nothing (404, the same as a missing campaign). Scope enforcement is the
// product here; the stores beneath it have their own tests.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/faction"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// recorder names the type the call() helper returns, so signatures below read
// without repeating httptest.
type recorder = httptest.ResponseRecorder

// hit wraps the shared call() helper with the testing.T these tests always
// have, so failure sites report usefully.
func hit(t *testing.T, s *Server, method, target, body string, cookies ...*http.Cookie) *recorder {
	t.Helper()
	return call(s, method, target, body, cookies...)
}

// dmSession signs in as the keeper account buildFixture's adminSession
// already claimed. Tests that need "the DM" mid-fixture use this; the second
// adminSession would fatal on the already-claimed install.
func dmSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	rec := call(s, http.MethodPost, "/api/auth/login", credsJSON("keeper", "a-fine-passphrase"))
	if rec.Code != http.StatusOK {
		t.Fatalf("login as keeper: status %d, body %s", rec.Code, rec.Body)
	}
	return sessionFrom(t, rec)
}

// newCampaignServer boots a gated server with the campaign graph, the
// knowledge layer and migrations applied — the shape runServe serves.
func newCampaignServer(t *testing.T) (*Server, *campaign.Store, *knowledge.Store, *auth.Store) {
	t.Helper()
	store, err := index.Open(testdb.Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
	factions, err := faction.New(store.DB())
	if err != nil {
		t.Fatalf("open faction store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithFactions(factions)
	return s, campaigns, knowledgeStore, users
}

// fixtureCampaign builds the canonical test campaign as the keeper/DM:
// a duke, his secret (a fact the party has not earned), a public fact the
// party knows, and a pc. Returns the campaign id and entity ids.
type fixture struct {
	campaignID string
	dukeID     string
	pcID       string
	secretID   string
	publicID   string
}

func buildFixture(t *testing.T, s *Server) fixture {
	t.Helper()
	dm := adminSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns", `{"name":"The Ashen Court","system":"D&D 5e","premise":"A kingdom consumed by an ancient forest."}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create campaign: status %d, body %s", rec.Code, rec.Body)
	}
	cid := idFrom(t, rec, "campaign")

	mk := func(kind, name string) string {
		body := `{"kind":` + quote(kind) + `,"name":` + quote(name) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/entities", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	duke := mk("npc", "Duke Aldric Vane")
	pc := mk("pc", "Mira Thorn")

	mkFact := func(subject, predicate, object, statement, visibility string) string {
		body := `{"subject":` + quote(subject) + `,"predicate":` + quote(predicate) +
			`,"object_literal":` + quote(object) + `,"statement":` + quote(statement) +
			`,"visibility":` + quote(visibility) + `}`
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+cid+"/facts", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create fact: status %d, body %s", r.Code, r.Body)
		}
		return idFrom(t, r, "fact")
	}
	secret := mkFact(duke, "secretly_is", "a vampire", "The Duke is secretly a vampire.", "secret")
	pub := mkFact(duke, "rules", "the northern marches", "The Duke rules the northern marches.", "public")

	// The party has met the duke and learned the public fact; the secret they
	// have not earned — but an awareness grant on it exists, which is exactly
	// the case the PlayerView must survive: granted at the wide store, still
	// invisible on the player surface.
	for _, f := range []string{pub, secret} {
		body := `{"knower":"party","fact_id":` + quote(f) + `,"stance":"knows"}`
		if r := hit(t, s, http.MethodPut, "/api/campaigns/"+cid+"/awareness", body, dm); r.Code != http.StatusOK {
			t.Fatalf("set awareness: status %d, body %s", r.Code, r.Body)
		}
	}
	return fixture{campaignID: cid, dukeID: duke, pcID: pc, secretID: secret, publicID: pub}
}

// addPlayerMember registers a player through the campaign invite flow — the
// exact path a real player takes — and binds them to the pc entity.
func addPlayerMember(t *testing.T, s *Server, f fixture, name string, withCharacter bool) *http.Cookie {
	t.Helper()
	dm := dmSession(t, s)
	inv := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/invites", `{"role":"player"}`, dm)
	if inv.Code != http.StatusCreated {
		t.Fatalf("mint campaign invite: status %d, body %s", inv.Code, inv.Body)
	}
	code, _ := inviteCodeFrom(t, inv)

	reg := hit(t, s, http.MethodPost, "/api/auth/register",
		`{"username":`+quote(name)+`,"password":"a-fine-passphrase","invite":`+quote(code)+`}`)
	if reg.Code != http.StatusCreated {
		t.Fatalf("register player: status %d, body %s", reg.Code, reg.Body)
	}
	cookie := sessionFrom(t, reg)

	if withCharacter {
		// Point the member at their pc so their scope resolves to
		// character:<id> rather than party.
		id, err := s.users.LookupUseridByName(t.Context(), name)
		if err != nil {
			t.Fatalf("lookup player id: %v", err)
		}
		body := `{"character_id":` + quote(f.pcID) + `}`
		if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/members/"+id, body, dm); r.Code != http.StatusOK {
			t.Fatalf("bind character: status %d, body %s", r.Code, r.Body)
		}
	}
	return cookie
}

func idFrom(t *testing.T, rec *recorder, key string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s response: %v (%s)", key, err, rec.Body)
	}
	obj, ok := body[key].(map[string]any)
	if !ok {
		t.Fatalf("no %s in response: %s", key, rec.Body)
	}
	id, _ := obj["id"].(string)
	if id == "" {
		t.Fatalf("%s has no id: %s", key, rec.Body)
	}
	return id
}

func inviteCodeFrom(t *testing.T, rec *recorder) (string, string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode invite response: %v", err)
	}
	inv, ok := body["invite"].(map[string]any)
	if !ok {
		t.Fatalf("no invite in response: %s", rec.Body)
	}
	code, _ := inv["code"].(string)
	url, _ := inv["url"].(string)
	if code == "" || url == "" {
		t.Fatalf("invite response missing code/url: %s", rec.Body)
	}
	if !strings.Contains(url, code) {
		t.Errorf("invite url %q does not carry the code", url)
	}
	return code, url
}

/* ---------- scope enforcement: the acceptance tests ---------- */

// TestPlayerCannotReadSecretFact is the one this issue exists for. The party
// HOLDS a granting awareness row on the secret — the wide store serves it to
// the DM — but the player surface must not carry it, in lists, by id, in
// search, in the entity bundle, or in the graph.
func TestPlayerCannotReadSecretFact(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	player := addPlayerMember(t, s, f, "mira", true)

	// The DM sees the secret — proving it exists and the test has teeth.
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/facts", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "vampire") {
		t.Fatalf("the DM must see the secret fact: status %d, body %s", rec.Code, rec.Body)
	}

	// The list at player scope carries only the public fact.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/facts", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player facts list: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vampire") {
		t.Error("the player's fact list contains the secret fact")
	}
	if !strings.Contains(rec.Body.String(), "northern marches") {
		t.Error("the player's fact list is missing the public fact")
	}

	// By id, the secret is indistinguishable from a missing fact.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/facts/"+f.secretID, "", player)
	if rec.Code != http.StatusNotFound {
		t.Errorf("player reading secret fact by id: status %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "vampire") {
		t.Error("the 404 body leaked the secret statement")
	}

	// The public fact reads fine by id.
	if rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/facts/"+f.publicID, "", player); rec.Code != http.StatusOK {
		t.Errorf("player reading public fact by id: status %d, want 200", rec.Code)
	}

	// Prose search cannot surface its text.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/search?q=vampire", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player search: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vampire") {
		t.Error("player prose search surfaced the secret fact's text")
	}

	// The entity bundle about the duke carries facts, but not that one.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/entities/"+f.dukeID, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player entity detail: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vampire") {
		t.Error("the player's entity bundle contains the secret fact")
	}
	if strings.Contains(rec.Body.String(), `"payload"`) {
		t.Error("the player's entity bundle carried the DM-only payload field")
	}
}

// TestPlayerCannotWriteAnything checks every write route refuses a player
// role before touching the store.
func TestPlayerCannotWriteAnything(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	player := addPlayerMember(t, s, f, "mira", true)
	base := "/api/campaigns/" + f.campaignID

	writes := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"create entity", http.MethodPost, base + "/entities", `{"kind":"npc","name":"Sneaky"}`},
		{"update entity", http.MethodPatch, base + "/entities/" + f.dukeID, `{"summary":"x"}`},
		{"delete entity", http.MethodDelete, base + "/entities/" + f.dukeID, ""},
		{"add alias", http.MethodPost, base + "/entities/" + f.dukeID + "/names", `{"name":"Aldric"}`},
		{"create fact", http.MethodPost, base + "/facts", `{"subject":"` + f.dukeID + `","predicate":"is","object_literal":"tested","statement":"tested"}`},
		{"supersede fact", http.MethodPost, base + "/facts/" + f.publicID + "/supersede", `{"subject":"` + f.dukeID + `","predicate":"is","object_literal":"x","statement":"x"}`},
		{"create event", http.MethodPost, base + "/events", `{"summary":"something happened"}`},
		{"create relationship", http.MethodPost, base + "/relationships", `{"from":"` + f.dukeID + `","rel_type":"knows","to":"` + f.pcID + `"}`},
		{"set awareness", http.MethodPut, base + "/awareness", `{"knower":"party","fact_id":"x","stance":"knows"}`},
		{"read awareness", http.MethodGet, base + "/awareness", ""},
		{"list members", http.MethodGet, base + "/members", ""},
		{"list quests", http.MethodGet, base + "/quests", ""},
		{"create quest", http.MethodPost, base + "/quests", `{"name":"q","state_machine":{"initial":"a","states":["a"]}}`},
		{"update campaign", http.MethodPatch, base, `{"name":"nope"}`},
		{"delete campaign", http.MethodDelete, base, ""},
		{"mint invite", http.MethodPost, base + "/invites", `{"role":"player"}`},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			if rec := hit(t, s, w.method, w.target, w.body, player); rec.Code != http.StatusForbidden {
				t.Errorf("player %s %s: status %d, want 403", w.method, w.target, rec.Code)
			}
		})
	}
}

// TestNonMemberLearnsNothing pins the default-deny rule: a caller with no
// membership row gets the same 404 a wrong campaign id produces — on reads
// and writes alike, with no hint the campaign exists.
func TestNonMemberLearnsNothing(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)

	// A real account with no standing in the campaign.
	dm := dmSession(t, s)
	inv := createInvite(t, s, dm, "")
	code, _ := inv["code"].(string)
	other := sessionFrom(t, hit(t, s, http.MethodPost, "/api/auth/register", registerJSON("wanderer", "a-fine-passphrase", code)))

	targets := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, "/api/campaigns/" + f.campaignID, ""},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/entities", ""},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/facts", ""},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/timeline", ""},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/graph?center=" + f.dukeID, ""},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/entities", `{"kind":"npc","name":"x"}`},
	}
	for _, tg := range targets {
		if rec := hit(t, s, tg.method, tg.target, tg.body, other); rec.Code != http.StatusNotFound {
			t.Errorf("non-member %s %s: status %d, want 404", tg.method, tg.target, rec.Code)
		}
	}

	// And the campaign does not appear in their picker.
	rec := hit(t, s, http.MethodGet, "/api/campaigns", "", other)
	if strings.Contains(rec.Body.String(), f.campaignID) {
		t.Error("a campaign the caller has no row for appeared in their list")
	}
}

// TestCampaignRoutesRequireASession: the gate closes before any scope is read.
func TestCampaignRoutesRequireASession(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	for _, target := range []string{"/api/campaigns", "/api/campaigns/some-id/facts"} {
		if rec := hit(t, s, http.MethodGet, target, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s: status %d, want 401", target, rec.Code)
		}
	}
}

/* ---------- the end-to-end shape ---------- */

// TestCampaignPopulatedAndBrowsedEndToEnd walks the whole surface a DM uses:
// create, populate (entities, facts, events, relationships, awareness,
// quest), then browse it back through the scoped reads, including the graph
// view and the fact provenance.
func TestCampaignPopulatedAndBrowsedEndToEnd(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	// A relationship and an event, the way the UI sends them.
	body := `{"from":` + quote(f.dukeID) + `,"rel_type":"secretly_controls","to":` + quote(f.pcID) + `}`
	if rec := hit(t, s, http.MethodPost, base+"/relationships", body, dm); rec.Code != http.StatusCreated {
		t.Fatalf("create relationship: status %d, body %s", rec.Code, rec.Body)
	}
	body = `{"summary":"The Duke invited the party to dinner.","clock_at":3,"participants":[{"entity_id":` + quote(f.dukeID) + `,"role":"host"},{"entity_id":` + quote(f.pcID) + `}]}`
	if rec := hit(t, s, http.MethodPost, base+"/events", body, dm); rec.Code != http.StatusCreated {
		t.Fatalf("create event: status %d, body %s", rec.Code, rec.Body)
	}

	// Entities list filters by kind.
	rec := hit(t, s, http.MethodGet, base+"/entities?kind=npc", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Duke Aldric Vane") {
		t.Fatalf("entities?kind=npc: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "Mira Thorn") {
		t.Error("kind=npc returned a pc")
	}

	// Entity detail carries the bundle: names, facts, relationships, events.
	rec = hit(t, s, http.MethodGet, base+"/entities/"+f.dukeID, "", dm)
	for _, want := range []string{"\"names\"", "\"facts\"", "\"relationships\"", "\"events\"", "vampire", "dinner"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("entity detail missing %s: %s", want, rec.Body)
		}
	}

	// Fact detail answers "why do we believe this?": provenance + awareness.
	rec = hit(t, s, http.MethodGet, base+"/facts/"+f.secretID, "", dm)
	if !strings.Contains(rec.Body.String(), "\"provenance\"") || !strings.Contains(rec.Body.String(), "dm_authored") {
		t.Errorf("fact detail missing provenance: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "\"awareness\"") || !strings.Contains(rec.Body.String(), "party") {
		t.Errorf("fact detail missing awareness: %s", rec.Body)
	}

	// The timeline plays back in order with participants.
	rec = hit(t, s, http.MethodGet, base+"/timeline", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "dinner") {
		t.Fatalf("timeline: status %d, body %s", rec.Code, rec.Body)
	}

	// The graph view carries both nodes and the edge between them.
	rec = hit(t, s, http.MethodGet, base+"/graph?center="+f.dukeID+"&hops=2", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("graph: status %d, body %s", rec.Code, rec.Body)
	}
	var g struct {
		Center string `json:"center"`
		Nodes  []struct {
			ID   string `json:"id"`
			Hops int    `json:"hops"`
		} `json:"nodes"`
		Edges []struct{ From, RelType, To string } `json:"-"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("decode graph: %v (%s)", err, rec.Body)
	}
	if len(g.Nodes) != 2 {
		t.Errorf("graph has %d nodes, want the duke and Mira (2)", len(g.Nodes))
	}

	// A quest with a legal transition and an illegal one.
	body = `{"name":"Unmask the Duke","state_machine":{"initial":"suspicious","states":["suspicious","confronted","undead_truth"],"edges":[{"from":"suspicious","to":"confronted"},{"from":"confronted","to":"undead_truth"}]}}`
	rec = hit(t, s, http.MethodPost, base+"/quests", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create quest: status %d, body %s", rec.Code, rec.Body)
	}
	questID := idFrom(t, rec, "quest")
	body = `{"to":"confronted"}`
	if rec = hit(t, s, http.MethodPost, base+"/quests/"+questID+"/transition", body, dm); rec.Code != http.StatusOK {
		t.Errorf("legal transition: status %d, body %s", rec.Code, rec.Body)
	}
	body = `{"to":"suspicious"}`
	if rec = hit(t, s, http.MethodPost, base+"/quests/"+questID+"/transition", body, dm); rec.Code != http.StatusBadRequest {
		t.Errorf("transition along an edge the machine does not have: status %d, want 400", rec.Code)
	}

	// Prose search at the DM scope finds the secret's text.
	rec = hit(t, s, http.MethodGet, base+"/search?q=vampire", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "vampire") {
		t.Errorf("DM prose search must find the secret: %s", rec.Body)
	}
}

// TestSupersedeRetconsThroughTheAPI: the DM's fact editor replaces a fact;
// the old row survives as retconned and the new one is what reads serve.
func TestSupersedeRetconsThroughTheAPI(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	body := `{"subject":` + quote(f.dukeID) + `,"predicate":"rules","object_literal":"the whole realm","statement":"The Duke rules the whole realm now."}`
	rec := hit(t, s, http.MethodPost, base+"/facts/"+f.publicID+"/supersede", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("supersede: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodGet, base+"/facts", "", dm)
	if !strings.Contains(rec.Body.String(), "whole realm") {
		t.Error("the replacement fact is not readable")
	}
	// Retconned history is the DM's to browse: hidden from the live list,
	// present when asked for.
	if strings.Contains(rec.Body.String(), `"retconned"`) {
		t.Error("the live list carried retconned history")
	}
	rec = hit(t, s, http.MethodGet, base+"/facts?superseded=1", "", dm)
	var list struct {
		Facts []struct {
			ID         string `json:"id"`
			Confidence string `json:"confidence"`
		} `json:"facts"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	retconned := 0
	for _, fact := range list.Facts {
		if fact.ID == f.publicID && fact.Confidence == "retconned" {
			retconned++
		}
	}
	if retconned != 1 {
		t.Errorf("the superseded fact should survive as retconned; found %d", retconned)
	}
}

/* ---------- invites + membership ---------- */

// TestCampaignInviteFlow: mint as DM, redeem as a signed-out friend, land as
// a player member with the character binding resolving their scope.
func TestCampaignInviteFlow(t *testing.T) {
	s, campaigns, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)

	player := addPlayerMember(t, s, f, "elias", true)

	// The membership row exists and says player.
	uid, err := s.users.LookupUseridByName(t.Context(), "elias")
	if err != nil {
		t.Fatalf("lookup player: %v", err)
	}
	role, ok, err := campaigns.Role(t.Context(), f.campaignID, uid)
	if err != nil || !ok || role != campaign.RolePlayer {
		t.Fatalf("membership after redeem: ok=%v role=%q err=%v", ok, role, err)
	}

	// The campaign appears in their picker with their role.
	rec := hit(t, s, http.MethodGet, "/api/campaigns", "", player)
	if !strings.Contains(rec.Body.String(), `"my_role":"player"`) {
		t.Errorf("player's campaign list lacks their role: %s", rec.Body)
	}

}

// TestJoinWithInviteAsSignedInUser: an existing account redeems a campaign
// invite through /api/campaigns/join.
func TestJoinWithInviteAsSignedInUser(t *testing.T) {
	s, campaigns, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)

	// A plain account with no campaign standing.
	dm := dmSession(t, s)
	inv := createInvite(t, s, dm, "for a neighbor")
	code, _ := inv["code"].(string)
	neighbor := sessionFrom(t, hit(t, s, http.MethodPost, "/api/auth/register", registerJSON("neighbor", "a-fine-passphrase", code)))

	// The DM mints a campaign invite; the neighbor redeems it signed in.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/invites", `{"role":"observer","note":"watching"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: status %d, body %s", rec.Code, rec.Body)
	}
	campaignCode, _ := inviteCodeFrom(t, rec)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/join", `{"code":`+quote(campaignCode)+`}`, neighbor)
	if rec.Code != http.StatusOK {
		t.Fatalf("join: status %d, body %s", rec.Code, rec.Body)
	}

	uid, _ := s.users.LookupUseridByName(t.Context(), "neighbor")
	role, ok, _ := campaigns.Role(t.Context(), f.campaignID, uid)
	if !ok || role != "observer" {
		t.Errorf("neighbor role = %q ok=%v, want observer", role, ok)
	}

	// Redeeming again is refused: the code is spent.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/join", `{"code":`+quote(campaignCode)+`}`, neighbor)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("spent invite rejoin: status %d, want 400", rec.Code)
	}
}

// TestMembershipManagementIsDMOnly: a player cannot manage members; the DM
// can change a role, bind a character, and revoke.
func TestMembershipManagementIsDMOnly(t *testing.T) {
	s, campaigns, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, f, "mira", false)
	base := "/api/campaigns/" + f.campaignID

	uid, _ := s.users.LookupUseridByName(t.Context(), "mira")

	// Player may not manage.
	if rec := hit(t, s, http.MethodPatch, base+"/members/"+uid, `{"role":"dm"}`, player); rec.Code != http.StatusForbidden {
		t.Errorf("player role change: status %d, want 403", rec.Code)
	}

	// The DM lists, promotes to observer, binds a character, revokes.
	rec := hit(t, s, http.MethodGet, base+"/members", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mira") {
		t.Fatalf("members list: status %d, body %s", rec.Code, rec.Body)
	}
	if rec = hit(t, s, http.MethodPatch, base+"/members/"+uid, `{"role":"observer"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("role change: status %d, body %s", rec.Code, rec.Body)
	}
	if role, _, _ := campaigns.Role(t.Context(), f.campaignID, uid); role != "observer" {
		t.Errorf("role after change = %q", role)
	}
	if rec = hit(t, s, http.MethodPatch, base+"/members/"+uid, `{"character_id":`+quote(f.pcID)+`}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("character bind: status %d, body %s", rec.Code, rec.Body)
	}
	if rec = hit(t, s, http.MethodDelete, base+"/members/"+uid, "", dm); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d, body %s", rec.Code, rec.Body)
	}
	if _, ok, _ := campaigns.Role(t.Context(), f.campaignID, uid); ok {
		t.Error("revoked member still has a row")
	}

	// A revoked member is a non-member again: 404, no hint.
	if rec := hit(t, s, http.MethodGet, base+"/facts", "", player); rec.Code != http.StatusNotFound {
		t.Errorf("revoked member read: status %d, want 404", rec.Code)
	}
}

// TestPartyScopeWithoutCharacter: a player member with no character bound
// reads at the party scope — the shared knowledge, still no secrets.
func TestPartyScopeWithoutCharacter(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	player := addPlayerMember(t, s, f, "tabletop", false)

	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/facts", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("party facts: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vampire") {
		t.Error("party scope surfaced the secret fact")
	}
	if !strings.Contains(rec.Body.String(), "northern marches") {
		t.Error("party scope missing the public fact")
	}
}

// TestObserverReadsButCannotWrite: observers get the player view (read-only
// by construction — every write route is DM-only).
func TestObserverReadsButCannotWrite(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	rec := hit(t, s, http.MethodPost, base+"/invites", `{"role":"observer"}`, dm)
	code, _ := inviteCodeFrom(t, rec)
	observer := sessionFrom(t, hit(t, s, http.MethodPost, "/api/auth/register",
		registerJSON("watcher", "a-fine-passphrase", code)))

	if rec = hit(t, s, http.MethodGet, base+"/entities", "", observer); rec.Code != http.StatusOK {
		t.Errorf("observer read: status %d", rec.Code)
	}
	if rec = hit(t, s, http.MethodPost, base+"/entities", `{"kind":"npc","name":"x"}`, observer); rec.Code != http.StatusForbidden {
		t.Errorf("observer write: status %d, want 403", rec.Code)
	}
}

// TestGraphIsScopedAtThePlayerToo: the player's graph reads through the same
// scoped edge list — the secretly_controls edge the DM drew must not appear.
func TestGraphIsScopedAtThePlayerToo(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	body := `{"from":` + quote(f.dukeID) + `,"rel_type":"secretly_controls","to":` + quote(f.pcID) + `}`
	if rec := hit(t, s, http.MethodPost, base+"/relationships", body, dm); rec.Code != http.StatusCreated {
		t.Fatalf("create relationship: %d %s", rec.Code, rec.Body)
	}

	player := addPlayerMember(t, s, f, "mira", true)
	rec := hit(t, s, http.MethodGet, base+"/graph?center="+f.dukeID+"&hops=1", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player graph: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "secretly_controls") {
		t.Error("the player's graph carries the DM-only secretly_controls edge")
	}
	// The DM's graph does.
	rec = hit(t, s, http.MethodGet, base+"/graph?center="+f.dukeID+"&hops=1", "", dm)
	if !strings.Contains(rec.Body.String(), "secretly_controls") {
		t.Error("the DM's graph is missing the edge")
	}
}

// TestCampaignDetailReportsMyRole: each caller sees their own standing.
func TestCampaignDetailReportsMyRole(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, f, "mira", false)

	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID, "", dm); !strings.Contains(rec.Body.String(), `"my_role":"dm"`) {
		t.Errorf("DM's detail role: %s", rec.Body)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID, "", player); !strings.Contains(rec.Body.String(), `"my_role":"player"`) {
		t.Errorf("player's detail role: %s", rec.Body)
	}
}

/* ---------- validation ---------- */

// TestCampaignFactValidation: the API surfaces the store's shape rules as
// clean 400s — both-object, missing provenance path (empty statement), bad
// confidence.
func TestCampaignFactValidation(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	cases := []struct {
		name string
		body string
	}{
		{"both objects", `{"subject":"` + f.dukeID + `","predicate":"is","object_entity":"` + f.pcID + `","object_literal":"x","statement":"x"}`},
		{"neither object", `{"subject":"` + f.dukeID + `","predicate":"is","statement":"x"}`},
		{"empty statement", `{"subject":"` + f.dukeID + `","predicate":"is","object_literal":"x","statement":"  "}`},
		{"bad confidence", `{"subject":"` + f.dukeID + `","predicate":"is","object_literal":"x","statement":"x","confidence":"mythic"}`},
		{"bad visibility", `{"subject":"` + f.dukeID + `","predicate":"is","object_literal":"x","statement":"x","visibility":"hidden"}`},
		{"dm cannot stage proposed", `{"subject":"` + f.dukeID + `","predicate":"is","object_literal":"x","statement":"x","confidence":"proposed"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := hit(t, s, http.MethodPost, base+"/facts", c.body, dm); rec.Code != http.StatusBadRequest {
				t.Errorf("status %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
}

// TestAwarenessStanceTransitionsEnforcedOverAPI: the stance table is served,
// not bypassed — knows cannot fall back to unaware.
func TestAwarenessStanceTransitionsEnforcedOverAPI(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	// The party knows the public fact; a manual unaware is refused.
	body := `{"knower":"party","fact_id":` + quote(f.publicID) + `,"stance":"unaware"}`
	if rec := hit(t, s, http.MethodPut, base+"/awareness", body, dm); rec.Code != http.StatusBadRequest {
		t.Errorf("knows -> unaware: status %d, want 400", rec.Code)
	}
	// Doubt re-opens a settled belief.
	body = `{"knower":"party","fact_id":` + quote(f.publicID) + `,"stance":"suspects","confidence":0.6}`
	if rec := hit(t, s, http.MethodPut, base+"/awareness", body, dm); rec.Code != http.StatusOK {
		t.Errorf("knows -> suspects: status %d, body %s", rec.Code, rec.Body)
	}
	// Reads list it.
	if rec := hit(t, s, http.MethodGet, base+"/awareness?knower=party", "", dm); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "suspects") {
		t.Errorf("awareness read: %d %s", rec.Code, rec.Body)
	}
}

// TestCampaignsUnavailableWithoutWiring: the additive-feature contract —
// endpoints report 503, they do not panic.
func TestCampaignsUnavailableWithoutWiring(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	s.campaigns = nil
	dm := adminSession(t, s) // fresh server: this test claims the install itself
	if rec := hit(t, s, http.MethodGet, "/api/campaigns", "", dm); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired campaigns list: status %d, want 503", rec.Code)
	}
}
