package server

// In-play capture (MAD-483): the DM screen's few-second affair — a
// discovery or ruling logged mid-play with entity linkage and scene
// context, the ruling matcher readable while the DM is still typing, and
// a discovery able to propose a fact into the review queue. Nothing here
// writes a fact directly; the queue stays the only gate, asserted as the
// round-trip below.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/story"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newCaptureServer boots the play-mode stack end to end: the campaign
// graph, the knowledge layer, the story spine, the session log — and a
// canon engine whose graph stores are wired, so a proposal can be
// decided into canon.
func newCaptureServer(t *testing.T) (*Server, *fixture) {
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
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
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

// buildCaptureStage seats the fixture's campaign mid-play: an earlier
// sitting carrying a ruling, and a live session the screen captures
// against. Returns the live session id.
func buildCaptureStage(t *testing.T, s *Server, f fixture) string {
	t.Helper()
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	// The earlier sitting: one ruling the matcher should find again.
	earlier := newCaptureSession(t, s, f, "The Long Rain", dm)
	if r := hit(t, s, http.MethodPost, base+"/sessions/"+earlier+"/events",
		`{"kind":"ruling","summary":"Can torches be relit underwater?","detail":"No — waterlogged tinder stays dead."}`, dm); r.Code != http.StatusCreated {
		t.Fatalf("log earlier ruling: status %d, body %s", r.Code, r.Body)
	}

	// The live sitting.
	live := newCaptureSession(t, s, f, "The Crypt Below", dm)
	if r := hit(t, s, http.MethodPatch, base+"/sessions/"+live, `{"status":"live"}`, dm); r.Code != http.StatusOK {
		t.Fatalf("go live: status %d, body %s", r.Code, r.Body)
	}
	return live
}

// newCaptureSession creates a session through the API as the given
// caller, returning its id.
func newCaptureSession(t *testing.T, s *Server, f fixture, name string, dm *http.Cookie) string {
	t.Helper()
	r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/sessions",
		fmt.Sprintf(`{"name":%q}`, name), dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", r.Code, r.Body)
	}
	var out struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Session.ID
}

