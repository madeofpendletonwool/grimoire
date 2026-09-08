package server

// The session copilot's tests (MAD-486). The load-bearing one is the
// acceptance the issue states: an NPC who does not know something does not
// reveal it — proven on the assembled context, the exact request body the
// model received. The scene's hidden clue is the DM's release material and
// rides the prompt as such, but the NPC's own record is scope-filtered at
// npc:<id> in SQL, so the secret never enters the section the answer is
// drawn from unless a fact says the NPC knows it.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/story"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newCopilotServer boots the copilot's whole stack: campaigns, knowledge,
// sessions, story, canon and a stubbed streaming LLM. The combat and board
// stores stay unwired — those context blocks are optional by design, and
// their spelling has its own unit test.
func newCopilotServer(t *testing.T, stub *capturingLLM) (*Server, *fixture) {
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
	stories, err := story.New(store.DB())
	if err != nil {
		t.Fatalf("open story store: %v", err)
	}
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
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
	s = s.WithCampaigns(campaigns, knowledgeStore).
		WithCampaign(campaigns, sessions).
		WithCanon(engine).
		WithStory(stories)
	f := buildFixture(t, s)
	return s, &f
}

// copilotAnswer is one stubbed reply shaped the way the contract asks:
// two parts plus a reveals fence.
const copilotAnswer = "REACTION — the Duke turns the question back on the party, cold as ever.\n\n" +
	"IN-VOICE — \"The marches have enough dead. Ask your questions elsewhere.\"\n\n" +
	"```json\n{\"reveals\":[{\"statement\":\"The Duke keeps the town watch's reports unopened in a drawer.\",\"rationale\":\"avoid exposure\"}]}\n```"

// askCopilot posts one question and returns the status plus SSE body.
func askCopilot(t *testing.T, s *Server, f fixture, cookie *http.Cookie, body string) (int, string) {
	t.Helper()
	var rec *recorder
	target := "/api/campaigns/" + f.campaignID + "/ask"
	if cookie == nil {
		rec = hit(t, s, http.MethodPost, target, body)
	} else {
		rec = hit(t, s, http.MethodPost, target, body, cookie)
	}
	return rec.Code, rec.Body.String()
}

// section slices the body between two === markers — the shape the leak
// assertion needs, since the hidden-clue section legitimately carries the
// secret while the NPC's record must not.
func section(body, marker string) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest[len(marker):], "\n==="); j >= 0 {
		return rest[:len(marker)+j]
	}
	if j := strings.Index(rest[len(marker):], "\nQuestion:"); j >= 0 {
		return rest[:len(marker)+j]
	}
	return rest
}

