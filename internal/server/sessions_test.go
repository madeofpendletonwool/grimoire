package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newSessionServer builds a server with accounts, the campaign graph, the
// knowledge layer, and the session layer wired, with the keeper signed in.
// Migrations run explicitly: index.Open applies the legacy schema, the
// campaign and session tables come from goose.
func newSessionServer(t *testing.T) *Server {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
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
	signIn(t, s, "keeper", "a-perfectly-fine-passphrase")
	return s
}

// newCampaignAPI creates a campaign through the API and returns its id.
func newCampaignAPI(t *testing.T, s *Server, name string) string {
	t.Helper()
	code, body := do(t, s, http.MethodPost, "/api/campaigns", fmt.Sprintf(`{"name":%q,"system":"dnd5e"}`, name))
	if code != http.StatusCreated {
		t.Fatalf("create campaign: status %d, body %v", code, body)
	}
	c, _ := body["campaign"].(map[string]any)
	id, _ := c["id"].(string)
	if id == "" {
		t.Fatal("campaign id missing from response")
	}
	return id
}

// newSessionAPI creates a session through the API and returns its id.
func newSessionAPI(t *testing.T, s *Server, campaignID, name string) string {
	t.Helper()
	code, body := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions", campaignID), fmt.Sprintf(`{"name":%q}`, name))
	if code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %v", code, body)
	}
	ses, _ := body["session"].(map[string]any)
	id, _ := ses["id"].(string)
	if id == "" {
		t.Fatal("session id missing from response")
	}
	return id
}

func TestCampaignsUnwiredReportUnavailable(t *testing.T) {
	s := newSessionServer(t)
	s.campaigns = nil
	if code, _ := do(t, s, http.MethodGet, "/api/campaigns", ""); code != http.StatusServiceUnavailable {
		t.Errorf("unwired list: status %d, want 503", code)
	}
}

func TestCampaignListCreateRoundTrip(t *testing.T) {
	s := newSessionServer(t)
	id := newCampaignAPI(t, s, "The Withering Kingdom")

	code, body := do(t, s, http.MethodGet, "/api/campaigns", "")
	if code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	list, _ := body["campaigns"].([]any)
	if len(list) != 1 {
		t.Fatalf("campaigns = %d, want 1", len(list))
	}
	got, _ := list[0].(map[string]any)
	if got["id"] != id || got["my_role"] != "dm" || got["name"] != "The Withering Kingdom" {
		t.Errorf("campaign view = %v", got)
	}

	// A nameless campaign is rejected, not defaulted: campaign creation is
	// the one place there is nothing to number.
	if code, _ := do(t, s, http.MethodPost, "/api/campaigns", `{}`); code != http.StatusBadRequest {
		t.Errorf("nameless campaign: status %d, want 400", code)
	}
}

func TestSessionLifecycleThroughAPI(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "The Ambush")

	code, body := do(t, s, http.MethodGet, fmt.Sprintf("/api/campaigns/%s/sessions", cid), "")
	if code != http.StatusOK {
		t.Fatalf("list sessions: %d", code)
	}
	list, _ := body["sessions"].([]any)
	if len(list) != 1 {
		t.Fatalf("sessions = %d, want 1", len(list))
	}

	// Start it, finish it — the timestamps record themselves.
	code, body = do(t, s, http.MethodPatch,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s", cid, sid), `{"status":"live"}`)
	if code != http.StatusOK {
		t.Fatalf("go live: %d %v", code, body)
	}
	ses, _ := body["session"].(map[string]any)
	if ses["status"] != "live" || ses["started_at"] == "" {
		t.Fatalf("live view = %v", ses)
	}
	code, _ = do(t, s, http.MethodPatch,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s", cid, sid), `{"status":"done"}`)
	if code != http.StatusOK {
		t.Fatalf("finish: %d", code)
	}
}

func TestSessionBelongsToItsCampaign(t *testing.T) {
	s := newSessionServer(t)
	one := newCampaignAPI(t, s, "one")
	two := newCampaignAPI(t, s, "two")
	sid := newSessionAPI(t, s, one, "The Ambush")

	// The session is simply not there under the other campaign's path.
	if code, _ := do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s", two, sid), ""); code != http.StatusNotFound {
		t.Errorf("cross-campaign read: status %d, want 404", code)
	}
	if code, _ := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources", two, sid),
		`{"kind":"transcript","content":"x"}`); code != http.StatusNotFound {
		t.Errorf("cross-campaign write: status %d, want 404", code)
	}
}

