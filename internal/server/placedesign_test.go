package server

// The location designer's handler tests (MAD-372): scope enforcement, the
// offline gate, the assembled prompt, the flesh-out surface, and the
// refusal arithmetic — asserting on the response and the prompt the fake
// model recorded (ADR 8).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/canon"
)

// placeModelJSON scripts a valid fill for a medium village against the
// server fixture: everything new, holders drawn from the new people, and
// a road out when the request is anchored.
func placeModelJSON(withRoute bool) string {
	m := map[string]any{
		"place_name":    "Fenwick",
		"place_summary": "Forty roofs in a clearing and the road running wet through the middle.",
		"population":    "about 200",
		"government":    "Reeve Aldous",
		"defences":      "a palisade and a watch of six",
		"climate":       "temperate; damp off the pines",
		"state":         "the mill burned last month",
		"senses":        "woodsmoke and wet pine; an axe in the trees",
		"danger":        float64(2),
		"service_1":     "inn",
		"service_2":     "market",
	}
	for i, role := range []string{"inn", "temple", "market"} {
		id := fmt.Sprintf("sub-%d", i+1)
		m[id] = "new"
		m[id+"_new_name"] = "The " + role + " of Fenwick"
		m[id+"_new_summary"] = "The village's " + role + ", kept and argued over."
	}
	npcNames := []string{"Reeve Aldous", "Mother Iva", "Odd Tom"}
	for i, name := range npcNames {
		m[fmt.Sprintf("npc_%d", i+1)] = "new"
		m[fmt.Sprintf("npc_%d_new_name", i+1)] = name
		m[fmt.Sprintf("npc_%d_new_summary", i+1)] = "Keeps a finger on the village's pulse."
		m[fmt.Sprintf("npc_%d_goal", i+1)] = "Wants the road quiet again."
		m[fmt.Sprintf("npc_%d_voice", i+1)] = "Flat, careful, watching the door."
	}
	for i := 1; i <= 2; i++ {
		m[fmt.Sprintf("hook_%d_statement", i)] = "Hook: the woodcutters will not past the north ridge."
		m[fmt.Sprintf("hook_%d_thread", i)] = "the missing woodcutters"
	}
	for i := 1; i <= 2; i++ {
		m[fmt.Sprintf("secret_%d_statement", i)] = "Secret: the reeve pays the thing in the trees."
		m[fmt.Sprintf("secret_%d_holder", i)] = npcNames[i-1]
	}
	if withRoute {
		m["route_days"] = float64(2)
		m["route_terrain"] = "dirt road through dark pines"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

const placeDesignBody = `{
	"premise": "A small village on the forest road, uneasy about what is in the trees",
	"kind": "village", "scale": "medium"
}`

func TestLocationDesign_StagesASettlement(t *testing.T) {
	s, f, model, _ := newSkeletonServer(t)
	model.response = placeModelJSON(false)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/location", placeDesignBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("design/location: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Batch  map[string]any   `json:"batch"`
		Shape  map[string]any   `json:"shape"`
		Reused []map[string]any `json:"reused"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Batch == nil || body.Batch["id"] == "" {
		t.Fatalf("no batch in response: %s", rec.Body)
	}
	if body.Batch["source"] != "location" {
		t.Errorf("batch source = %v", body.Batch["source"])
	}
	if body.Shape["kind"] != "village" {
		t.Errorf("shape kind = %v", body.Shape["kind"])
	}
	items, _ := body.Batch["items"].([]any)
	if len(items) != 19 {
		t.Errorf("batch items = %d, want 19 (the village medium band: 1 place + 3 subs × 2 + 3 people × 2 + 2 hooks + 2 secrets × 2)", len(items))
	}

	// The prompt carried the computed shape, the band, the candidate pools
	// and the campaign's existing entities — the design contract, on the
	// wire.
	if len(model.prompts) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	for _, marker := range []string{
		"forest road", "\"kind\":\"village\"", "sub-1", "npc_1",
		"Duke Aldric Vane", "secret_1_holder", "\"npcs\":3",
	} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}
}

func TestLocationDesign_ScopeAndGates(t *testing.T) {
	s, f, _, _ := newSkeletonServer(t)
	player := addPlayerMember(t, s, *f, "mira", true)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/location", placeBody, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player status = %d, want 403", rec.Code)
	}
	dm := dmSession(t, s)
	for _, bad := range []struct {
		body string
		want int
	}{
		{`{"premise":"  "}`, http.StatusBadRequest},
		{`{"premise":"x","kind":"megacity"}`, http.StatusBadRequest},
		{`{"premise":"x","scale":"colossal"}`, http.StatusBadRequest},
		{`{"premise":"x","parts":["dragons"]}`, http.StatusBadRequest},
		{`{"premise":"x","parts":["npcs"]}`, http.StatusBadRequest},
	} {
		if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/location", bad.body, dm); rec.Code != bad.want {
			t.Errorf("body %s: status = %d, want %d (%s)", bad.body, rec.Code, bad.want, rec.Body)
		}
	}
}

func TestLocationDesign_OfflineRefuses(t *testing.T) {
	s, f, _, store := newSkeletonServer(t)
	offline, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("offline engine: %v", err)
	}
	s.canon = offline.WithGraphStores(s.campaigns, s.knowledge)
	dm := dmSession(t, s)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/location", placeDesignBody, dm); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline status = %d, want 503", rec.Code)
	}
}

func TestLocationFleshOut_ProposesAround(t *testing.T) {
	s, f, model, _ := newSkeletonServer(t)
	dm := dmSession(t, s)

	// A location that already exists: a name and one line.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities",
		`{"kind":"location","name":"Ashford","summary":"A market village on the Duke's road."}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create location: status %d, body %s", rec.Code, rec.Body)
	}
	eid := idFrom(t, rec, "entity")

	flesh := func() string {
		var m map[string]any
		if err := json.Unmarshal([]byte(placeModelJSON(false)), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, "place_name")
		delete(m, "place_summary")
		b, _ := json.Marshal(m)
		return string(b)
	}()
	model.response = flesh
	rec = hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/locations/"+eid+"/design/flesh-out",
		`{"premise":"the mill burned and nobody says why"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("flesh-out: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Batch map[string]any `json:"batch"`
		Shape map[string]any `json:"shape"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Batch == nil || body.Batch["id"] == "" {
		t.Fatalf("no batch in response: %s", rec.Body)
	}
	// The prompt proposed around what is already there: the existing
	// entity is in the structure, never to be replaced.
	prompt := model.prompts[len(model.prompts)-1]
	for _, marker := range []string{"Ashford", "A market village on the Duke's road.", "will not be replaced"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}

	// The refusals: a foreign entity, a non-location, a player.
	if rec := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/locations/no-such-place/design/flesh-out",
		`{"premise":"x"}`, dm); rec.Code != http.StatusNotFound {
		t.Errorf("foreign location status = %d, want 404", rec.Code)
	}
	if rec := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/locations/"+f.dukeID+"/design/flesh-out",
		`{"premise":"x"}`, dm); rec.Code != http.StatusBadRequest {
		t.Errorf("non-location status = %d, want 400", rec.Code)
	}
	player := addPlayerMember(t, s, *f, "thalia", true)
	if rec := hit(t, s, http.MethodPost,
		"/api/campaigns/"+f.campaignID+"/locations/"+eid+"/design/flesh-out",
		`{"premise":"x"}`, player); rec.Code != http.StatusForbidden {
		t.Errorf("player status = %d, want 403", rec.Code)
	}
}