func TestRulingMatchesWhileTyping(t *testing.T) {
	s, f := newCaptureServer(t)
	live := buildCaptureStage(t, s, *f)
	dm := dmSession(t, s)

	// The question, half-typed, matches the earlier sitting's ruling.
	r := hit(t, s, http.MethodGet,
		"/api/campaigns/"+f.campaignID+"/sessions/"+live+"/ruling-matches?q="+url.QueryEscape("torches underwater"), "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("matches: status %d, body %s", r.Code, r.Body)
	}
	var out struct {
		Matches []struct {
			SessionOrdinal int64  `json:"session_ordinal"`
			Summary        string `json:"summary"`
			Detail         string `json:"detail"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %+v, want the earlier ruling", out.Matches)
	}
	m := out.Matches[0]
	if m.Summary != "Can torches be relit underwater?" || m.Detail == "" || m.SessionOrdinal != 1 {
		t.Errorf("match = %+v, want session 1's ruling with its answer", m)
	}

	// A question with nothing behind it is an empty list, not an error.
	r = hit(t, s, http.MethodGet,
		"/api/campaigns/"+f.campaignID+"/sessions/"+live+"/ruling-matches?q="+url.QueryEscape("planar travel"), "", dm)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"matches":[]`) {
		t.Fatalf("no-match read: status %d, body %s", r.Code, r.Body)
	}
}

// The matcher is the DM's material: a player member is refused, a
// stranger learns the campaign does not exist.
func TestRulingMatchesScope(t *testing.T) {
	s, f := newCaptureServer(t)
	live := buildCaptureStage(t, s, *f)

	player := addPlayerMember(t, s, *f, "mira", true)
	r := hit(t, s, http.MethodGet,
		"/api/campaigns/"+f.campaignID+"/sessions/"+live+"/ruling-matches?q=torches", "", player)
	if r.Code != http.StatusForbidden {
		t.Fatalf("player read: status %d, want 403", r.Code)
	}
	if strings.Contains(r.Body.String(), "waterlogged") {
		t.Error("the refusal carried a prior ruling")
	}

	stranger := registerOutsider(t, s, "capture-stranger")
	r = hit(t, s, http.MethodGet,
		"/api/campaigns/"+f.campaignID+"/sessions/"+live+"/ruling-matches?q=torches", "", stranger)
	if r.Code != http.StatusNotFound {
		t.Fatalf("stranger read: status %d, want 404", r.Code)
	}
}

// THE ACCEPTANCE TEST: a logged discovery proposes a fact through the
// review queue — the proposal is open, nothing is canon yet, and only the
// queue's decision writes the fact, with the event linked as provenance.
func TestProposeEventFactRoundTrip(t *testing.T) {
	s, f := newCaptureServer(t)
	live := buildCaptureStage(t, s, *f)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	// The capture itself: a discovery with scene and entity linkage.
	evJSON := fmt.Sprintf(`{
		"kind": "discovery",
		"summary": "the cult sigil opens the crypt door",
		"detail": "Pressed into the keystone; the door answered.",
		"payload": {
			"scene": {"scene_id": "sc-1", "scene_name": "The Crypt Below"},
			"entities": [{"id": %q, "name": "Duke Aldric Vane", "source": "focus"}]
		}
	}`, f.dukeID)
	r := hit(t, s, http.MethodPost, base+"/sessions/"+live+"/events", evJSON, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("log discovery: status %d, body %s", r.Code, r.Body)
	}
	var evOut struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &evOut); err != nil {
		t.Fatal(err)
	}

	// The proposal: prefilled from the capture, subject the linked entity.
	r = hit(t, s, http.MethodPost, base+"/sessions/"+live+"/events/"+evOut.Event.ID+"/propose-fact",
		fmt.Sprintf(`{"statement":"The cult sigil opens the crypt door.","subject":%q,"predicate":"opens","object_literal":"the crypt door"}`, f.dukeID), dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("propose: status %d, body %s", r.Code, r.Body)
	}
	batch := decodeBatch(t, r)
	if batch["source"] != "session_capture" || batch["status"] != "open" {
		t.Fatalf("batch = %v / %v, want session_capture and open", batch["source"], batch["status"])
	}
	items, _ := batch["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want the one fact", len(items))
	}
	item, _ := items[0].(map[string]any)
	if item["kind"] != "proposed_fact" {
		t.Fatalf("item kind = %v", item["kind"])
	}
	payload, _ := item["payload"].(map[string]any)
	if payload["statement"] != "The cult sigil opens the crypt door." || payload["subject"] != f.dukeID {
		t.Fatalf("payload = %+v", payload)
	}
	if payload["session_event_id"] != evOut.Event.ID {
		t.Errorf("payload session_event_id = %v, want the anchor event", payload["session_event_id"])
	}
	batchID, _ := batch["id"].(string)

	// Nothing wrote a fact directly: the graph is unchanged until the
	// queue decides.
	r = hit(t, s, http.MethodGet, base+"/entities/"+f.dukeID, "", dm)
	if r.Code != http.StatusOK {
		t.Fatalf("entity read: status %d", r.Code)
	}
	if strings.Contains(r.Body.String(), "cult sigil") {
		t.Fatal("the fact landed without a review decision — the queue is not the only gate")
	}

	// The queue's decision is what writes it.
	r = hit(t, s, http.MethodPost, base+"/proposals/"+batchID+"/decision", `{"decision":"accept"}`, dm)
	if r.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", r.Code, r.Body)
	}
	r = hit(t, s, http.MethodGet, base+"/entities/"+f.dukeID, "", dm)
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "The cult sigil opens the crypt door.") {
		t.Fatalf("the accepted fact is missing: %d %s", r.Code, r.Body)
	}
}