/* ---------- sources, spans, export ---------- */

const apiTranscript = "DM: The door grinds open.\nMira: I check for traps."

func addTranscript(t *testing.T, s *Server, cid, sid string) string {
	t.Helper()
	code, body := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources", cid, sid),
		fmt.Sprintf(`{"kind":"transcript","author":"DM","title":"the recording","content":%q}`, apiTranscript))
	if code != http.StatusCreated {
		t.Fatalf("paste source: status %d, body %v", code, body)
	}
	src, _ := body["source"].(map[string]any)
	id, _ := src["id"].(string)
	return id
}

func TestPasteSourceResolveSpanExport(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "The Ambush")
	srcID := addTranscript(t, s, cid, sid)

	// Resolve an arbitrary span back to its quoted words — by offsets, and
	// by quote back to offsets.
	start := strings.Index(apiTranscript, "I check for traps")
	end := start + len("I check for traps")
	code, body := do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/span?source_id=%s&start=%d&end=%d",
			cid, sid, srcID, start, end), "")
	if code != http.StatusOK {
		t.Fatalf("resolve span: %d %v", code, body)
	}
	span, _ := body["span"].(map[string]any)
	if span["quote"] != "I check for traps" {
		t.Fatalf("span = %v", span)
	}
	code, body = do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/span?source_id=%s&quote=%s",
			cid, sid, srcID, url.QueryEscape("door grinds open")), "")
	if code != http.StatusOK {
		t.Fatalf("locate span: %d %v", code, body)
	}
	span, _ = body["span"].(map[string]any)
	if span["start"].(float64) < 0 || span["quote"] != "door grinds open" {
		t.Fatalf("located = %v", span)
	}

	// Out-of-bounds offsets are a 400 with a clear message, never a quote.
	if code, _ := do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/span?source_id=%s&start=0&end=99999",
			cid, sid, srcID), ""); code != http.StatusBadRequest {
		t.Errorf("out-of-bounds span: status %d, want 400", code)
	}

	// The export carries the verbatim source (body asserted in
	// TestExportBodyIsMarkdown).
	if code, _ := do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/export", cid, sid), ""); code != http.StatusOK {
		t.Errorf("export: %d", code)
	}
}

func TestExportBodyIsMarkdown(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "The Ambush")
	addTranscript(t, s, cid, sid)

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/export", cid, sid), nil)
	req.AddCookie(sessions[s])
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("content-type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("content-disposition"); !strings.Contains(cd, "session-01") {
		t.Errorf("content-disposition = %q", cd)
	}
	if !strings.Contains(rec.Body.String(), "I check for traps") {
		t.Error("export lost the transcript")
	}
}