// THE ACCEPTANCE TEST: the question names the Duke, who is on stage, and
// the scene carries the vampire secret in play. The live context rides
// the prompt (session, scene, events), the clue lists ride it (the public
// fact discovered, the secret hidden) — and the Duke's own record
// contains ONLY what his awareness covers. He does not know the secret,
// so it is not in his record, so he cannot reveal it: the model never
// received it as his knowledge.
func TestCopilotAskGroundsLiveContextAndNPCScope(t *testing.T) {
	stub := newCapturingLLM(t, copilotAnswer)
	s, f := newCopilotServer(t, stub)
	activeID, _, sessionID := buildLiveStage(t, s, *f) // scene: duke focus, secret in play, session live
	grantDuke(t, s, *f, f.publicID)                    // the Duke knows what he rules; NOT the secret
	dm := dmSession(t, s)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/sessions/"+sessionID+"/events",
		`{"kind":"discovery","summary":"The party found the mining ledger."}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("log event: %d %s", r.Code, r.Body)
	}

	code, body := askCopilot(t, s, *f, dm, `{"question":"They ask the Duke about the murders. What does he know?"}`)
	if code != http.StatusOK {
		t.Fatalf("ask: status %d, body %s", code, body)
	}
	prompt := stub.lastBody()
	if prompt == "" {
		t.Fatal("the model was never called")
	}

	// The live context braid: session, scene, cast, recent event.
	for _, want := range []string{
		"Live session: Session 1",
		"Scene: The Waystone at midnight",
		"On stage: Duke Aldric Vane (focus)",
		"(discovery) The party found the mining ledger.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing live context %q:\n%s", want, prompt)
		}
	}
	// The clue-discovery awareness: the public fact is discovered, the
	// scene's secret is hidden release material.
	if !strings.Contains(section(prompt, "=== CLUES THE TABLE ALREADY HOLDS"), "The Duke rules the northern marches.") {
		t.Errorf("the discovered public fact must ride the discovered list:\n%s", prompt)
	}
	hidden := section(prompt, "=== CLUES IN PLAY, STILL HIDDEN")
	if !strings.Contains(hidden, "The Duke is secretly a vampire.") {
		t.Errorf("the scene's in-play secret must ride the hidden list:\n%s", prompt)
	}
	// The voice: the mind and the scoped record.
	if !strings.Contains(prompt, "THE MIND OF DUKE ALDRIC VANE") || !strings.Contains(prompt, "WHAT DUKE ALDRIC VANE KNOWS") {
		t.Errorf("the npc braid is missing:\n%s", prompt)
	}
	record := section(prompt, "=== WHAT DUKE ALDRIC VANE KNOWS")
	if !strings.Contains(record, "The Duke rules the northern marches.") {
		t.Errorf("the granted fact must be in the Duke's record:\n%s", record)
	}
	if strings.Contains(record, "vampire") || strings.Contains(record, f.secretID) {
		t.Errorf("LEAK: the secret the Duke was never granted is in his record — he cannot know it:\n%s", record)
	}
	// The system prompt carries the rule the scope filter cannot express.
	if !strings.Contains(prompt, "must not speak, hint at, or act on a hidden clue") {
		t.Errorf("the hidden-clue rule is missing from the system prompt:\n%s", prompt)
	}

	// The frames: meta names the voice and the clue lists; done carries
	// the clean answer and the parsed reveal.
	frames := campaignSSEFrames(body)
	meta, ok := frames["meta"]
	if !ok {
		t.Fatalf("no meta frame: %s", body)
	}
	npc, _ := meta["npc"].(map[string]any)
	if npc == nil || npc["id"] != f.dukeID {
		t.Fatalf("meta.npc must name the voice on stage: %v", meta["npc"])
	}
	if scene, _ := meta["scene"].(map[string]any); scene == nil || scene["id"] != activeID {
		t.Fatalf("meta.scene must name the current scene: %v", meta["scene"])
	}
	disc, _ := meta["discovered"].([]any)
	if len(disc) != 1 {
		t.Fatalf("meta.discovered: %v", meta["discovered"])
	}
	hid, _ := meta["hidden"].([]any)
	if len(hid) != 1 {
		t.Fatalf("meta.hidden: %v", meta["hidden"])
	}
	if _, ok := frames["delta"]; !ok {
		t.Fatalf("no delta frame: %s", body)
	}
	done, ok := frames["done"]
	if !ok {
		t.Fatalf("no done frame: %s", body)
	}
	if a := done["answer"].(string); strings.Contains(a, "```") || strings.Contains(a, "drawer") {
		t.Errorf("done.answer must be the prose only: %q", a)
	}
	revs, _ := done["reveals"].([]any)
	if len(revs) != 1 {
		t.Fatalf("done.reveals: %v", done["reveals"])
	}
}

// The granted-secret direction: a secret the NPC HAS been granted is
// genuinely in their head — the record is a scope filter, not a
// secret-blocker.
func TestCopilotGrantedSecretReachesTheVoice(t *testing.T) {
	stub := newCapturingLLM(t, copilotAnswer)
	s, f := newCopilotServer(t, stub)
	buildLiveStage(t, s, *f)
	grantDuke(t, s, *f, f.publicID)
	grantDuke(t, s, *f, f.secretID)

	code, _ := askCopilot(t, s, *f, dmSession(t, s), `{"question":"Why does the Duke never eat?"}`)
	if code != http.StatusOK {
		t.Fatalf("ask: %d", code)
	}
	record := section(stub.lastBody(), "=== WHAT DUKE ALDRIC VANE KNOWS")
	if !strings.Contains(record, "(secret) The Duke is secretly a vampire.") {
		t.Errorf("the granted secret must reach the record marked (secret):\n%s", record)
	}
}

