package server

// The canon engine's HTTP surface: the DM runs the deterministic checks and
// decides flags; a player cannot. Assertions are on HTTP responses, in the
// package's standing style.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// newCanonServer builds a server with accounts, the campaign graph, the
// session layer, the knowledge layer and the offline canon engine wired,
// with the keeper signed in.
func newCanonServer(t *testing.T) *Server {
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
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore)
	s = s.WithCampaign(campaigns, sessions)
	s = s.WithCanon(engine)
	return s
}

func canonFlags(t *testing.T, rec *recorder) []flagView {
	t.Helper()
	var body struct {
		Flags []flagView `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode flags: %v (%s)", err, rec.Body)
	}
	return body.Flags
}

func TestCanonCheckDMOnly(t *testing.T) {
	s := newCanonServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	// The DM runs the check. The fixture's awareness rows were written
	// through the API without discoveries, so the engine must report
	// awareness_without_source on both facts.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/check", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("check: status %d, body %s", rec.Code, rec.Body)
	}
	flags := canonFlags(t, rec)
	var withoutSource int
	for _, fl := range flags {
		if fl.CheckCode == canon.CheckAwarenessWithoutSource && fl.Status == canon.FlagOpen {
			withoutSource++
		}
	}
	if withoutSource != 2 {
		t.Fatalf("awareness_without_source findings: got %d, want 2 (flags: %+v)", withoutSource, flags)
	}

	// A second run is idempotent on the ledger: same flags, still open.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/check", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-check: status %d", rec.Code)
	}
	if got := len(canonFlags(t, rec)); got != len(flags) {
		t.Fatalf("re-check changed the flag count: %d -> %d", len(flags), got)
	}

	// A player cannot run the engine: it walks every secret.
	player := addPlayerMember(t, s, f, "mira", true)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/check", "", player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player check: status %d, want 403", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/flags", "", player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player flags: status %d, want 403", rec.Code)
	}

	// Unwired: the endpoints report unavailable, not a panic.
	s.canon = nil
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/check", "", dm)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired check: status %d, want 503", rec.Code)
	}
}

func TestCanonFlagListAndDecision(t *testing.T) {
	s := newCanonServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/check", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("check: status %d, body %s", rec.Code, rec.Body)
	}

	// Status filter: open returns the current findings, cleared returns none.
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/flags?status=open", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("open flags: status %d", rec.Code)
	} else if n := len(canonFlags(t, rec)); n != 2 {
		t.Fatalf("open flags: got %d, want 2", n)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/flags?status=cleared", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("cleared flags: status %d", rec.Code)
	} else if n := len(canonFlags(t, rec)); n != 0 {
		t.Fatalf("cleared flags: got %d, want 0", n)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/canon/flags?status=bogus", "", dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus status: %d, want 400", rec.Code)
	}

	// Decide one flag: accepted, with a note. The other stays open.
	body := fmt.Sprintf(`{"check_code":%q,"record_kind":"awareness","record_id":"party/%s","decision":"accepted","note":"dm told them at the table"}`,
		canon.CheckAwarenessWithoutSource, f.publicID)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/flags/decision", body, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", rec.Code, rec.Body)
	}
	var accepted int
	for _, fl := range canonFlags(t, rec) {
		if fl.Status == canon.FlagAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted flags: got %d, want 1", accepted)
	}

	// A re-run never clobbers the decision.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/check", "", dm); rec.Code != http.StatusOK {
		t.Fatalf("re-check: status %d", rec.Code)
	}
	for _, fl := range canonFlags(t, rec) {
		if fl.RecordID == "party/"+f.publicID && fl.Status != canon.FlagAccepted {
			t.Fatalf("decision clobbered by re-run: %+v", fl)
		}
	}

	// Deciding again is a 400, not an overwrite.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/flags/decision", body, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("re-decide: status %d, want 400", rec.Code)
	}
	// A decision outside the vocabulary is a 400.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/flags/decision",
		fmt.Sprintf(`{"check_code":%q,"record_kind":"awareness","record_id":"party/%s","decision":"maybe"}`,
			canon.CheckAwarenessWithoutSource, f.secretID), dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus decision: status %d, want 400", rec.Code)
	}
	// A missing flag is a 404.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/flags/decision",
		`{"check_code":"no_such_check","record_kind":"fact","record_id":"nope","decision":"accepted"}`, dm)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing flag: status %d, want 404", rec.Code)
	}
	// A player cannot decide either.
	player := addPlayerMember(t, s, f, "thalia", false)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/flags/decision", body, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player decide: status %d, want 403", rec.Code)
	}
}

/* ---------- continuity, entailment, health (MAD-312) ---------- */

// canonContinuityFindings decodes a continuity response's finding lists.
func canonContinuityFindings(t *testing.T, rec *recorder) (findings, modelFindings []findingView, offline bool) {
	t.Helper()
	var body struct {
		Offline       bool          `json:"offline"`
		Findings      []findingView `json:"findings"`
		ModelFindings []findingView `json:"model_findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode continuity: %v (%s)", err, rec.Body)
	}
	return body.Findings, body.ModelFindings, body.Offline
}