// uploadSource posts a file as multipart, the way the UI's upload button
// does.
func uploadSource(t *testing.T, s *Server, cid, sid, filename, kind, content string) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("kind", kind)
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources", cid, sid), &buf)
	req.Header.Set("content-type", mw.FormDataContentType())
	req.AddCookie(sessions[s])
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestUploadSRTProducesTimedSource(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "The Crypt")

	srt := "1\n00:00:01,000 --> 00:00:04,000\nDM: The door grinds open.\n\n2\n00:00:05,500 --> 00:00:09,250\nMira: I check for traps.\n"
	code, body := uploadSource(t, s, cid, sid, "session-12.srt", "transcript", srt)
	if code != http.StatusCreated {
		t.Fatalf("upload srt: status %d, body %v", code, body)
	}
	src, _ := body["source"].(map[string]any)
	srcID, _ := src["id"].(string)
	if src["timed"] != true || src["title"] != "session-12.srt" {
		t.Fatalf("source view = %v", src)
	}

	// The parsed text is what spans address: cue text, not SRT markup.
	code, body = do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources/%s", cid, sid, srcID), "")
	if code != http.StatusOK {
		t.Fatalf("get source: %d", code)
	}
	full, _ := body["source"].(map[string]any)
	content, _ := full["content"].(string)
	if !strings.Contains(content, "Mira: I check for traps.") || strings.Contains(content, "-->") {
		t.Fatalf("parsed content = %q", content)
	}
	timing, _ := body["timing"].([]any)
	if len(timing) != 2 {
		t.Fatalf("timing = %v", timing)
	}

	// A span into the parsed text resolves to real words.
	start := strings.Index(content, "I check for traps")
	code, body = do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/span?source_id=%s&start=%d&end=%d",
			cid, sid, srcID, start, start+len("I check for traps")), "")
	if code != http.StatusOK {
		t.Fatalf("span into parsed text: %d", code)
	}
}

func TestUploadRejectsNonTextFormats(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")
	// A .srt-named file with no cues fails loudly rather than storing junk.
	if code, _ := uploadSource(t, s, cid, sid, "x.srt", "transcript", "not a subtitle file at all"); code != http.StatusBadRequest {
		t.Errorf("junk srt: status %d, want 400", code)
	}
	// An unknown kind is a 400.
	if code, _ := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources", cid, sid),
		`{"kind":"podcast","content":"x"}`); code != http.StatusBadRequest {
		t.Errorf("bad kind: status %d, want 400", code)
	}
}

/* ---------- events + prior rulings ---------- */

func TestRulingSurfacesPriorRulingOverAPI(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	first := newSessionAPI(t, s, cid, "The Ambush")
	later := newSessionAPI(t, s, cid, "The Crypt")

	code, _ := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/events", cid, first),
		`{"kind":"ruling","summary":"Does hiding work in dim light?","detail":"Ruled yes."}`)
	if code != http.StatusCreated {
		t.Fatalf("first ruling: %d", code)
	}

	code, body := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/events", cid, later),
		`{"kind":"ruling","summary":"Can you hide in dim light after attacking?","detail":"Ruled no this time."}`)
	if code != http.StatusCreated {
		t.Fatalf("second ruling: %d %v", code, body)
	}
	matches, _ := body["prior_matches"].([]any)
	if len(matches) == 0 {
		t.Fatal("prior ruling was not surfaced — the MAD-286 feature")
	}
	m, _ := matches[0].(map[string]any)
	if m["summary"] != "Does hiding work in dim light?" {
		t.Errorf("top match = %v", m)
	}
	if m["session_ordinal"].(float64) != 1 {
		t.Errorf("match session = %v", m["session_ordinal"])
	}
}

func TestEventLogListsInOrder(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	for _, body := range []string{
		`{"kind":"note","detail":"opened the door"}`,
		`{"kind":"discovery","summary":"the cult sigil","payload":{"who":"party","how":"searched the altar"}}`,
		`{"kind":"qa","summary":"How far can Mira see?","detail":"60ft darkvision."}`,
	} {
		if code, _ := do(t, s, http.MethodPost,
			fmt.Sprintf("/api/campaigns/%s/sessions/%s/events", cid, sid), body); code != http.StatusCreated {
			t.Fatalf("add event: %q", body)
		}
	}
	code, body := do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/events", cid, sid), "")
	if code != http.StatusOK {
		t.Fatalf("list events: %d", code)
	}
	events, _ := body["events"].([]any)
	if len(events) != 3 {
		t.Fatalf("events = %d", len(events))
	}
	last, _ := events[2].(map[string]any)
	if last["seq"].(float64) != 3 || last["kind"] != "qa" {
		t.Errorf("ordering broke: %v", last)
	}
	discovery, _ := events[1].(map[string]any)
	if discovery["payload"].(map[string]any)["who"] != "party" {
		t.Errorf("payload = %v", discovery["payload"])
	}
}

