package server

// The player journal surface (MAD-489): the write's author binding, the
// read's scoping, the write's reach (no other kind, no other campaign, no
// other member), and the drafting assist's grounding — asserted on the
// assembled prompt the way the campaign chat's leak test is (MAD-311's
// pattern), because the contract being pinned is what the model is handed.

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
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newJournalTestServer boots the campaign, knowledge and session layers
// with no keeper pre-registered, so buildFixture's setup path owns the
// account — the same shape newCampaignServer gives the campaign tests.
func newJournalTestServer(t *testing.T) *Server {
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
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaign(campaigns, sessions)
	s = s.WithCampaigns(campaigns, knowledgeStore)
	return s
}

// newJournalSession creates a session through the API under the DM's cookie.
func newJournalSession(t *testing.T, s *Server, campaignID, name string, dm *http.Cookie) string {
	t.Helper()
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+campaignID+"/sessions", `{"name":`+quote(name)+`}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	ses, _ := body["session"].(map[string]any)
	id, _ := ses["id"].(string)
	if id == "" {
		t.Fatalf("session id missing: %s", rec.Body)
	}
	return id
}

func TestJournalWriteBindsAuthorAndKindServerSide(t *testing.T) {
	s := newJournalTestServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	sid := newJournalSession(t, s, f.campaignID, "The one with the merchant", dm)
	base := "/api/campaigns/" + f.campaignID + "/sessions/" + sid

	player := addPlayerMember(t, s, f, "mira", true)

	// A body that tries to claim another author and another kind: both are
	// ignored — the author is the caller's bound character, the kind is
	// player_journal, and nothing else.
	body := `{"title":"The ledger","content":"We finally discovered the merchant is a vampire. The dust never settled.","author":"not-mira","kind":"transcript"}`
	rec := hit(t, s, http.MethodPost, base+"/journal", body, player)
	if rec.Code != http.StatusCreated {
		t.Fatalf("player journal write: status %d, body %s", rec.Code, rec.Body)
	}
	entry := journalEntryFrom(t, rec)
	if entry["author"] != f.pcID {
		t.Fatalf("author = %v; want the bound character %s (never client-claimed)", entry["author"], f.pcID)
	}
	if entry["kind"] != "player_journal" {
		t.Fatalf("kind = %v; a journal write can produce no other kind", entry["kind"])
	}
	if entry["author_name"] == "" {
		t.Fatalf("author_name missing from %v", entry)
	}

	// The row itself carries the binding: author is the character entity id.
	var author, kind string
	if err := s.sessions.DB().QueryRow(
		`SELECT kind, author FROM session_sources WHERE id = ?`, entry["id"]).Scan(&kind, &author); err != nil {
		t.Fatal(err)
	}
	if author != f.pcID || kind != "player_journal" {
		t.Fatalf("stored row = %s/%s; want player_journal/%s", kind, author, f.pcID)
	}

	// The write's reach. An unbound member has no journal to write.
	unbound := addPlayerMember(t, s, f, "wanderer", false)
	if rec := hit(t, s, http.MethodPost, base+"/journal", `{"content":"x"}`, unbound); rec.Code != http.StatusForbidden {
		t.Fatalf("unbound member's journal write: status %d, want 403", rec.Code)
	}
	// The DM's material has the sources route; this one is the player's.
	if rec := hit(t, s, http.MethodPost, base+"/journal", `{"content":"x"}`, dm); rec.Code != http.StatusForbidden {
		t.Fatalf("dm journal write: status %d, want 403", rec.Code)
	}
	// Another campaign's session is a plain 404, not a write.
	otherRec := hit(t, s, http.MethodPost, "/api/campaigns", `{"name":"elsewhere","system":"dnd5e"}`, dm)
	if otherRec.Code != http.StatusCreated {
		t.Fatalf("other campaign: %d %s", otherRec.Code, otherRec.Body)
	}
	other := idFrom(t, otherRec, "campaign")
	otherSid := newJournalSession(t, s, other, "their session", dm)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+other+"/sessions/"+otherSid+"/journal", `{"content":"x"}`, player); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign campaign's session: status %d, want 404", rec.Code)
	}
	// Empty content is refused, the same rule every source follows.
	if rec := hit(t, s, http.MethodPost, base+"/journal", `{"content":"   "}`, player); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty journal write: status %d, want 400", rec.Code)
	}
}

func TestJournalReadScoping(t *testing.T) {
	s := newJournalTestServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	sid := newJournalSession(t, s, f.campaignID, "one", dm)
	base := "/api/campaigns/" + f.campaignID + "/sessions/" + sid

	// Two bound players on two characters.
	mira := addPlayerMember(t, s, f, "mira", true)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"pc","name":"Thalia"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second pc: status %d %s", rec.Code, rec.Body)
	}
	thaliaID := idFrom(t, rec, "entity")
	thalia := addPlayerMemberNamed(t, s, f, "thalia", thaliaID)

	mine := hit(t, s, http.MethodPost, base+"/journal", `{"title":"mine","content":"The dust never settled."}`, mira)
	if mine.Code != http.StatusCreated {
		t.Fatalf("mira write: %d %s", mine.Code, mine.Body)
	}
	mineID := journalEntryFrom(t, mine)["id"].(string)
	theirs := hit(t, s, http.MethodPost, base+"/journal", `{"title":"theirs","content":"The shield held."}`, thalia)
	if theirs.Code != http.StatusCreated {
		t.Fatalf("thalia write: %d %s", theirs.Code, theirs.Body)
	}
	theirsID := journalEntryFrom(t, theirs)["id"].(string)

	// A player's session list: exactly their own, marked by name.
	var body map[string]any
	rec = hit(t, s, http.MethodGet, base+"/journal", "", mira)
	if rec.Code != http.StatusOK {
		t.Fatalf("mira list: %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("mira sees %d entries; want exactly her own (%v)", len(entries), body)
	}
	first, _ := entries[0].(map[string]any)
	if first["id"] != mineID || first["author_name"] == "" {
		t.Fatalf("mira's entry = %v", first)
	}

	// The campaign-wide read behaves the same.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/journal", "", mira)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	entries, _ = body["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["id"] != mineID {
		t.Fatalf("mira's campaign journal = %v; want her one entry", entries)
	}

	// The DM reads the party's journals against their own.
	rec = hit(t, s, http.MethodGet, base+"/journal", "", dm)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	entries, _ = body["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("dm sees %d entries; want the party's two (%v)", len(entries), body)
	}

	// Another player's journal is not even addressable: the source read and
	// the span resolver both 404, the same indistinguishability the DM's
	// notes get.
	for _, target := range []string{
		"/api/campaigns/" + f.campaignID + "/sessions/" + sid + "/sources/" + theirsID,
		"/api/campaigns/" + f.campaignID + "/sessions/" + sid + "/span?source_id=" + theirsID + "&start=0&end=5",
	} {
		if rec := hit(t, s, http.MethodGet, target, "", mira); rec.Code != http.StatusNotFound {
			t.Errorf("foreign journal via %s: status %d, want 404", target[strings.Index(target, "/sessions"):], rec.Code)
		}
	}
	// And the session source list never hands the row over either.
	rec = hit(t, s, http.MethodGet, base+"/sources", "", mira)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	sources, _ := body["sources"].([]any)
	for _, src := range sources {
		if src.(map[string]any)["id"] == theirsID {
			t.Errorf("LEAK: the session source list handed Mira Thalia's journal")
		}
	}
	// Her own is there, marked by name.
	found := false
	for _, src := range sources {
		if m := src.(map[string]any); m["id"] == mineID {
			found = true
			if m["author_name"] == "" {
				t.Errorf("own journal in source list lacks author_name: %v", m)
			}
		}
	}
	if !found {
		t.Errorf("mira's own journal missing from her session source list: %v", sources)
	}
}

/* ---------- the drafting assist ---------- */

// capturingPromptLLM is a stubbed Anthropic-compatible endpoint that records
// every non-streaming request body, so the test asserts on the exact prompt
// the drafting assist assembled.
type capturingPromptLLM struct {
	mu      sync.Mutex
	bodies  []string
	baseURL string
}

func newJournalServer(t *testing.T, stub *capturingPromptLLM) (*Server, *fixture) {
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
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	cfg := llm.Config{BaseURL: stub.baseURL, APIKey: "test-key", Model: "test-model"}
	s, err := New(store, llm.New(cfg), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaign(campaigns, sessions)
	s = s.WithCampaigns(campaigns, knowledgeStore)
	f := buildFixture(t, s)
	return s, &f
}

func newCapturingPromptLLM(t *testing.T, answer string) *capturingPromptLLM {
	t.Helper()
	c := &capturingPromptLLM{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(raw))
		c.mu.Unlock()
		fmt.Fprintf(w, `{"content":[{"type":"text","text":%q}],"usage":{"input_tokens":10,"output_tokens":20}}`, answer)
	}))
	t.Cleanup(up.Close)
	c.baseURL = up.URL
	return c
}

func (c *capturingPromptLLM) lastBody() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return c.bodies[len(c.bodies)-1]
}

func TestJournalDraftPromptCarriesOnlyTheCharactersRecord(t *testing.T) {
	stub := newCapturingPromptLLM(t, "Dear journal — the ledger troubled me today.")
	s, f := newJournalServer(t, stub)
	dm := dmSession(t, s)
	sid := newJournalSession(t, s, f.campaignID, "one", dm)
	base := "/api/campaigns/" + f.campaignID + "/sessions/" + sid

	player := addPlayerMember(t, s, *f, "mira", true)

	// Give the character a real record: a discovery of the public fact,
	// with the trail the assist should ground in.
	ks := s.knowledge
	pcName := ""
	if e, err := s.campaigns.GetEntity(t.Context(), campaign.ScopeDM, f.campaignID, f.pcID); err == nil {
		pcName = e.Name
	}
	if _, err := ks.RecordDiscovery(t.Context(), knowledge.RecordDiscoveryInput{
		CampaignID: f.campaignID, FactID: f.publicID, DiscoveredBy: f.pcID,
		SessionID: sid, Method: "read the steward's ledger at the Waystone",
		Quote:  "the signature at the bottom is the Duke's steward's",
		Stance: knowledge.StanceKnows, Confidence: 0.9, AcceptedBy: "keeper",
	}); err != nil {
		t.Fatalf("record discovery: %v", err)
	}

	rec := hit(t, s, http.MethodPost, base+"/journal/draft",
		`{"notes":"mention the silver"}`, player)
	if rec.Code != http.StatusOK {
		t.Fatalf("draft: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Draft       string `json:"draft"`
		Perspective string `json:"perspective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Draft != "Dear journal — the ledger troubled me today." {
		t.Fatalf("draft = %q", body.Draft)
	}
	if body.Perspective != pcName || pcName == "" {
		t.Fatalf("perspective = %q; want the character's name", body.Perspective)
	}

	// The assembled prompt: the character's own record rides along, the
	// player's notes ride along, and the secret — which the fixture's
	// party HOLDS a granting awareness row on — provably does not.
	prompt := stub.lastBody()
	for _, want := range []string{
		"read the steward's ledger at the Waystone",
		"the signature at the bottom is the Duke's steward's",
		"mention the silver",
		pcName,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("draft prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "vampire") {
		t.Fatal("LEAK: the drafting assist's prompt contains the secret's text")
	}
	if strings.Contains(prompt, f.secretID) {
		t.Fatal("LEAK: the drafting assist's prompt references the secret fact")
	}

	// The write's own gate: the assist drafts, the player posts. An
	// unbound member gets no assist at all.
	unbound := addPlayerMember(t, s, *f, "wanderer", false)
	if rec := hit(t, s, http.MethodPost, base+"/journal/draft", `{}`, unbound); rec.Code != http.StatusForbidden {
		t.Fatalf("unbound draft: status %d, want 403", rec.Code)
	}
}

/* ---------- helpers ---------- */

func journalEntryFrom(t *testing.T, rec *recorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode journal response: %v (%s)", err, rec.Body)
	}
	entry, ok := body["entry"].(map[string]any)
	if !ok {
		t.Fatalf("no entry in response: %s", rec.Body)
	}
	return entry
}

// addPlayerMemberNamed is addPlayerMember for a pc other than the fixture's
// own, so two players can sit at two characters.
func addPlayerMemberNamed(t *testing.T, s *Server, f fixture, name, characterID string) *http.Cookie {
	t.Helper()
	cookie := addPlayerMember(t, s, f, name, false)
	dm := dmSession(t, s)
	id, err := s.users.LookupUseridByName(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"character_id":` + quote(characterID) + `}`
	if r := hit(t, s, http.MethodPatch, "/api/campaigns/"+f.campaignID+"/members/"+id, body, dm); r.Code != http.StatusOK {
		t.Fatalf("bind character: %d %s", r.Code, r.Body)
	}
	return cookie
}
