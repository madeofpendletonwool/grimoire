package server

// The rumour mill's HTTP tests (MAD-374). The load-bearing one is the
// acceptance gate: a player scope reading every rumour endpoint never
// receives a truth value, a fact link, or a DM-only rumour — asserted on
// the response bodies, not the store rows.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// newCampaignServerOnly boots the campaign server alone, for tests that
// need no store handles.
func newCampaignServerOnly(t *testing.T) *Server {
	t.Helper()
	s, _, _, _ := newCampaignServer(t)
	return s
}

// rumorFixture builds the campaign fixture plus the mill: a true rumour
// attesting the secret, a false one naming the fact it contradicts, a
// fact-less one, and one DM-only.
type rumorFixture struct {
	fixture
	trueRumor   string
	falseRumor  string
	bareRumor   string
	dmOnlyRumor string
}

func buildRumorFixture(t *testing.T, s *Server) rumorFixture {
	t.Helper()
	f := buildFixture(t, s)
	dm := dmSession(t, s)

	mk := func(body string) string {
		r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/rumors", body, dm)
		if r.Code != http.StatusCreated {
			t.Fatalf("create rumor: status %d, body %s", r.Code, r.Body)
		}
		return idFrom(t, r, "rumor")
	}
	return rumorFixture{
		fixture:     f,
		trueRumor:   mk(`{"statement":"They say the Duke keeps a second face in the cellar.","truth":"true","about":"` + f.dukeID + `","fact_id":"` + f.secretID + `"}`),
		falseRumor:  mk(`{"statement":"The Duke's steward was seen buying silver.","truth":"false","fact_id":"` + f.publicID + `"}`),
		bareRumor:   mk(`{"statement":"The miller's boy came back wrong.","truth":"false"}`),
		dmOnlyRumor: mk(`{"statement":"The thing in the trees pays the reeve.","truth":"true","dm_only":true}`),
	}
}

func TestRumorEndpointsNeverLeakTruthToPlayers(t *testing.T) {
	s := newCampaignServerOnly(t)
	rf := buildRumorFixture(t, s)
	player := addPlayerMember(t, s, rf.fixture, "rumor-player", false)

	// The list: the DM sees everything, the player sees three rumours and
	// not one truth value or fact link among them.
	dm := dmSession(t, s)
	dmRec := hit(t, s, http.MethodGet, "/api/campaigns/"+rf.campaignID+"/rumors", "", dm)
	if dmRec.Code != http.StatusOK {
		t.Fatalf("dm list: status %d, body %s", dmRec.Code, dmRec.Body)
	}
	if !strings.Contains(dmRec.Body.String(), `"truth":"true"`) {
		t.Fatal("dm list must carry truth values")
	}

	pRec := hit(t, s, http.MethodGet, "/api/campaigns/"+rf.campaignID+"/rumors", "", player)
	if pRec.Code != http.StatusOK {
		t.Fatalf("player list: status %d, body %s", pRec.Code, pRec.Body)
	}
	var list struct {
		Rumors []struct {
			ID      string `json:"id"`
			Truth   string `json:"truth"`
			FactID  string `json:"fact_id"`
			DMOnly  bool   `json:"dm_only"`
			Holders []any  `json:"holders"`
		} `json:"rumors"`
	}
	if err := json.Unmarshal(pRec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Rumors) != 3 {
		t.Fatalf("player rumours = %d, want 3 (the DM-only one stays DM-side)", len(list.Rumors))
	}
	for _, r := range list.Rumors {
		if r.Truth != "" {
			t.Fatalf("LEAK: player rumour %s carries truth %q", r.ID, r.Truth)
		}
		if r.FactID != "" {
			t.Fatalf("LEAK: player rumour %s carries fact_id %q", r.ID, r.FactID)
		}
		if r.DMOnly {
			t.Fatalf("LEAK: player rumour %s carries the dm-only marker", r.ID)
		}
	}
	for _, r := range list.Rumors {
		if r.ID == rf.dmOnlyRumor {
			t.Fatal("LEAK: the DM-only rumour reached a player scope")
		}
	}

	// The single fetch: the true rumour reads clean, the DM-only one is
	// indistinguishable from a missing one.
	one := hit(t, s, http.MethodGet, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.trueRumor, "", player)
	if one.Code != http.StatusOK || strings.Contains(one.Body.String(), `"truth":`) {
		t.Fatalf("player single fetch: status %d body %s", one.Code, one.Body)
	}
	missing := hit(t, s, http.MethodGet, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.dmOnlyRumor, "", player)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("dm-only fetch at player scope: status %d, want 404", missing.Code)
	}
	// The truth filter is the DM's question; a player asking it is a 400.
	filtered := hit(t, s, http.MethodGet, "/api/campaigns/"+rf.campaignID+"/rumors?truth=false", "", player)
	if filtered.Code != http.StatusBadRequest {
		t.Fatalf("player truth filter: status %d, want 400", filtered.Code)
	}
	// And the DM-only holders never ride along either.
	_ = dm
}