// Table mode: a question that names no one on stage still grounds in the
// live table and the clue lists, with no voice section.
func TestCopilotTableModeWithoutNPC(t *testing.T) {
	stub := newCapturingLLM(t, "The watch is stretched thin; two bodies in a week.")
	s, f := newCopilotServer(t, stub)
	buildLiveStage(t, s, *f)
	_ = f

	code, body := askCopilot(t, s, *f, dmSession(t, s), `{"question":"What do the townsfolk know about the murders?"}`)
	if code != http.StatusOK {
		t.Fatalf("ask: %d", code)
	}
	prompt := stub.lastBody()
	if strings.Contains(prompt, "THE MIND OF") || strings.Contains(prompt, "WHAT DUKE ALDRIC VANE KNOWS") {
		t.Errorf("no NPC was named; no voice section belongs:\n%s", prompt)
	}
	if !strings.Contains(prompt, "THE TABLE NOW") || !strings.Contains(prompt, "CLUES THE TABLE ALREADY HOLDS") {
		t.Errorf("the live table and clue lists must still ground the answer:\n%s", prompt)
	}
	frames := campaignSSEFrames(body)
	meta, ok := frames["meta"]
	if !ok {
		t.Fatalf("no meta frame: %s", body)
	}
	if _, ok := meta["npc"]; ok {
		t.Errorf("meta.npc must be absent in table mode: %v", meta["npc"])
	}
	if _, ok := frames["done"]; !ok {
		t.Fatalf("no done frame: %s", body)
	}
}

// The DM's picked scene rides the ask: the screen sends scene_id when the
// DM tapped another active scene to read.
func TestCopilotHonorsPickedScene(t *testing.T) {
	stub := newCapturingLLM(t, "x")
	s, f := newCopilotServer(t, stub)
	_, plannedID, _ := buildLiveStage(t, s, *f)
	dm := dmSession(t, s)
	// Seat the planned scene in a second session-free act of play: make
	// it active so it is pickable.
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/scenes/"+plannedID, `{"status":"active"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("activate second scene: %d %s", r.Code, r.Body)
	}
	code, _ := askCopilot(t, s, *f, dm, `{"question":"What is happening here?","scene_id":`+quote(plannedID)+`}`)
	if code != http.StatusOK {
		t.Fatalf("ask: %d", code)
	}
	if !strings.Contains(stub.lastBody(), "Scene: The auction at dawn") {
		t.Errorf("the picked scene must ground the answer:\n%s", stub.lastBody())
	}
}

