package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

/*
The quest graph's HTTP surface (MAD-369): the PATCH/DELETE a DM edits a quest
with, the entity-link endpoints, and the player journal. The machine-edit
guard — a recorded transition blocking the removal of its state or edge — is
exercised end to end here; the store-level tests in internal/campaign cover
the rest of the matrix.
*/

func TestQuestPatchDeleteAndEntityLinks(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	base := "/api/campaigns/" + f.campaignID

	machine := `{"initial":"rumoured","states":[
		{"key":"rumoured","label":"A rumour at the inn"},
		{"key":"confirmed","label":"Confirmed at the march fort"},
		{"key":"closed","label":"Laid to rest","terminal":"success"}],
		"edges":[{"from":"rumoured","to":"confirmed","label":"investigate"},
		         {"from":"confirmed","to":"closed","label":"resolve"}]}`
	rec := hit(t, s, http.MethodPost, base+"/quests",
		`{"name":"The March Fort Ghost","summary":"Odd lights.","visibility":"public","state_machine":`+machine+`}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create quest: status %d, body %s", rec.Code, rec.Body)
	}
	questID := idFrom(t, rec, "quest")
	var created struct {
		Quest struct {
			Summary    string `json:"summary"`
			Status     string `json:"status"`
			Visibility string `json:"visibility"`
		} `json:"quest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Quest.Summary != "Odd lights." || created.Quest.Status != "active" ||
		created.Quest.Visibility != "public" {
		t.Fatalf("the authored columns must round-trip: %+v", created.Quest)
	}

	// Move once, then try to edit the machine out from under the move.
	if rec = hit(t, s, http.MethodPost, base+"/quests/"+questID+"/transition", `{"to":"confirmed"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("transition: status %d, body %s", rec.Code, rec.Body)
	}
	transitions := hit(t, s, http.MethodGet, base+"/quests/"+questID, "", dm)
	if transitions.Code != http.StatusOK || !strings.Contains(transitions.Body.String(), `"transitions"`) {
		t.Fatalf("quest detail: status %d, body %s", transitions.Code, transitions.Body)
	}
	var detail struct {
		Transitions []struct {
			ID string `json:"from_state"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(transitions.Body.Bytes(), &detail); err != nil || len(detail.Transitions) != 1 {
		t.Fatalf("one recorded move: %v (%s)", err, transitions.Body)
	}

	// Removing the state the move left must be refused, naming the move.
	gutted := `{"initial":"confirmed","states":[{"key":"confirmed"},{"key":"closed"}],
		"edges":[{"from":"confirmed","to":"closed"}]}`
	rec = hit(t, s, http.MethodPatch, base+"/quests/"+questID, `{"state_machine":`+gutted+`}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("orphaning edit: status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "rumoured") {
		t.Fatalf("the refusal must name what the move still uses: %s", rec.Body)
	}

	// A history-preserving edit lands.
	grown := `{"initial":"rumoured","states":[
		{"key":"rumoured"},{"key":"confirmed"},{"key":"derailed"},{"key":"closed","terminal":"success"}],
		"edges":[{"from":"rumoured","to":"confirmed"},{"from":"confirmed","to":"derailed"},
		         {"from":"confirmed","to":"closed"}]}`
	rec = hit(t, s, http.MethodPatch, base+"/quests/"+questID, `{"state_machine":`+grown+`}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("sound edit: status %d, body %s", rec.Code, rec.Body)
	}

	// Column patches, including clearing act_id with an empty string.
	rec = hit(t, s, http.MethodPatch, base+"/quests/"+questID, `{"name":"The Fort Light","summary":"Strange lights.","act_id":""}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "The Fort Light") {
		t.Fatalf("patch columns: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPatch, base+"/quests/"+questID, `{"status":"paused"}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("off-vocabulary status: status %d, want 400", rec.Code)
	}

	// Entity links: add, refuse off-vocabulary, refuse duplicates, remove.
	rec = hit(t, s, http.MethodPost, base+"/quests/"+questID+"/entities",
		`{"entity_id":`+quote(f.dukeID)+`,"role":"obstacle"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link entity: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, base+"/quests/"+questID+"/entities",
		`{"entity_id":`+quote(f.dukeID)+`,"role":"obstacle"}`, dm)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate link: status %d, want 409", rec.Code)
	}
	rec = hit(t, s, http.MethodPost, base+"/quests/"+questID+"/entities",
		`{"entity_id":`+quote(f.dukeID)+`,"role":"menace"}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("off-vocabulary role: status %d, want 400", rec.Code)
	}
	rec = hit(t, s, http.MethodGet, base+"/quests/"+questID, "", dm)
	if !strings.Contains(rec.Body.String(), `"entities"`) || !strings.Contains(rec.Body.String(), `"obstacle"`) {
		t.Fatalf("quest detail must carry the links: %s", rec.Body)
	}
	rec = hit(t, s, http.MethodDelete, base+"/quests/"+questID+"/entities/"+f.dukeID+"?role=obstacle", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("unlink: status %d, body %s", rec.Code, rec.Body)
	}

	// DELETE soft-abandons; the quest survives and stops moving.
	rec = hit(t, s, http.MethodDelete, base+"/quests/"+questID, "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"abandoned"`) {
		t.Fatalf("delete must abandon: status %d, body %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, base+"/quests/"+questID+"/transition", `{"to":"closed"}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an abandoned quest must not move: status %d, want 400", rec.Code)
	}
}

// TestQuestJournalSurface: the journal serves every scope, carries only
// public quests with visited states, and hides the DM-only machine.
func TestQuestJournalSurface(t *testing.T) {
	s, _, _, _ := newCampaignServer(t)
	f := buildFixture(t, s)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, f, "mira", true)
	base := "/api/campaigns/" + f.campaignID

	rich := `{"initial":"offered","states":[
		{"key":"offered","label":"The job is offered"},
		{"key":"taken","label":"The job is taken"},
		{"key":"refused","label":"The job is refused"},
		{"key":"done","label":"Paid in full","terminal":"success"}],
		"edges":[{"from":"offered","to":"taken","label":"accept","requires":["` + f.secretID + `"]},
		         {"from":"offered","to":"refused","label":"decline"},
		         {"from":"taken","to":"done","label":"deliver"}]}`
	rec := hit(t, s, http.MethodPost, base+"/quests",
		`{"name":"The Silver Road Contract","summary":"Escort the caravan.","visibility":"public","state_machine":`+rich+`}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create public quest: status %d, body %s", rec.Code, rec.Body)
	}
	publicID := idFrom(t, rec, "quest")
	secret := `{"initial":"a","states":[{"key":"a"},{"key":"b"}],"edges":[{"from":"a","to":"b"}]}`
	rec = hit(t, s, http.MethodPost, base+"/quests",
		`{"name":"The Duke's Secret Ledger","state_machine":`+secret+`}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create secret quest: status %d, body %s", rec.Code, rec.Body)
	}

	// Move the public quest one step so "visited" has teeth.
	if rec = hit(t, s, http.MethodPost, base+"/quests/"+publicID+"/transition", `{"to":"taken"}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("transition: status %d, body %s", rec.Code, rec.Body)
	}

	// The player's journal: the public quest, its visited states — and
	// neither the secret quest nor the unvisited branch.
	rec = hit(t, s, http.MethodGet, base+"/quests/journal", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("player journal: status %d, body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"The Silver Road Contract", "The job is offered", "The job is taken"} {
		if !strings.Contains(body, want) {
			t.Errorf("journal missing %q: %s", want, body)
		}
	}
	for _, leak := range []string{"The Duke's Secret Ledger", "refused", "The job is refused", "requires", "terminal"} {
		if strings.Contains(body, leak) {
			t.Errorf("LEAK: journal carries %q: %s", leak, body)
		}
	}

	// The DM reads the same leak-safe projection; the DM's own quest list
	// still carries both quests and full machines.
	rec = hit(t, s, http.MethodGet, base+"/quests/journal", "", dm)
	if !strings.Contains(rec.Body.String(), "The Silver Road Contract") ||
		strings.Contains(rec.Body.String(), "The Duke's Secret Ledger") {
		t.Errorf("the dm journal is the same projection: %s", rec.Body)
	}
	rec = hit(t, s, http.MethodGet, base+"/quests", "", dm)
	if !strings.Contains(rec.Body.String(), "The Duke's Secret Ledger") {
		t.Errorf("the dm list must carry the secret quest: %s", rec.Body)
	}

	// A player touching any DM quest route is refused — journal excepted,
	// which the earlier successful GET already proved.
	for _, w := range []struct {
		method, target, reqBody string
	}{
		{http.MethodGet, base + "/quests/" + publicID, ""},
		{http.MethodPatch, base + "/quests/" + publicID, `{"summary":"x"}`},
		{http.MethodDelete, base + "/quests/" + publicID, ""},
		{http.MethodPost, base + "/quests/" + publicID + "/entities", `{"entity_id":"x","role":"giver"}`},
		{http.MethodDelete, base + "/quests/" + publicID + "/entities/x", ""},
	} {
		if rec := hit(t, s, w.method, w.target, w.reqBody, player); rec.Code != http.StatusForbidden {
			t.Errorf("player %s %s: status %d, want 403", w.method, w.target, rec.Code)
		}
	}
}