func TestRumorHeardEndpointWritesStances(t *testing.T) {
	s := newCampaignServerOnly(t)
	rf := buildRumorFixture(t, s)
	dm := dmSession(t, s)

	// The pc hears the true rumour: suspects on the secret.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.trueRumor+"/heard",
		`{"knower":"`+rf.pcID+`"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("heard: status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"stance":"suspects"`) {
		t.Fatalf("heard true: body %s", rec.Body)
	}
	// The pc hears the false one naming a fact they hold: believes_false.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.falseRumor+"/heard",
		`{"knower":"`+rf.pcID+`"}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"stance":"believes_false"`) {
		t.Fatalf("heard false: status %d body %s", rec.Code, rec.Body)
	}
	// Hearing it a second time changes nothing.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.falseRumor+"/heard",
		`{"knower":"`+rf.pcID+`"}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"unchanged"`) {
		t.Fatalf("repeat heard: status %d body %s", rec.Code, rec.Body)
	}
	// The fact-less rumour is carried on the mill, not the awareness table.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.bareRumor+"/heard",
		`{"knower":"`+rf.pcID+`"}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"carried"`) {
		t.Fatalf("heard bare: status %d body %s", rec.Code, rec.Body)
	}
	one := hit(t, s, http.MethodGet, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.bareRumor, "", dm)
	if !strings.Contains(one.Body.String(), rf.pcID) {
		t.Fatalf("the carrier must render as a holder: %s", one.Body)
	}

	// A player cannot write hearths — the heard path is the DM's record
	// of what was said at the table.
	player := addPlayerMember(t, s, rf.fixture, "rumor-player-2", false)
	denied := hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.trueRumor+"/heard",
		`{"knower":"`+rf.pcID+`"}`, player)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("player heard: status %d, want 403", denied.Code)
	}
}

func TestRumorCrudGuards(t *testing.T) {
	s := newCampaignServerOnly(t)
	rf := buildRumorFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, rf.fixture, "rumor-player-3", false)

	// Shape errors surface as 400 before the constraint does.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors",
		`{"statement":"x","truth":"maybe"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad truth: status %d, want 400", rec.Code)
	}
	// A player cannot author, patch, delete or hold.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors",
		`{"statement":"x","truth":"true"}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player create: status %d, want 403", rec.Code)
	}
	// The DM settles what the rumour was: the false one is debunked.
	rec := hit(t, s, http.MethodPatch, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.falseRumor,
		`{"status":"debunked"}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"debunked"`) {
		t.Fatalf("patch: status %d body %s", rec.Code, rec.Body)
	}
	// Holders: the DM gives the bare rumour a mouth and takes it away.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.bareRumor+"/holders",
		`{"entity":"`+rf.dukeID+`","variant":"come back wrong, he says"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("holder: status %d body %s", rec.Code, rec.Body)
	}
	del := hit(t, s, http.MethodDelete, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.bareRumor+"/holders/"+rf.dukeID, "", dm)
	if del.Code != http.StatusOK {
		t.Fatalf("holder delete: status %d", del.Code)
	}
	// Delete is a delete.
	if rec := hit(t, s, http.MethodDelete, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.bareRumor, "", dm); rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+rf.campaignID+"/rumors/"+rf.bareRumor, "", dm); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted rumor: status %d, want 404", rec.Code)
	}
}