// The release round-trip: accept stages the npc_reveal and logs the
// discovery event against the live session; no fact exists until the
// queue decides; the decision writes it with human provenance. A modify
// is an accept of an edited statement.
func TestCopilotReleaseRoundTripsThroughQueue(t *testing.T) {
	stub := newCapturingLLM(t, copilotAnswer)
	s, f := newCopilotServer(t, stub)
	_, _, sessionID := buildLiveStage(t, s, *f)
	dm := dmSession(t, s)

	release := func(body string) (int, map[string]any) {
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/ask/release", body, dm)
		var out map[string]any
		if r.Code == http.StatusCreated {
			if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode release: %v (%s)", err, r.Body)
			}
		}
		return r.Code, out
	}

	// Accept: the event anchors, the queue stages, nothing becomes fact.
	code, resp := release(`{"statement":"The Duke keeps the town watch's reports unopened in a drawer.",` +
		`"rationale":"avoid exposure","question":"They ask the Duke about the murders.","npc_id":` + quote(f.dukeID) + `}`)
	if code != http.StatusCreated {
		t.Fatalf("release: %d", code)
	}
	ev, _ := resp["event"].(map[string]any)
	if ev == nil || ev["kind"] != "discovery" || ev["session_id"] != sessionID {
		t.Fatalf("the discovery event must anchor to the live session: %v", ev)
	}
	staged, _ := resp["staged"].(map[string]any)
	if staged["kind"] != "npc_reveal" {
		t.Fatalf("staged: %v", staged)
	}
	reviewID, _ := staged["review_id"].(string)

	// The Stage 4 anchor is the immutable record: it is in the log with
	// its copilot payload linkage.
	log := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/sessions/"+sessionID+"/events", "", dm)
	if !strings.Contains(log.Body.String(), "unopened in a drawer") || !strings.Contains(log.Body.String(), `"source":"copilot"`) {
		t.Fatalf("the release must be a logged discovery event with copilot linkage:\n%s", log.Body)
	}

	// Nothing is a fact yet — the queue is the only gate.
	facts := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/facts", "", dm)
	if strings.Contains(facts.Body.String(), "unopened in a drawer") {
		t.Fatalf("LEAK: the release became a fact before any decision:\n%s", facts.Body)
	}

	// The queue shows it; the decision makes it canon.
	list := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/reviews?status=open", "", dm)
	if !strings.Contains(list.Body.String(), "npc_reveal") {
		t.Fatalf("queue missing the npc_reveal item:\n%s", list.Body)
	}
	dec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/reviews/"+reviewID+"/decision", `{"decision":"accept"}`, dm)
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
	if !strings.Contains(fact.Body.String(), "unopened in a drawer") || !strings.Contains(fact.Body.String(), `"confidence":"canon"`) {
		t.Fatalf("the accepted release must be a canon fact:\n%s", fact.Body)
	}

	// Modify: an edited statement through the same route stages the
	// corrected item.
	code, resp = release(`{"statement":"The Duke burns the town watch's reports unread each morning.","npc_id":` + quote(f.dukeID) + `}`)
	if code != http.StatusCreated {
		t.Fatalf("modify release: %d", code)
	}
	staged, _ = resp["staged"].(map[string]any)
	modID, _ := staged["review_id"].(string)
	if modID == "" || modID == reviewID {
		t.Fatalf("the modified statement is its own queue item: %v", staged)
	}
	modList := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/reviews?status=open", "", dm)
	if !strings.Contains(modList.Body.String(), "burns the town watch's reports unread") {
		t.Fatalf("the modified statement must sit in the queue:\n%s", modList.Body)
	}
}

// A voiceless release still round-trips: the session_capture path Stage 4
// established, with the subject entity required.
func TestCopilotReleaseWithoutNPCStagesCaptureFact(t *testing.T) {
	stub := newCapturingLLM(t, copilotAnswer)
	s, f := newCopilotServer(t, stub)
	buildLiveStage(t, s, *f)
	dm := dmSession(t, s)

	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/ask/release",
		`{"statement":"The murders follow the old road, not the new.","subject":`+quote(f.dukeID)+`}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("release: %d %s", r.Code, r.Body)
	}
	if !strings.Contains(r.Body.String(), `"kind":"session_capture"`) {
		t.Fatalf("a voiceless release stages a session_capture item:\n%s", r.Body)
	}

	// No voice, no subject: refused.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/ask/release",
		`{"statement":"x"}`, dm); r.Code != http.StatusBadRequest {
		t.Errorf("voiceless subjectless release: %d, want 400", r.Code)
	}
	// Empty statement: refused.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/ask/release",
		`{"statement":"  ","npc_id":`+quote(f.dukeID)+`}`, dm); r.Code != http.StatusBadRequest {
		t.Errorf("empty statement: %d, want 400", r.Code)
	}
	// A non-npc voice: refused.
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/ask/release",
		`{"statement":"x","npc_id":`+quote(f.pcID)+`}`, dm); r.Code != http.StatusBadRequest {
		t.Errorf("pc as voice: %d, want 400", r.Code)
	}
}

// A release with no live session is refused: the Stage 4 anchor is the
// point, and an anchor needs a session.
func TestCopilotReleaseNeedsALiveSession(t *testing.T) {
	stub := newCapturingLLM(t, copilotAnswer)
	s, f := newCopilotServer(t, stub)
	buildLiveStage(t, s, *f)
	dm := dmSession(t, s)
	sid := extractSessionID(t, s, *f)
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/sessions/"+sid, `{"status":"done"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("end session: %d", r.Code)
	}
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/ask/release",
		`{"statement":"x","npc_id":`+quote(f.dukeID)+`}`, dm); r.Code != http.StatusBadRequest {
		t.Errorf("no live session: %d, want 400 (%s)", r.Code, r.Body)
	}
}

