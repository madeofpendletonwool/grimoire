package server

// The proposal-batch HTTP surface (MAD-359): a batch can be staged by hand
// and decided as one unit, and only the DM can touch any of it — the same
// gate every other canon write sits behind (ADR 8's handler tests).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newProposalsServer boots a gated server with the campaign stack and a
// canon engine whose graph stores are wired, so the decision path can
// write canon.
func newProposalsServer(t *testing.T) (*Server, *fixture) {
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
	engine, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine)
	f := buildFixture(t, s)
	return s, &f
}

// stageBody is one hand-staged batch over the fixture's existing graph: a
// new faction, a secret fact tying it to the fixture's duke (by entity id —
// a generator knows the campaign's real ids; names are for the batch's own
// objects), and a relationship between the two.
func stageBody(f fixture) string {
	return fmt.Sprintf(`{
		"source": "skeleton",
		"prompt": "Design the faction that shadows the duke",
		"items": [
			{"id": "house", "kind": "entity", "payload": {
				"local_id": "house-vane", "kind": "faction", "name": "House Vane",
				"summary": "The richest trading house."}},
			{"id": "tie", "kind": "fact", "depends_on": ["house"], "payload": {
				"statement": "Duke Aldric Vane secretly controls House Vane.",
				"subject": %q, "predicate": "secretly_controls",
				"object_entity": "House Vane", "visibility": "secret"}},
			{"id": "edge", "kind": "relationship", "depends_on": ["house"], "payload": {
				"from_entity": %q, "rel_type": "secretly_controls",
				"to_entity": "House Vane"}}
		]
	}`, f.dukeID, f.dukeID)
}

func decodeBatch(t *testing.T, rec *recorder) map[string]any {
	t.Helper()
	var body struct {
		Batch map[string]any `json:"batch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode batch: %v (%s)", err, rec.Body)
	}
	return body.Batch
}

func TestProposals_StageListGetDecide(t *testing.T) {
	s, f := newProposalsServer(t)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals", stageBody(*f), dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("stage: status %d, body %s", rec.Code, rec.Body)
	}
	batch := decodeBatch(t, rec)
	batchID, _ := batch["id"].(string)
	if batchID == "" || batch["status"] != "open" {
		t.Fatalf("staged batch = %v", batch)
	}
	items, _ := batch["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("batch items = %d, want 3", len(items))
	}

	// The list read carries counts, not items.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/proposals?status=open", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body %s", rec.Code, rec.Body)
	}
	var listBody struct {
		Batches []map[string]any `json:"batches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listBody); err != nil {
		t.Fatal(err)
	}
	if len(listBody.Batches) != 1 || listBody.Batches[0]["item_count"] != float64(3) {
		t.Fatalf("batches = %+v", listBody.Batches)
	}

	// The single read carries the items with their payloads and depends_on.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID, "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d, body %s", rec.Code, rec.Body)
	}
	full := decodeBatch(t, rec)
	items, _ = full["items"].([]any)
	var factItem map[string]any
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["kind"] == "proposed_fact" {
			factItem = m
		}
	}
	if factItem == nil {
		t.Fatalf("no proposed_fact in %v", items)
	}
	if deps, _ := factItem["depends_on"].([]any); len(deps) != 1 {
		t.Fatalf("fact depends_on = %v", factItem["depends_on"])
	}
	if payload, _ := factItem["payload"].(map[string]any); payload["statement"] == nil {
		t.Fatalf("fact payload missing: %v", factItem["payload"])
	}

	// Decide: the whole batch accepted through one call.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"accept"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("decide: status %d, body %s", rec.Code, rec.Body)
	}
	var dec struct {
		Batch map[string]any   `json:"batch"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatal(err)
	}
	if dec.Batch["status"] != "accepted" {
		t.Fatalf("batch status = %v", dec.Batch["status"])
	}
	for _, it := range dec.Items {
		if it["status"] != "accepted" {
			t.Fatalf("item = %v", it)
		}
	}

	// The graph landed: the faction exists, and the fact ties it to the
	// duke — the relationship points at the entity this batch created.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/entities?kind=faction", "", dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "House Vane") {
		t.Fatalf("faction missing after accept: %d %s", rec.Code, rec.Body)
	}

	// Deciding again is a no-op: the never-resurrect rule, held by the
	// batch even under a different decision.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"dismiss"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-decide: status %d, body %s", rec.Code, rec.Body)
	}
	if again := decodeBatch(t, rec); again["status"] != "accepted" {
		t.Fatalf("re-decide flipped the batch to %v", again["status"])
	}
}

func TestProposals_PlayerCannotTouchTheSurface(t *testing.T) {
	s, f := newProposalsServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "mira", true)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals", stageBody(*f), dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("stage: status %d, body %s", rec.Code, rec.Body)
	}
	batchID, _ := decodeBatch(t, rec)["id"].(string)

	for _, target := range []struct {
		method, path string
		body         string
	}{
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/proposals", stageBody(*f)},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/proposals", ""},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/proposals/" + batchID, ""},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/proposals/" + batchID + "/decision", `{"decision":"accept"}`},
	} {
		if rec := hit(t, s, target.method, target.path, target.body, player); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: player status = %d, want 403", target.method, target.path, rec.Code)
		}
	}
}

func TestProposals_ValidationFailures(t *testing.T) {
	s, f := newProposalsServer(t)
	dm := dmSession(t, s)

	// An unknown source is a 400, not a silent row.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals",
		`{"source":"wish","items":[{"id":"a","kind":"entity","payload":{"kind":"npc","name":"A"}}]}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown source: status %d, body %s", rec.Code, rec.Body)
	}
	// A cyclic graph is refused before anything is written.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals", `{
		"source": "scene",
		"items": [
			{"id": "a", "kind": "entity", "depends_on": ["b"], "payload": {"kind": "npc", "name": "A"}},
			{"id": "b", "kind": "entity", "depends_on": ["a"], "payload": {"kind": "npc", "name": "B"}}
		]
	}`, dm)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "depends on") {
		t.Fatalf("cycle: status %d, body %s", rec.Code, rec.Body)
	}
	// A bad batch decision vocabulary is a 400.
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals",
		`{"source":"scene","items":[{"id":"a","kind":"entity","payload":{"kind":"npc","name":"A"}}]}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("stage: status %d, body %s", rec.Code, rec.Body)
	}
	batchID, _ := decodeBatch(t, rec)["id"].(string)
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/proposals/"+batchID+"/decision",
		`{"decision":"maybe"}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad decision: status %d, body %s", rec.Code, rec.Body)
	}
	// An unknown batch is a 404.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/proposals/no-such-batch", "", dm)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown batch: status %d", rec.Code)
	}
}
