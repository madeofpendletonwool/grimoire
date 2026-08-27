package server

// The command interface's handler tests (MAD-363): scope enforcement, the
// offline gate (undo must survive it), the endpoint end to end against the
// fake canon model, and the DM Grimoire's routing — a slash-prefixed DM
// message reaches the command engine, never the answer path, and a
// player's slash is still just a question.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/chat"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newCommandServer boots a full gated server — campaign stack, chat store,
// canon engine with a scripted fake model — for the command surface.
func newCommandServer(t *testing.T, response string, llmCfg llm.Config) (*Server, *fixture, *fakeCanonModel) {
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
	chats, err := chat.New(store.DB())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	model := &fakeCanonModel{response: response}
	engine, err := canon.New(store.DB(), model, canon.Config{MaxCandidates: 500, BatchSize: 8})
	if err != nil {
		t.Fatalf("open canon engine: %v", err)
	}
	engine = engine.WithGraphStores(campaigns, knowledgeStore)
	s, err := New(store, llm.New(llmCfg), nil, nil, nil, chats, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithCanon(engine)
	f := buildFixture(t, s)
	return s, &f, model
}

func commandLLM(t *testing.T) llm.Config {
	t.Helper()
	stub := newCapturingLLM(t, "an ordinary answer")
	return llm.Config{BaseURL: stub.baseURL, APIKey: "test-key", Model: "test-model"}
}

const commandCreateFill = `{"verb":"create_entity","name":"Vess the Quiet","kind":"npc",
	"summary":"A level 5 necromancer.","rel_type":"serves","rel_target":"Duke Aldric Vane"}`

func TestCommandEndpoint_StagesBatchAndLogsIt(t *testing.T) {
	s, f, model := newCommandServer(t, commandCreateFill, commandLLM(t))
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/command",
		`{"text":"Create a level 5 necromancer called Vess who secretly works for the Duke."}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Command struct {
			Kind    string `json:"kind"`
			Message string `json:"message"`
			Batch   struct {
				ID     string `json:"id"`
				Source string `json:"source"`
				Items  []struct {
					Kind string `json:"kind"`
				} `json:"items"`
			} `json:"batch"`
		} `json:"command"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Command.Kind != "batch" || body.Command.Batch.ID == "" {
		t.Fatalf("command = %+v", body.Command)
	}
	if body.Command.Batch.Source != "nl_command" || len(body.Command.Batch.Items) != 2 {
		t.Fatalf("batch = %+v", body.Command.Batch)
	}
	if !strings.Contains(body.Command.Message, "does not exist yet") {
		t.Errorf("message must say the entity will be created: %s", body.Command.Message)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("model calls = %d", len(model.prompts))
	}

	// The transcript read: the DM-only log.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/command/log", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("log status %d", rec.Code)
	}
	var logBody struct {
		Commands []struct {
			Text      string `json:"text"`
			Kind      string `json:"kind"`
			Referents []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"referents"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &logBody); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if len(logBody.Commands) != 1 || logBody.Commands[0].Kind != "batch" {
		t.Fatalf("log = %+v", logBody.Commands)
	}
	if len(logBody.Commands[0].Referents) != 1 || logBody.Commands[0].Referents[0].Name != "Vess the Quiet" {
		t.Fatalf("referents = %+v", logBody.Commands[0].Referents)
	}
}

func TestCommandEndpoint_QuestionStagesNothing(t *testing.T) {
	// "Vess" resolves to nothing in the fixture: the zero-match reference
	// must come back a question, with nothing staged behind the gate.
	s, f, _ := newCommandServer(t, `{"verb":"create_relationship","rel_type":"located_in",
		"from_entity":"Vess","to_entity":"Blackwater"}`, commandLLM(t))
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/command",
		`{"text":"Put Vess in Blackwater."}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Command struct {
			Kind     string `json:"kind"`
			Question struct {
				Question string `json:"question"`
			} `json:"question"`
		} `json:"command"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Command.Kind != "question" || !strings.Contains(body.Command.Question.Question, "Vess") {
		t.Fatalf("command = %+v", body.Command)
	}
	// Nothing staged: the proposals list is empty.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/proposals?status=open", "", dm)
	if !strings.Contains(rec.Body.String(), `"batches":[]`) && !strings.Contains(rec.Body.String(), `"batches": null`) {
		t.Fatalf("open batches leaked: %s", rec.Body)
	}
}

func TestCommandEndpoint_OnlyTheDMCanCommand(t *testing.T) {
	s, f, _ := newCommandServer(t, commandCreateFill, commandLLM(t))
	player := addPlayerMember(t, s, *f, "mira", true)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/command",
		`{"text":"create Vess"}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player status = %d, want 403", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/command/log", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("player log status = %d, want 403", rec.Code)
	}
	// A bad request names its problem.
	dm := dmSession(t, s)
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/command", `{"text":"  "}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text status = %d, want 400", rec.Code)
	}
}

