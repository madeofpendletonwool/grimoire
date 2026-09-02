package server

// The quest designer's handler tests (MAD-371): scope enforcement, the
// offline gate, the assembled prompt, and the branch operation's HTTP
// surface — asserting on the response and the prompt the fake model
// recorded (ADR 8).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// questModelJSON scripts a valid fill for an investigation with two branch
// points and depth four, against the server fixture's cast: the Duke is
// reused as the giver, the site and obstacle are new.
func questModelJSON() string {
	m := map[string]any{
		"quest_name":           "The Caravan That Never Came",
		"quest_summary":        "The grain caravan is missing and the road is hungry.",
		"giver":                "Duke Aldric Vane",
		"site":                 "new",
		"site_new_name":        "The Ashen Road",
		"site_new_summary":     "The old trade road through the forest's edge.",
		"obstacle":             "new",
		"obstacle_new_name":    "The Thorn Wardens",
		"obstacle_new_summary": "Hooded figures who decide what travels.",
	}
	names := map[string]string{
		"beat-1": "The Hook Lands", "beat-2": "The Ledger Trail",
		"beat-3": "The Road Again", "beat-4": "The Confrontation",
		"fork-1-a": "Trust the Survivor", "fork-1-b": "Accuse the Survivor",
		"fork-2-a": "Deliver the Truth", "fork-2-b": "Bury the Truth",
		"ending-success": "The Caravan Found", "ending-failure": "The Trail Dies",
	}
	for id, name := range names {
		m["state_"+id+"_name"] = name
		if id != "ending-success" && id != "ending-failure" {
			m["state_"+id+"_detail"] = "The situation tightens."
		}
	}
	for _, id := range []string{"fork-1-a", "fork-1-b", "fork-2-a", "fork-2-b"} {
		m["secret_"+id] = "new"
		m["secret_"+id+"_new"] = "Someone paid for the road to stay dark."
	}
	b, _ := json.Marshal(m)
	return string(b)
}

const questBody = `{
	"hook": "The grain caravan from the marches never arrived. Something has been taking carts for months.",
	"kind": "investigation", "branch_points": 2, "depth": 4
}`

