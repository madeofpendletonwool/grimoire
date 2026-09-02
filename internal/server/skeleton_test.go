package server

// The campaign skeleton generator's handler tests (MAD-361): scope
// enforcement, the offline gate, and the assembled prompt — asserting on the
// HTTP response and the prompt the fake model recorded (ADR 8).

import (
	"context"
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
	"github.com/madeofpendletonwool/grimoire/internal/testdb"
)

// fakeCanonModel replays one scripted response and records the prompts.
type fakeCanonModel struct {
	response string
	prompts  []string
}

func (f *fakeCanonModel) ModelName() string { return "fake-skeleton" }

func (f *fakeCanonModel) Complete(ctx context.Context, system, user string) (canon.Completion, error) {
	f.prompts = append(f.prompts, user)
	return canon.Completion{Text: f.response, InputTokens: 100, OutputTokens: 200}, nil
}

// skeletonModelJSON scripts a full four-act, four-faction fill.
func skeletonModelJSON() string {
	m := map[string]any{}
	factions := []struct{ faction, npc string }{
		{"The Ashen Crown", "Duke Aldric Vane"},
		{"The Pale Root", "Mother Yffre"},
		{"The Wayfarers' Compact", "Sergeant Bray"},
		{"The Sable Ledger", "Vess the Quiet"},
	}
	for i, f := range factions {
		m[fmt.Sprintf("faction_%d_name", i+1)] = f.faction
		m[fmt.Sprintf("faction_%d_summary", i+1)] = "Patient, indebted, and patient."
		m[fmt.Sprintf("npc_%d_name", i+1)] = f.npc
		m[fmt.Sprintf("npc_%d_summary", i+1)] = "Never where the blame lands."
	}
	m["secret_statement"] = "The Pale Root secretly controls the Ashen Crown."
	for i := 1; i <= 4; i++ {
		m[fmt.Sprintf("hook_%d_statement", i)] = "Carters vanish on the old road."
		m[fmt.Sprintf("hook_%d_thread", i)] = "the road disappearances"
		m[fmt.Sprintf("hook_%d_lead", i)] = "A limping carter swears it at the Waystone."
		m[fmt.Sprintf("act_%d_name", i)] = fmt.Sprintf("Act %d", i)
		m[fmt.Sprintf("act_%d_premise", i)] = "The forest leans closer."
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// newSkeletonServer boots a gated server whose canon engine carries a fake
// model, so the generator path is live end to end. The index store is handed
// back for tests that rewire the engine.
func newSkeletonServer(t *testing.T) (*Server, *fixture, *fakeCanonModel, *index.Store) {
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
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("open knowledge store: %v", err)
	}
	model := &fakeCanonModel{response: skeletonModelJSON()}
	engine, err := canon.New(store.DB(), model, canon.Config{MaxCandidates: 500, BatchSize: 8})
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	s, err := New(store, llm.New(llm.Config{APIKey: "test"}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine)
	f := buildFixture(t, s)
	return s, &f, model, store
}

const skeletonBody = `{
	"premise": "dark fantasy, a kingdom consumed by an ancient forest, occasional nasty horror",
	"level_start": 1, "level_end": 12, "act_count": 4
}`

func TestSkeleton_GeneratesBatchActsAndPlans(t *testing.T) {
	s, f, model, _ := newSkeletonServer(t)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/skeleton", skeletonBody, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("skeleton: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Batch map[string]any `json:"batch"`
		Bands []struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"bands"`
		Acts []struct {
			LevelStart int    `json:"level_start"`
			LevelEnd   int    `json:"level_end"`
			Name       string `json:"name"`
		} `json:"acts"`
		Plans []map[string]any `json:"plans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Batch == nil || body.Batch["id"] == "" {
		t.Fatalf("no batch in response: %s", rec.Body)
	}
	if body.Batch["source"] != "skeleton" {
		t.Errorf("batch source = %v", body.Batch["source"])
	}
	// The bands tile 1-12 exactly with the model faked: our arithmetic.
	want := [][2]int{{1, 3}, {4, 6}, {7, 9}, {10, 12}}
	if len(body.Acts) != 4 || len(body.Bands) != 4 {
		t.Fatalf("acts = %d, bands = %d, want 4 and 4", len(body.Acts), len(body.Bands))
	}
	for i, act := range body.Acts {
		if act.LevelStart != want[i][0] || act.LevelEnd != want[i][1] {
			t.Errorf("act %d band = %d-%d, want %d-%d", i+1, act.LevelStart, act.LevelEnd, want[i][0], want[i][1])
		}
	}
	if len(body.Plans) == 0 {
		t.Error("no session plans written")
	}

	// The prompt carried the computed structure and the existing entities the
	// model was told to reuse.
	if len(model.prompts) != 1 {
		t.Fatalf("model calls = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	for _, marker := range []string{"hidden_hand", "secretly_controls", "Duke Aldric Vane", "\"level_start\":1", "\"level_end\":12"} {
		if !strings.Contains(prompt, marker) {
			t.Errorf("prompt missing %q", marker)
		}
	}
}

func TestSkeleton_OnlyTheDMCanDesign(t *testing.T) {
	s, f, _, _ := newSkeletonServer(t)
	player := addPlayerMember(t, s, *f, "mira", true)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/skeleton", skeletonBody, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player status = %d, want 403", rec.Code)
	}
	// A bad request names its problem.
	dm := dmSession(t, s)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/skeleton", `{"premise":""}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty premise status = %d, want 400", rec.Code)
	}
}

func TestSkeleton_OfflineRefuses(t *testing.T) {
	s, f, _, store := newSkeletonServer(t)
	// Swap in an offline canon store: no model, no generation.
	offline, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("offline engine: %v", err)
	}
	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	s.canon = offline.WithGraphStores(campaigns, knowledgeStore)
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/design/skeleton", skeletonBody, dm)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline status = %d, want 503 (body %s)", rec.Code, rec.Body)
	}
}
