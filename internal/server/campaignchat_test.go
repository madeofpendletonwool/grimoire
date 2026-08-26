package server

// Campaign chat tests (MAD-311). The load-bearing ones assert on the
// assembled prompt — the exact system + messages body sent to the model —
// not just the HTTP output: a player-scoped question about a secret fact
// must be answered from a context that provably never contained it.

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
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// capturingLLM is a stubbed Anthropic-compatible endpoint that records every
// request body it receives, so tests can assert on the assembled prompt.
type capturingLLM struct {
	mu      sync.Mutex
	bodies  []string
	baseURL string
}

func newCapturingLLM(t *testing.T, answer string) *capturingLLM {
	t.Helper()
	c := &capturingLLM{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(raw))
		c.mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", answer)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(up.Close)
	c.baseURL = up.URL
	return c
}

// lastBody returns the most recent request body sent to the model.
func (c *capturingLLM) lastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return c.bodies[len(c.bodies)-1]
}

// newCampaignChatServer boots a gated server with the campaign stack, a chat
// store, and a stubbed LLM. The fixture campaign carries a secret fact the
// party has a granting awareness row on — exactly the state the PlayerView
// must survive.
func newCampaignChatServer(t *testing.T, stub *capturingLLM) (*Server, *fixture) {
	t.Helper()
	store, err := index.Open(t.TempDir() + "/test.db")
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
	chats, err := chat.New(store.DB())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	cfg := llm.Config{BaseURL: stub.baseURL, APIKey: "test-key", Model: "test-model"}
	s, err := New(store, llm.New(cfg), nil, nil, nil, chats, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore)
	f := buildFixture(t, s)
	return s, &f
}

// newCampaignThread mints a campaign conversation for the cookie's caller.
func newCampaignThread(t *testing.T, s *Server, f fixture, cookie *http.Cookie) string {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/chats", "", cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create campaign chat: status %d, body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	c, ok := body["chat"].(map[string]any)
	if !ok {
		t.Fatalf("no chat in response: %s", rec.Body)
	}
	id, _ := c["id"].(string)
	if id == "" {
		t.Fatalf("chat has no id: %s", rec.Body)
	}
	return id
}

// askCampaign posts a question and returns the SSE body plus status.
func askCampaign(t *testing.T, s *Server, f fixture, cookie *http.Cookie, thread, question string) (int, string) {
	t.Helper()
	body := `{"question":` + quote(question) + `}`
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/chats/"+thread+"/messages", body, cookie)
	return rec.Code, rec.Body.String()
}

// sseFrames splits an SSE body into its event payloads.
func campaignSSEFrames(body string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, frame := range strings.Split(body, "\n\n") {
		event, data := "", ""
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if event == "" || data == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err == nil {
			out[event] = payload
		}
	}
	return out
}

// TestPlayerPromptNeverContainsSecret is the acceptance test this issue
// exists for: the party HOLDS a granting awareness row on the secret, yet the
// answer must be built from a context that provably never contained it. The
// assertion runs on the assembled prompt (the request body the model
// received), not just the output.
func TestPlayerPromptNeverContainsSecret(t *testing.T) {
	stub := newCapturingLLM(t, "You don't know that.")
	s, f := newCampaignChatServer(t, stub)
	player := addPlayerMember(t, s, *f, "mira", true)
	thread := newCampaignThread(t, s, *f, player)

	code, body := askCampaign(t, s, *f, player, thread, "What is the Duke's dark secret?")
	if code != http.StatusOK {
		t.Fatalf("player ask: status %d, body %s", code, body)
	}

	prompt := stub.lastBody()
	if prompt == "" {
		t.Fatal("the model was never called")
	}
	if strings.Contains(prompt, "vampire") {
		t.Fatal("LEAK: the assembled player prompt contains the secret's text")
	}
	if strings.Contains(prompt, f.secretID) {
		t.Fatal("LEAK: the assembled player prompt references the secret fact")
	}
	// The prompt is player-shaped: it must carry the don't-know discipline
	// and never the DM's "secrets included" framing.
	if !strings.Contains(prompt, "do not know") && !strings.Contains(prompt, "do not know that") {
		t.Fatal("the player system prompt lacks the don't-know discipline")
	}
	if strings.Contains(prompt, "secrets included") {
		t.Fatal("the player prompt carries the DM framing")
	}

	// The citations riding the meta frame must not carry the secret either.
	frames := campaignSSEFrames(body)
	meta, ok := frames["meta"]
	if !ok {
		t.Fatalf("no meta frame in SSE body: %s", body)
	}
	camp, _ := meta["campaign"].(map[string]any)
	facts, _ := camp["facts"].([]any)
	for _, raw := range facts {
		fact, _ := raw.(map[string]any)
		if fact["id"] == f.secretID {
			t.Fatal("LEAK: the player's citations include the secret fact")
		}
	}
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/chats/"+thread, "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("get thread: status %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "vampire") || strings.Contains(rec.Body.String(), f.secretID) {
		t.Fatal("LEAK: the persisted thread carries the secret")
	}
}