/* ---------- role enforcement ---------- */

func TestPlayerSeesLessThanTheDM(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "The Ambush")

	// The DM ingests a shared transcript and private notes.
	addTranscript(t, s, cid, sid)
	code, body := do(t, s, http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources", cid, sid),
		`{"kind":"dm_notes","author":"DM","content":"the vampire secret"}`)
	if code != http.StatusCreated {
		t.Fatalf("dm notes: %d %v", code, body)
	}
	notes, _ := body["source"].(map[string]any)
	notesID, _ := notes["id"].(string)

	// Register the player for real: username -> user id through the store.
	admin := adminSession2(t, s)
	inv := createInvite(t, s, admin, "")
	code0, _ := inv["code"].(string)
	rec := call(s, http.MethodPost, "/api/auth/register", registerJSON("mira", "a-fine-passphrase", code0))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register player: %d %s", rec.Code, rec.Body)
	}
	player := sessionFrom(t, rec)
	// Resolve the username to the id the membership table foreign-keys.
	u, err := s.users.Authenticate(t.Context(), "mira", "a-fine-passphrase")
	if err != nil {
		t.Fatalf("authenticate player: %v", err)
	}
	if err := s.campaigns.AddMember(t.Context(), cid, u.ID, campaign.RolePlayer, ""); err != nil {
		t.Fatalf("add member: %v", err)
	}

	dmList, body := do(t, s, http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources", cid, sid), "")
	if dmList != http.StatusOK {
		t.Fatalf("dm list: %d", dmList)
	}
	dmSources, _ := body["sources"].([]any)
	if len(dmSources) != 2 {
		t.Fatalf("dm sees %d sources, want 2", len(dmSources))
	}

	// The player lists the same session: only the transcript.
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/sources", cid, sid), nil)
	req.AddCookie(player)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("player list: %d %s", rec2.Code, rec2.Body)
	}
	var pbody map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &pbody)
	pSources, _ := pbody["sources"].([]any)
	if len(pSources) != 1 {
		t.Fatalf("player sees %d sources, want 1 (dm_notes filtered)", len(pSources))
	}

	// The DM's notes are not even addressable for the player: the span
	// endpoint 404s rather than quoting them.
	req = httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/span?source_id=%s&start=0&end=5", cid, sid, notesID), nil)
	req.AddCookie(player)
	rec2 = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("player span into dm_notes: status %d, want 404", rec2.Code)
	}

	// Writes are the DM's seat only.
	req = httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/campaigns/%s/sessions/%s/events", cid, sid), strings.NewReader(`{"kind":"note","detail":"x"}`))
	req.Header.Set("content-type", "application/json")
	req.AddCookie(player)
	rec2 = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("player write: status %d, want 403", rec2.Code)
	}
}

func TestStrangerSeesNoCampaign(t *testing.T) {
	s := newSessionServer(t)
	cid := newCampaignAPI(t, s, "one")
	sid := newSessionAPI(t, s, cid, "one")

	stranger := secondUser(t, s)
	for _, tc := range []struct {
		method, target string
	}{
		{http.MethodGet, fmt.Sprintf("/api/campaigns/%s/sessions", cid)},
		{http.MethodGet, fmt.Sprintf("/api/campaigns/%s/sessions/%s", cid, sid)},
		{http.MethodPost, fmt.Sprintf("/api/campaigns/%s/sessions", cid)},
		{http.MethodGet, fmt.Sprintf("/api/campaigns/%s/sessions/%s/events", cid, sid)},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req.AddCookie(stranger)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404 (membership is the gate)", tc.method, tc.target, rec.Code)
		}
	}
}
