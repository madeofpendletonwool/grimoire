package server

// NPC simulation tests (MAD-313). The load-bearing one is the acceptance
// test: an NPC answer's assembled context — the exact request body sent to
// the model — contains no fact outside that NPC's awareness. The Duke does
// not know the party holds the second relic unless a fact says he does, and
// the model cannot leak it, because it is never retrieved.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// jsonLLM is a stubbed Anthropic-compatible endpoint for the non-streaming
// AnswerPrompt path: it records every request body so tests can assert on
// the assembled prompt, and replies with one fixed text block.
type jsonLLM struct {
	mu      sync.Mutex
	bodies  []string
	baseURL string
}

func newJSONLLM(t *testing.T, answer string) *jsonLLM {
	t.Helper()
	c := &jsonLLM{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(raw))
		c.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"content":[{"type":"text","text":%s}]}`, mustQuote(answer))
	}))
	t.Cleanup(up.Close)
	c.baseURL = up.URL
	return c
}

func mustQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (c *jsonLLM) lastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return c.bodies[len(c.bodies)-1]
}

// newNPCAskServer boots a gated server with the campaign stack, the canon
// engine, and a stubbed LLM.
func newNPCAskServer(t *testing.T, stub *jsonLLM) (*Server, *fixture) {
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
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	cfg := llm.Config{BaseURL: stub.baseURL, APIKey: "test-key", Model: "test-model"}
	s, err := New(store, llm.New(cfg), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine)
	f := buildFixture(t, s)
	return s, &f
}

// grantDuke gives the duke npc a knows-awareness row on one fact.
func grantDuke(t *testing.T, s *Server, f fixture, factID string) {
	t.Helper()
	dm := dmSession(t, s)
	body := `{"knower":` + quote(f.dukeID) + `,"fact_id":` + quote(factID) + `,"stance":"knows"}`
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/awareness", body, dm); r.Code != http.StatusOK {
		t.Fatalf("grant duke awareness: status %d, body %s", r.Code, r.Body)
	}
}

// askAs posts one question to the npc ask endpoint and decodes the response.
// A nil cookie sends the request unauthenticated.
func askAs(t *testing.T, s *Server, f fixture, cookie *http.Cookie, npcID, body string) (int, map[string]any) {
	t.Helper()
	var rec *recorder
	if cookie == nil {
		rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/npc/"+npcID+"/ask", body)
	} else {
		rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/npc/"+npcID+"/ask", body, cookie)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode ask response: %v (%s)", err, rec.Body)
	}
	return rec.Code, out
}

// THE ACCEPTANCE TEST: granted facts ride the prompt, ungranted ones never
// appear in it — not the secret, not the party's knowledge, nothing.
func TestNPCAskPromptContainsOnlyGrantedFacts(t *testing.T) {
	stub := newJSONLLM(t, "REACTION — the Duke acts.\n\nIN-VOICE — \"Hm.\"")
	s, f := newNPCAskServer(t, stub)
	grantDuke(t, s, *f, f.publicID) // the Duke knows what he rules; NOT his secret

	code, resp := askAs(t, s, *f, dmSession(t, s), f.dukeID, `{"question":"What will you do about the unrest?"}`)
	if code != http.StatusOK {
		t.Fatalf("ask: status %d, body %v", code, resp)
	}
	body := stub.lastBody()
	if body == "" {
		t.Fatalf("the model was never called")
	}
	if !strings.Contains(body, "The Duke rules the northern marches.") {
		t.Errorf("the granted fact must be in the assembled context:\n%s", body)
	}
	if strings.Contains(body, "vampire") || strings.Contains(body, "secretly_is") {
		t.Errorf("the ungranted secret leaked into the NPC context:\n%s", body)
	}
	if strings.Contains(body, "Mira Thorn") {
		t.Errorf("an entity the NPC has no awareness of leaked into the context:\n%s", body)
	}
	if !strings.Contains(body, "Duke Aldric Vane") || !strings.Contains(body, "WHAT DUKE ALDRIC VANE KNOWS") {
		t.Errorf("the mind/record framing is missing:\n%s", body)
	}
	// Citations carry exactly the granted fact.
	cites, _ := resp["citations"].(map[string]any)
	facts, _ := cites["facts"].([]any)
	if len(facts) != 1 {
		t.Fatalf("citations.facts: got %d items, want 1 (%v)", len(facts), cites)
	}
	got, _ := facts[0].(map[string]any)
	if got["id"] != f.publicID {
		t.Errorf("cited fact %v, want the granted public fact %s", got["id"], f.publicID)
	}
}

// The epistemic direction cuts both ways: a secret the NPC HAS been granted
// is genuinely in their head, marked (secret) for the DM.
func TestGrantedSecretFlowsToTheNPC(t *testing.T) {
	stub := newJSONLLM(t, "REACTION — he lies.\n\nIN-VOICE — \"I eat well.\"")
	s, f := newNPCAskServer(t, stub)
	grantDuke(t, s, *f, f.publicID)
	grantDuke(t, s, *f, f.secretID)

	if code, _ := askAs(t, s, *f, dmSession(t, s), f.dukeID, `{"question":"Why do you never eat?"}`); code != http.StatusOK {
		t.Fatalf("ask failed")
	}
	body := stub.lastBody()
	if !strings.Contains(body, "(secret) The Duke is secretly a vampire.") {
		t.Errorf("the granted secret must reach the NPC's context marked (secret):\n%s", body)
	}
}

func TestNPCAskRequiresDM(t *testing.T) {
	stub := newJSONLLM(t, "x")
	s, f := newNPCAskServer(t, stub)
	player := addPlayerMember(t, s, *f, "npcplayer", true)
	if code, _ := askAs(t, s, *f, player, f.dukeID, `{"question":"hi"}`); code != http.StatusForbidden {
		t.Errorf("player: status %d, want 403", code)
	}
	// A caller with no session is stopped at the gate — never reaching the
	// store, the model, or any hint the campaign exists.
	if code, _ := askAs(t, s, *f, nil, f.dukeID, `{"question":"hi"}`); code != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", code)
	}
	if stub.lastBody() != "" {
		t.Errorf("the model must not be called by a non-DM")
	}
}

func TestNPCAskRejectsNonNPCAndBadRequests(t *testing.T) {
	stub := newJSONLLM(t, "x")
	s, f := newNPCAskServer(t, stub)
	dm := dmSession(t, s)
	if code, _ := askAs(t, s, *f, dm, f.pcID, `{"question":"hi"}`); code != http.StatusBadRequest {
		t.Errorf("pc entity: status %d, want 400", code)
	}
	if code, _ := askAs(t, s, *f, dm, f.dukeID, `{"question":"  "}`); code != http.StatusBadRequest {
		t.Errorf("empty question: status %d, want 400", code)
	}
	if code, _ := askAs(t, s, *f, dm, "no-such-npc", `{"question":"hi"}`); code != http.StatusNotFound {
		t.Errorf("missing npc: status %d, want 404", code)
	}
	if stub.lastBody() != "" {
		t.Errorf("the model must not be called for invalid requests")
	}
}

func TestNPCAskUnconfigured(t *testing.T) {
	s, _, kStore, users := newCampaignServer(t)
	_ = kStore
	_ = users
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	// newCampaignServer wires no LLM key.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/npc/"+f.dukeID+"/ask",
		`{"question":"hi"}`, dm)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured: status %d, want 503 (%s)", rec.Code, rec.Body)
	}
}

// Faction membership colours the answer: the faction an NPC belongs to (their
// own outgoing allegiance edge) is in the context; a faction that secretly
// controls the NPC from outside is not — the NPC does not know they are a
// puppet.
func TestNPCAskFactionAllegianceColoursContext(t *testing.T) {
	stub := newJSONLLM(t, "REACTION — x")
	s, f := newNPCAskServer(t, stub)
	dm := dmSession(t, s)
	mk := func(kind, name, summary string) string {
		body := map[string]any{"kind": kind, "name": name, "summary": summary}
		b, _ := json.Marshal(body)
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", string(b), dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, r.Code, r.Body)
		}
		return idFrom(t, r, "entity")
	}
	cult := mk("faction", "Cult of the Root", "Working to bring the forest back.")
	hand := mk("faction", "The Hidden Hand", "Pulls strings from the shadows.")
	rel := func(from, relType, to string) {
		body := `{"from":` + quote(from) + `,"rel_type":` + quote(relType) + `,"to":` + quote(to) + `}`
		if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/relationships", body, dm); r.Code != http.StatusCreated {
			t.Fatalf("create rel %s %s %s: %d %s", from, relType, to, r.Code, r.Body)
		}
	}
	rel(f.dukeID, "member_of", cult)         // the Duke's own allegiance — in
	rel(hand, "secretly_controls", f.dukeID) // someone else's claim on him — out

	if code, _ := askAs(t, s, *f, dm, f.dukeID, `{"question":"What drives you?"}`); code != http.StatusOK {
		t.Fatalf("ask failed")
	}
	body := stub.lastBody()
	if !strings.Contains(body, "Cult of the Root") || !strings.Contains(body, "Working to bring the forest back.") {
		t.Errorf("the Duke's faction allegiance must colour the context:\n%s", body)
	}
	if !strings.Contains(body, "Faction allegiances") {
		t.Errorf("the allegiance framing is missing:\n%s", body)
	}
	if strings.Contains(body, "Hidden Hand") {
		t.Errorf("a faction secretly controlling the NPC leaked into the NPC's own context:\n%s", body)
	}
}

// Agent fields round-trip through the API, preserve the rest of the payload,
// and reach the prompt as the mind section.
func TestNPCAgentFieldsRoundTripAndFeedPrompt(t *testing.T) {
	stub := newJSONLLM(t, "x")
	s, f := newNPCAskServer(t, stub)
	dm := dmSession(t, s)

	// Seed an unrelated payload key first — the agent block must not clobber it.
	patch := `{"payload":{"cr":9}}`
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/entities/"+f.dukeID, patch, dm); r.Code != http.StatusOK {
		t.Fatalf("patch payload: %d %s", r.Code, r.Body)
	}
	agent := `{"public_identity":"Ruler of the northern marches","private_truth":"secretly serves the old forest","goals":["preserve his line","complete the ritual","avoid exposure"],"fears":["fire","the party's wizard"],"resources":["the city watch","a hoard of silver"],"personality":"precise, cold, patient","voice":"never uses contractions; pauses before threats"}`
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/npc/"+f.dukeID+"/agent", agent, dm); r.Code != http.StatusOK {
		t.Fatalf("put agent: %d %s", r.Code, r.Body)
	}
	get := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/npc/"+f.dukeID+"/agent", "", dm)
	if get.Code != http.StatusOK {
		t.Fatalf("get agent: %d %s", get.Code, get.Body)
	}
	var got struct {
		Agent campaign.NPCAgent `json:"agent"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if got.Agent.PrivateTruth != "secretly serves the old forest" || len(got.Agent.Goals) != 3 {
		t.Fatalf("agent round trip: %+v", got.Agent)
	}
	// The payload's other keys survive.
	ent := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/entities/"+f.dukeID, "", dm)
	var ev struct {
		Entity struct {
			Payload map[string]any `json:"payload"`
		} `json:"entity"`
	}
	if err := json.Unmarshal(ent.Body.Bytes(), &ev); err != nil {
		t.Fatalf("decode entity: %v", err)
	}
	if ev.Entity.Payload["cr"] != float64(9) || ev.Entity.Payload["agent"] == nil {
		t.Fatalf("payload keys clobbered: %v", ev.Entity.Payload)
	}

	// The mind reaches the prompt.
	grantDuke(t, s, *f, f.publicID)
	if code, _ := askAs(t, s, *f, dm, f.dukeID, `{"question":"What do you want?"}`); code != http.StatusOK {
		t.Fatalf("ask failed")
	}
	body := stub.lastBody()
	for _, want := range []string{
		"THE MIND OF DUKE ALDRIC VANE",
		"Public identity: Ruler of the northern marches",
		"Private truth",
		"[G1] preserve his line",
		"[G3] avoid exposure",
		"never uses contractions",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt missing %q:\n%s", want, body)
		}
	}

	// A player cannot read or write the mind.
	player := addPlayerMember(t, s, *f, "agentplayer", true)
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/npc/"+f.dukeID+"/agent", "", player); r.Code != http.StatusForbidden {
		t.Errorf("player GET agent: %d, want 403", r.Code)
	}
	if r := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/npc/"+f.dukeID+"/agent", agent, player); r.Code != http.StatusForbidden {
		t.Errorf("player PUT agent: %d, want 403", r.Code)
	}
}