// TestDMSeesTheSecret proves the fixture has teeth: the same question at the
// DM's scope grounds the secret and marks it.
func TestDMSeesTheSecret(t *testing.T) {
	stub := newCapturingLLM(t, "He is a vampire.")
	s, f := newCampaignChatServer(t, stub)
	dm := dmSession(t, s)
	thread := newCampaignThread(t, s, *f, dm)

	code, body := askCampaign(t, s, *f, dm, thread, "What is the Duke's dark secret?")
	if code != http.StatusOK {
		t.Fatalf("dm ask: status %d, body %s", code, body)
	}
	prompt := stub.lastBody()
	if !strings.Contains(prompt, "vampire") {
		t.Fatal("fixture integrity: the DM prompt does not contain the secret; the player test is vacuous")
	}
	if !strings.Contains(prompt, "(secret)") {
		t.Fatal("the DM grounding does not mark the secret")
	}
	if !strings.Contains(prompt, "secrets included") {
		t.Fatal("the DM system prompt lacks the DM framing")
	}

	// Provenance rides the DM's citations — "cite your sources" reaches the
	// DM's own world.
	frames := campaignSSEFrames(body)
	meta, _ := frames["meta"]
	camp, _ := meta["campaign"].(map[string]any)
	facts, _ := camp["facts"].([]any)
	found := false
	for _, raw := range facts {
		fact, _ := raw.(map[string]any)
		if fact["id"] == f.secretID {
			found = true
			if _, has := fact["provenance"]; !has {
				t.Error("the DM's secret citation carries no provenance")
			}
		}
	}
	if !found {
		t.Error("the DM's citations do not include the secret fact")
	}
}

// TestPlayerGroundsWhatThePartyKnows: the public fact the party holds is
// grounded into the player's prompt — the Player Grimoire answers, it just
// answers a smaller world.
func TestPlayerGroundsWhatThePartyKnows(t *testing.T) {
	stub := newCapturingLLM(t, "He rules the northern marches.")
	s, f := newCampaignChatServer(t, stub)
	player := addPlayerMember(t, s, *f, "mira", true)
	thread := newCampaignThread(t, s, *f, player)

	code, _ := askCampaign(t, s, *f, player, thread, "What do we know about the Duke's lands?")
	if code != http.StatusOK {
		t.Fatalf("player ask failed: %d", code)
	}
	prompt := stub.lastBody()
	if !strings.Contains(prompt, "The Duke rules the northern marches.") {
		t.Fatalf("the player prompt does not ground the party-known public fact:\n%s", prompt)
	}
}

// TestScopePinnedOnTheThread: a thread opened at one perspective refuses to
// continue at another. Rebinding the member's character (or promoting them)
// changes their resolved scope; the old thread must not answer at the new,
// wider view.
func TestScopePinnedOnTheThread(t *testing.T) {
	stub := newCapturingLLM(t, "ok")
	s, f := newCampaignChatServer(t, stub)
	player := addPlayerMember(t, s, *f, "mira", false) // no character: party scope
	thread := newCampaignThread(t, s, *f, player)

	// Sanity: the thread answers at the party scope.
	code, _ := askCampaign(t, s, *f, player, thread, "What do we know about the Duke?")
	if code != http.StatusOK {
		t.Fatalf("party-scope ask failed: %d", code)
	}

	// The DM binds the member to the pc: their scope becomes character:<id>.
	dm := dmSession(t, s)
	uid, err := s.users.LookupUseridByName(t.Context(), "mira")
	if err != nil {
		t.Fatalf("lookup player: %v", err)
	}
	body := `{"character_id":` + quote(f.pcID) + `}`
	if rec := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/members/"+uid, body, dm); rec.Code != http.StatusOK {
		t.Fatalf("bind character: status %d, body %s", rec.Code, rec.Body)
	}

	code, resp := askCampaign(t, s, *f, player, thread, "and his armies?")
	if code != http.StatusForbidden {
		t.Fatalf("a thread opened at party scope continued at character scope: status %d, body %s", code, resp)
	}
	// A fresh thread at the new scope works fine.
	thread2 := newCampaignThread(t, s, *f, player)
	if code, _ := askCampaign(t, s, *f, player, thread2, "What do we know about the Duke?"); code != http.StatusOK {
		t.Fatalf("new thread at character scope failed: %d", code)
	}
}