func TestQuestDesign_StagesABranchingQuest(t *testing.T) {
	s, f, model, _ := newSkeletonServer(t)
	model.response = questModelJSON()
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/quest", questBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("design/quest: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Batch   map[string]any `json:"batch"`
		Shape   map[string]any `json:"shape"`
		Machine struct {
			Initial string           `json:"initial"`
			States  []map[string]any `json:"states"`
		} `json:"machine"`
		Reused []map[string]any `json:"reused"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Batch == nil || body.Batch["id"] == "" {
		t.Fatalf("no batch in response: %s", rec.Body)
	}
	if body.Batch["source"] != "quest" {
		t.Errorf("batch source = %v", body.Batch["source"])
	}
	if body.Shape["key"] != "investigation" {
		t.Errorf("shape = %v", body.Shape["key"])
	}
	// Two branch points, depth four: 4 beats + 4 arms + 2 endings.
	if len(body.Machine.States) != 10 {
		t.Errorf("machine states = %d, want 10", len(body.Machine.States))
	}
	if body.Machine.Initial != "the-hook-lands" {
		t.Errorf("initial = %q", body.Machine.Initial)
	}
	if len(body.Reused) == 0 {
		t.Error("the reused giver was not reported")
	}

	// The prompt carried the computed topology, the cast pool and the
	// campaign's existing entities — the design contract, on the wire.
	if len(model.prompts) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	for _, marker := range []string{
		"\"branch_points\":2", "fork-2-a", "ending-success",
		"Duke Aldric Vane", "beat-1", "grain caravan",
	} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}
}

func TestQuestDesign_ScopeAndGates(t *testing.T) {
	s, f, _, _ := newSkeletonServer(t)
	player := addPlayerMember(t, s, *f, "mira", true)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/quest", questBody, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player status = %d, want 403", rec.Code)
	}
	dm := dmSession(t, s)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/quest", `{"hook":"  "}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty hook status = %d, want 400", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/quest", `{"hook":"x","kind":"musical"}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind status = %d, want 400", rec.Code)
	}
}

func TestQuestDesign_OfflineRefuses(t *testing.T) {
	s, f, _, store := newSkeletonServer(t)
	offline, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("offline engine: %v", err)
	}
	s.canon = offline.WithGraphStores(s.campaigns, s.knowledge)
	dm := dmSession(t, s)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/quest", questBody, dm); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline status = %d, want 503", rec.Code)
	}
}

func TestQuestBranch_ProposesTwoOutcomes(t *testing.T) {
	s, f, model, _ := newSkeletonServer(t)
	dm := dmSession(t, s)

	// A quest on the board to branch.
	machine := campaign.StateMachine{Initial: "offered"}
	machine.States = campaign.States("offered", "find-the-survivor", "triumph", "disaster")
	machine.States[2].Terminal = campaign.TerminalSuccess
	machine.States[3].Terminal = campaign.TerminalFailure
	machine.Edges = []campaign.StateEdge{
		{From: "offered", To: "find-the-survivor"},
		{From: "find-the-survivor", To: "triumph"},
		{From: "find-the-survivor", To: "disaster"},
	}
	qbody := map[string]any{"name": "The Missing Caravan", "state_machine": machine}
	qb, _ := json.Marshal(qbody)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/quests", string(qb), dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create quest: status %d, body %s", rec.Code, rec.Body)
	}
	questID := idFrom(t, rec, "quest")

	model.response = `{
		"branch_a_name": "Trust the Survivor", "branch_a_detail": "Hide them in the cellar.",
		"branch_a_ending": "The Survivor Saved",
		"branch_b_name": "Accuse the Survivor", "branch_b_detail": "Name them before the town.",
		"branch_b_ending": "The Wrong Party Condemned"
	}`
	rec = hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/quests/"+questID+"/design/branch",
		`{"state":"find-the-survivor","notes":"the survivor knows more than they say"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("design/branch: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Batch   map[string]any `json:"batch"`
		Machine struct {
			States []map[string]any `json:"states"`
		} `json:"machine"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Batch == nil || body.Batch["id"] == "" {
		t.Fatalf("no batch in response: %s", rec.Body)
	}
	if len(body.Machine.States) != 8 {
		t.Errorf("machine states = %d, want 8 (four beats added)", len(body.Machine.States))
	}
	// The prompt showed the model the quest and the chosen state.
	prompt := model.prompts[len(model.prompts)-1]
	for _, marker := range []string{"The Missing Caravan", "find-the-survivor", "triumph"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}

	// The refusals: an unknown state, an ending, a foreign quest.
	for _, bad := range []struct {
		state string
		want  int
	}{
		{"nope", http.StatusBadRequest},
		{"triumph", http.StatusBadRequest},
	} {
		rec := hit(t, s, http.MethodPost,
			"/api/campaigns/"+f.campaignID+"/quests/"+questID+"/design/branch",
			fmt.Sprintf(`{"state":%q}`, bad.state), dm)
		if rec.Code != bad.want {
			t.Errorf("branch at %q status = %d, want %d (body %s)", bad.state, rec.Code, bad.want, rec.Body)
		}
	}
	rec = hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/quests/no-such-quest/design/branch",
		`{"state":"offered"}`, dm)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign quest status = %d, want 404", rec.Code)
	}

	// Only the DM branches.
	player := addPlayerMember(t, s, *f, "thalia", true)
	if rec := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/quests/"+questID+"/design/branch",
		`{"state":"offered"}`, player); rec.Code != http.StatusForbidden {
		t.Errorf("player status = %d, want 403", rec.Code)
	}
}