func TestCanonContinuity(t *testing.T) {
	s := newCanonServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	// Table history so the unheard-name join is armed, and an npc the party
	// has never heard a fact about.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/sessions", `{"name":"Session 1"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"npc","name":"Brother Venn"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create npc: status %d, body %s", rec.Code, rec.Body)
	}

	// The offline engine catches the unheard name with no model involved.
	prep := `{"title":"Session 2","scenes":[{"name":"The chapel","cast":[{"ref":"Brother Venn","state":"alive"}]}]}`
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/continuity", prep, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("continuity: status %d, body %s", rec.Code, rec.Body)
	}
	findings, modelFindings, offline := canonContinuityFindings(t, rec)
	if !offline {
		t.Fatal("the offline engine must report offline=true")
	}
	if len(modelFindings) != 0 {
		t.Fatalf("offline engine must not produce model findings: %+v", modelFindings)
	}
	var unheard int
	for _, fv := range findings {
		if fv.Check == canon.CheckPrepUnheardName {
			unheard++
		}
	}
	if unheard != 1 {
		t.Fatalf("unheard name findings: got %d, want 1 (%+v)", unheard, findings)
	}

	// A player cannot run the continuity check: it reads the whole graph.
	player := addPlayerMember(t, s, f, "mira", true)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/continuity", prep, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player continuity: status %d, want 403", rec.Code)
	}
}

func TestCanonEntail(t *testing.T) {
	s := newCanonServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	// Prose resting on the record: the Duke is a vampire (secret fact).
	prose := `{"prose":"The Duke is secretly a vampire, and the Ashen Court made him."}`
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/entail", prose, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("entail: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Offline  bool          `json:"offline"`
		Records  []struct{}    `json:"records"`
		Findings []findingView `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode entail: %v (%s)", err, rec.Body)
	}
	if !body.Offline {
		t.Fatal("the offline engine must report offline=true")
	}
	if len(body.Records) == 0 {
		t.Fatal("auto-selection must pull the Duke's facts as records")
	}
	var unbacked int
	for _, fv := range body.Findings {
		if fv.Check == canon.CheckUnbackedName {
			unbacked++
		}
	}
	if unbacked != 1 {
		t.Fatalf("unbacked name: got %d, want 1 (%+v)", unbacked, body.Findings)
	}

	// Prose is required.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/entail", `{"prose":"  "}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty prose: status %d, want 400", rec.Code)
	}

	// A player cannot run the entailment pass: it reads secrets.
	player := addPlayerMember(t, s, f, "mira", true)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/entail", prose, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player entail: status %d, want 403", rec.Code)
	}
}

func TestCanonHealth(t *testing.T) {
	s := newCanonServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	// Table history so the unused-entity checks arm, plus an npc no live
	// fact, relationship or event touches.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/sessions", `{"name":"Session 1"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"npc","name":"Brother Venn"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create npc: status %d, body %s", rec.Code, rec.Body)
	}

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/health", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("health: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Offline    bool   `json:"offline"`
		OpenFlags  int    `json:"open_flags"`
		Narrative  string `json:"narrative"`
		UnusedNPCs []struct {
			Name string `json:"name"`
		} `json:"unused_npcs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v (%s)", err, rec.Body)
	}
	if !body.Offline || body.Narrative != "" {
		t.Fatalf("offline engine: offline=%v narrative=%q", body.Offline, body.Narrative)
	}
	if len(body.UnusedNPCs) != 1 || body.UnusedNPCs[0].Name != "Brother Venn" {
		t.Fatalf("the untouched npc must appear as unused: %+v", body.UnusedNPCs)
	}
	if body.OpenFlags == 0 {
		t.Fatal("the report must count open flags")
	}

	// A player cannot pull the health report: it walks every secret.
	player := addPlayerMember(t, s, f, "mira", true)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/canon/health", "", player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player health: status %d, want 403", rec.Code)
	}
}