func TestProposeEventFactValidation(t *testing.T) {
	s, f := newCaptureServer(t)
	live := buildCaptureStage(t, s, *f)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	r := hit(t, s, http.MethodPost, base+"/sessions/"+live+"/events",
		`{"kind":"discovery","summary":"the sigil"}`, dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("log discovery: status %d, body %s", r.Code, r.Body)
	}
	var evOut struct {
		Event struct {
			ID string `json:"id"`
		} `json:"event"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &evOut); err != nil {
		t.Fatal(err)
	}
	propose := base + "/sessions/" + live + "/events/" + evOut.Event.ID + "/propose-fact"

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"no subject", `{"statement":"x"}`, http.StatusBadRequest},
		{"bad visibility", `{"statement":"x","subject":"` + f.dukeID + `","visibility":"loud"}`, http.StatusBadRequest},
		{"bad body", `{`, http.StatusBadRequest},
	} {
		if r := hit(t, s, http.MethodPost, propose, tc.body, dm); r.Code != tc.want {
			t.Fatalf("%s: status %d, want %d (%s)", tc.name, r.Code, tc.want, r.Body)
		}
	}

	// A statement may fall back to the event's summary — the prefilled
	// capture is the common case.
	if r := hit(t, s, http.MethodPost, propose,
		fmt.Sprintf(`{"subject":%q}`, f.dukeID), dm); r.Code != http.StatusCreated {
		t.Fatalf("prefilled statement: status %d, body %s", r.Code, r.Body)
	}

	// An event that is not this session's is a plain 404.
	if r := hit(t, s, http.MethodPost, base+"/sessions/"+live+"/events/no-such-event/propose-fact",
		fmt.Sprintf(`{"statement":"x","subject":%q}`, f.dukeID), dm); r.Code != http.StatusNotFound {
		t.Fatalf("unknown event: status %d, want 404", r.Code)
	}

	// Only the DM proposes.
	player := addPlayerMember(t, s, *f, "tam", true)
	if r := hit(t, s, http.MethodPost, propose,
		fmt.Sprintf(`{"statement":"x","subject":%q}`, f.dukeID), player); r.Code != http.StatusForbidden {
		t.Fatalf("player propose: status %d, want 403", r.Code)
	}

	// Without the canon engine there is nothing to stage into.
	s.canon = nil
	if r := hit(t, s, http.MethodPost, propose,
		fmt.Sprintf(`{"statement":"x","subject":%q}`, f.dukeID), dm); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired canon: status %d, want 503", r.Code)
	}
}

// The leak pin: the capture payload is new player-visible surface (the
// event list is member-readable), so its absence of spine material is
// asserted on the raw body — the scene's name rides (the table played
// it), its purpose and the campaign's secrets do not.
func TestCapturePayloadCarriesNoSpineMaterial(t *testing.T) {
	s, f := newCaptureServer(t)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID
	activeID, _, live := buildLiveStage(t, s, *f)

	// The scene carries planning material the card never spells.
	r := hit(t, s, http.MethodPost, base+"/sessions/"+live+"/events", fmt.Sprintf(`{
		"kind": "discovery",
		"summary": "the waystone's cellar hides the crypt entrance",
		"payload": {
			"scene": {"scene_id": %q, "scene_name": "The Waystone at midnight"},
			"entities": [{"id": %q, "name": "Duke Aldric Vane", "source": "focus"}]
		}
	}`, activeID, f.dukeID), dm)
	if r.Code != http.StatusCreated {
		t.Fatalf("log discovery: status %d, body %s", r.Code, r.Body)
	}

	player := addPlayerMember(t, s, *f, "mira", true)
	r = hit(t, s, http.MethodGet, base+"/sessions/"+live+"/events", "", player)
	if r.Code != http.StatusOK {
		t.Fatalf("player event list: status %d, body %s", r.Code, r.Body)
	}
	body := r.Body.String()
	if !strings.Contains(body, "the waystone's cellar hides the crypt entrance") {
		t.Fatal("the logged discovery did not reach the member-visible log")
	}
	if !strings.Contains(body, "The Waystone at midnight") || !strings.Contains(body, f.dukeID) {
		t.Fatal("the linkage (scene name, entity id) did not ride the payload")
	}
	for _, leak := range []string{"Put the question on the table", "vampire", f.secretID} {
		if strings.Contains(body, leak) {
			t.Fatalf("the capture payload leaked %q — a leak:\n%s", leak, body)
		}
	}
}