// THE SCOPE TESTS: the copilot is the DM's material. A player member is
// refused without a model call; a stranger learns nothing.
func TestCopilotAskRequiresDM(t *testing.T) {
	stub := newCapturingLLM(t, "x")
	s, f := newCopilotServer(t, stub)
	buildLiveStage(t, s, *f)

	player := addPlayerMember(t, s, *f, "copilotplayer", true)
	if code, _ := askCopilot(t, s, *f, player, `{"question":"hi"}`); code != http.StatusForbidden {
		t.Errorf("player ask: %d, want 403", code)
	}
	if code, _ := askCopilot(t, s, *f, nil, `{"question":"hi"}`); code != http.StatusUnauthorized {
		t.Errorf("anonymous ask: %d, want 401", code)
	}
	stranger := registerOutsider(t, s, "copilot-stranger")
	if code, _ := askCopilot(t, s, *f, stranger, `{"question":"hi"}`); code != http.StatusNotFound {
		t.Errorf("stranger ask: %d, want 404", code)
	}
	if stub.lastBody() != "" {
		t.Errorf("the model must not be called by a non-DM")
	}
	dm := dmSession(t, s)
	if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/ask/release", `{"statement":"x"}`, player); r.Code != http.StatusForbidden {
		t.Errorf("player release: %d, want 403", r.Code)
	}
	if code, _ := askCopilot(t, s, *f, dm, `{"question":"  "}`); code != http.StatusBadRequest {
		t.Errorf("empty question: %d, want 400", code)
	}
	if code, _ := askCopilot(t, s, *f, dm, `not json`); code != http.StatusBadRequest {
		t.Errorf("bad body: %d, want 400", code)
	}
}

// The honest 503: no model key, no copilot — the screen says so plainly.
func TestCopilotAskUnconfigured(t *testing.T) {
	s := newStoryServer(t) // wires no LLM key
	f := buildFixture(t, s)
	if code, _ := askCopilot(t, s, f, dmSession(t, s), `{"question":"hi"}`); code != http.StatusServiceUnavailable {
		t.Errorf("unconfigured: %d, want 503", code)
	}
}

// The pure derivations: the combatant state lines, the party lines, and
// the scene picking order.
func TestCopilotPromptDerivations(t *testing.T) {
	fighter := combat.Combatant{HP: 11, TempHP: 2, MaxHP: 20}
	if got := combatantState(fighter); got != "13/20 hp" {
		t.Errorf("combatantState hp: %q", got)
	}
	if got := combatantState(combat.Combatant{Downed: true}); got != "downed" {
		t.Errorf("combatantState downed: %q", got)
	}
	if got := combatantState(combat.Combatant{Downed: true, Stable: true}); got != "downed, stable" {
		t.Errorf("combatantState stable: %q", got)
	}
	if got := combatantState(combat.Combatant{Dead: true}); got != "dead" {
		t.Errorf("combatantState dead: %q", got)
	}

	scenes := []story.Scene{{ID: "a"}, {ID: "b", SessionID: "s1"}}
	if got := pickScene(scenes, nil, ""); got != "a" {
		t.Errorf("pickScene first: %q", got)
	}
	if got := pickScene(scenes, &gamesession.Session{ID: "s1"}, ""); got != "b" {
		t.Errorf("pickScene seated: %q", got)
	}
	if got := pickScene(scenes, nil, "b"); got != "b" {
		t.Errorf("pickScene picked: %q", got)
	}
	if got := pickScene(nil, nil, ""); got != "" {
		t.Errorf("pickScene empty: %q", got)
	}

	// Word matching: 4+-letter name words fold into the question's words.
	if !wordSet("What does the DUKE know?")["duke"] {
		t.Error("wordSet must fold case")
	}
	if wordSet("the duke")["the"] {
		t.Error("short words do not match")
	}
}
