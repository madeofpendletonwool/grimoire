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
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newCanonServer builds a server with accounts, the campaign graph, the
// knowledge layer and the offline canon engine wired, with the keeper signed
// in.
func newCanonServer(t *testing.T) *Server {
	t.Helper()
	store, err := index.Open(t.TempDir() + "/test.db")
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