// The reveals contract: inventions come back as data, default writes nothing,
// and the explicit stage opt-in lands them in the review queue behind the
// Make-it-canon gate.
func TestNPCAskRevealsParseStageAndAccept(t *testing.T) {
	answer := "REACTION — he defers, then moves to end the audience.\n\nIN-VOICE — \"We are done here.\"\n\n" +
		"```json\n{\"reveals\":[{\"statement\":\"The Duke keeps a loaded signet crossbow in his desk.\",\"rationale\":\"avoid exposure at all costs\"}]}\n```"
	stub := newJSONLLM(t, answer)
	s, f := newNPCAskServer(t, stub)
	dm := dmSession(t, s)
	grantDuke(t, s, *f, f.publicID)

	// Default: no stage, nothing queued.
	code, resp := askAs(t, s, *f, dm, f.dukeID, `{"question":"How do you react to the accusation?"}`)
	if code != http.StatusOK {
		t.Fatalf("ask: %d %v", code, resp)
	}
	if a, _ := resp["answer"].(string); strings.Contains(a, "```") || strings.Contains(a, "signet crossbow") {
		t.Errorf("answer must be the prose only, no fence, no reveal text: %q", a)
	}
	revs, _ := resp["reveals"].([]any)
	if len(revs) != 1 {
		t.Fatalf("reveals: %v", resp["reveals"])
	}
	if r := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/reviews?status=open", "", dm); r.Code == http.StatusOK {
		if strings.Contains(r.Body.String(), "npc_reveal") {
			t.Errorf("nothing may be staged without the explicit opt-in")
		}
	}

	// Explicit opt-in: the reveal lands in the queue.
	code, resp = askAs(t, s, *f, dm, f.dukeID, `{"question":"How do you react to the accusation?","stage":true}`)
	if code != http.StatusOK {
		t.Fatalf("ask+stage: %d %v", code, resp)
	}
	staged, _ := resp["staged"].([]any)
	if len(staged) != 1 {
		t.Fatalf("staged: %v", resp["staged"])
	}
	stagedItem, _ := staged[0].(map[string]any)
	reviewID, _ := stagedItem["review_id"].(string)

	// The queue shows it as an npc_reveal item.
	list := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/reviews?status=open", "", dm)
	if !strings.Contains(list.Body.String(), "npc_reveal") || !strings.Contains(list.Body.String(), "Duke Aldric Vane") {
		t.Fatalf("queue missing the npc_reveal item: %s", list.Body)
	}

	// Accept it: the reveal becomes canon with human provenance.
	dec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/reviews/"+reviewID+"/decision",
		`{"decision":"accept"}`, dm)
	if dec.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", dec.Code, dec.Body)
	}
	var dv struct {
		Review struct {
			ResultRef string `json:"result_ref"`
		} `json:"review"`
	}
	if err := json.Unmarshal(dec.Body.Bytes(), &dv); err != nil || dv.Review.ResultRef == "" {
		t.Fatalf("accept response: %v (%s)", err, dec.Body)
	}
	fact := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/facts/"+dv.Review.ResultRef, "", dm)
	if fact.Code != http.StatusOK {
		t.Fatalf("accepted fact: %d %s", fact.Code, fact.Body)
	}
	if !strings.Contains(fact.Body.String(), "signet crossbow") || !strings.Contains(fact.Body.String(), `"confidence":"canon"`) {
		t.Fatalf("accepted fact wrong: %s", fact.Body)
	}
	if !strings.Contains(fact.Body.String(), `"method":"ai_proposed"`) || !strings.Contains(fact.Body.String(), `"accepted_by":"`) {
		t.Fatalf("provenance must record the model proposal and the human acceptance: %s", fact.Body)
	}
}