// TestCrossCampaignThreadIsInvisible: a thread id from one campaign does not
// answer under another, even for the same owner.
func TestCrossCampaignThreadIsInvisible(t *testing.T) {
	stub := newCapturingLLM(t, "ok")
	s, f := newCampaignChatServer(t, stub)
	dm := dmSession(t, s)
	thread := newCampaignThread(t, s, *f, dm)

	// A second campaign under the same DM.
	rec := hit(t, s, http.MethodPost, "/api/campaigns", `{"name":"Elsewhere","system":"D&D 5e"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create campaign: status %d, body %s", rec.Code, rec.Body)
	}
	other := idFrom(t, rec, "campaign")

	req := hit(t, s, http.MethodPost, "/api/campaigns/"+other+"/chats/"+thread+"/messages", `{"question":"hello?"}`, dm)
	if req.Code != http.StatusNotFound {
		t.Fatalf("cross-campaign thread: status %d, want 404", req.Code)
	}
}

// TestRulesChatRefusesCampaignThreads: the rules chat path must not answer a
// campaign thread — it has no campaign grounding at all.
func TestRulesChatRefusesCampaignThreads(t *testing.T) {
	stub := newCapturingLLM(t, "ok")
	s, f := newCampaignChatServer(t, stub)
	dm := dmSession(t, s)
	thread := newCampaignThread(t, s, *f, dm)

	rec := hit(t, s, http.MethodPost, "/api/chats/"+thread+"/messages", `{"question":"hello?"}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rules chat answering a campaign thread: status %d, want 400", rec.Code)
	}
}

// TestRulesSidebarExcludesCampaignThreads: the campaign surface has its own
// thread list; the rules sidebar must not leak campaign threads into it (and
// vice versa).
func TestRulesSidebarExcludesCampaignThreads(t *testing.T) {
	stub := newCapturingLLM(t, "ok")
	s, f := newCampaignChatServer(t, stub)
	dm := dmSession(t, s)

	// A rules conversation and a campaign conversation.
	if rec := hit(t, s, http.MethodPost, "/api/chats", `{"corpus":"dnd"}`, dm); rec.Code != http.StatusCreated {
		t.Fatalf("create rules chat: status %d", rec.Code)
	}
	thread := newCampaignThread(t, s, *f, dm)
	code, _ := askCampaign(t, s, *f, dm, thread, "Who is the Duke?")
	if code != http.StatusOK {
		t.Fatalf("campaign ask failed: %d", code)
	}

	rec := hit(t, s, http.MethodGet, "/api/chats", "", dm)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, raw := range body["chats"].([]any) {
		c := raw.(map[string]any)
		if c["id"] == thread {
			t.Fatal("the rules sidebar lists a campaign thread")
		}
	}

	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/chats", "", dm)
	body = map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range body["chats"].([]any) {
		c := raw.(map[string]any)
		if c["id"] == thread {
			found = true
			if c["scope"] != "dm" || c["campaign_id"] != f.campaignID {
				t.Fatalf("campaign thread missing its pin: %v", c)
			}
		}
	}
	if !found {
		t.Fatal("the campaign thread list does not include the campaign thread")
	}
}

// TestPlayerEmptyRetrievalSaysUnknown: the player asks about something the
// party has no record of at all. The prompt must carry the empty-record
// marker, and the citations must be empty of campaign facts.
func TestPlayerEmptyRetrievalSaysUnknown(t *testing.T) {
	stub := newCapturingLLM(t, "You don't know that yet.")
	s, f := newCampaignChatServer(t, stub)
	player := addPlayerMember(t, s, *f, "mira", true)
	thread := newCampaignThread(t, s, *f, player)

	code, body := askCampaign(t, s, *f, player, thread, "Where is the Black Sun headquarters?")
	if code != http.StatusOK {
		t.Fatalf("player ask failed: %d", code)
	}
	prompt := stub.lastBody()
	if !strings.Contains(prompt, "(nothing") {
		t.Fatalf("the empty player record is not marked in the prompt:\n%s", prompt)
	}
	frames := campaignSSEFrames(body)
	meta, _ := frames["meta"]
	camp, _ := meta["campaign"].(map[string]any)
	if facts, _ := camp["facts"].([]any); len(facts) != 0 {
		t.Fatalf("expected no campaign facts in the player's citations, got %d", len(facts))
	}
}