func TestCommandEndpoint_OfflineUndoStillWorks(t *testing.T) {
	// No model key configured: undo survives, everything model-driven 503s.
	s, f, _ := newCommandServer(t, commandCreateFill, llm.Config{})
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/command",
		`{"text":"Undo that."}`, dm)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nothing to undo") {
		t.Fatalf("offline undo: %d %s", rec.Code, rec.Body)
	}
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/command",
		`{"text":"create Vess"}`, dm)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline create status = %d, want 503 (body %s)", rec.Code, rec.Body)
	}
}

func TestCommandChat_RoutesSlashCommandsToTheEngine(t *testing.T) {
	stub := newCapturingLLM(t, "an ordinary answer")
	cfg := llm.Config{BaseURL: stub.baseURL, APIKey: "test-key", Model: "test-model"}
	s, f, model := newCommandServer(t, commandCreateFill, cfg)
	dm := dmSession(t, s)
	thread := newCampaignThread(t, s, *f, dm)

	code, body := askCampaign(t, s, *f, dm, thread, "/create a level 5 necromancer called Vess who secretly works for the Duke")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	frames := campaignSSEFrames(body)
	meta := frames["meta"]
	if meta == nil {
		t.Fatalf("no meta frame: %s", body)
	}
	cmd, ok := meta["command"].(map[string]any)
	if !ok || cmd["kind"] != "batch" {
		t.Fatalf("meta command = %+v", meta["command"])
	}
	delta := frames["delta"]
	if delta == nil || !strings.Contains(delta["text"].(string), "does not exist yet") {
		t.Fatalf("delta = %+v", delta)
	}
	if frames["done"] == nil {
		t.Fatalf("no done frame: %s", body)
	}
	// The command never reached the answer path: the chat model was not
	// called, the command model was.
	if len(stub.bodies) != 0 {
		t.Fatalf("the answer model was called %d times for a command", len(stub.bodies))
	}
	if len(model.prompts) != 1 {
		t.Fatalf("command model calls = %d", len(model.prompts))
	}
	// The exchange persisted like any other turn.
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/chats/"+thread, "", dm)
	if !strings.Contains(rec.Body.String(), "does not exist yet") {
		t.Fatalf("command turn not persisted: %s", rec.Body)
	}
}

func TestCommandChat_PlayerSlashStaysAQuestion(t *testing.T) {
	stub := newCapturingLLM(t, "an ordinary answer")
	cfg := llm.Config{BaseURL: stub.baseURL, APIKey: "test-key", Model: "test-model"}
	s, f, model := newCommandServer(t, commandCreateFill, cfg)
	player := addPlayerMember(t, s, *f, "mira", true)
	thread := newCampaignThread(t, s, *f, player)

	code, body := askCampaign(t, s, *f, player, thread, "/undo")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	// The player's slash went to the answer path, not the command engine.
	if len(model.prompts) != 0 {
		t.Fatalf("command model was called for a player: %d", len(model.prompts))
	}
	if len(stub.bodies) != 1 {
		t.Fatalf("answer model calls = %d, want 1", len(stub.bodies))
	}
}