// The model forgetting the fence, or fencing garbage, must never break the
// answer.
func TestSplitRevealsTolerance(t *testing.T) {
	plain := "REACTION — x\n\nIN-VOICE — \"y.\""
	if a, revs, err := splitReveals(plain); err != nil || a != plain || revs != nil {
		t.Errorf("no fence: a=%q revs=%v err=%v", a, revs, err)
	}
	mid := "REACTION — ```quoted speech``` here.\n\nIN-VOICE — \"y.\""
	if a, revs, err := splitReveals(mid); err != nil || a != mid || revs != nil {
		t.Errorf("mid-text fence is dialogue, not the reveals tail: a=%q revs=%v err=%v", a, revs, err)
	}
	garbage := "REACTION — x\n\n```json\n{not json}\n```"
	a, revs, err := splitReveals(garbage)
	if err == nil || revs != nil || a != "REACTION — x" {
		t.Errorf("garbage fence: a=%q revs=%v err=%v", a, revs, err)
	}
	empty := "x\n\n```json\n{\"reveals\":[]}\n```"
	if a, revs, err := splitReveals(empty); err != nil || a != "x" || len(revs) != 0 {
		t.Errorf("empty reveals: a=%q revs=%v err=%v", a, revs, err)
	}
	bare := "x\n\n```\n{\"reveals\":[{\"statement\":\"s\"}]}\n```"
	if a, revs, err := splitReveals(bare); err != nil || a != "x" || len(revs) != 1 {
		t.Errorf("bare fence: a=%q revs=%v err=%v", a, revs, err)
	}
}
