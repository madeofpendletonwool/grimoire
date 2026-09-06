package server

// The duration and condition engine's HTTP surface (MAD-421): permissions
// at the API layer, the combat tick, the world clock expiring things
// through the clock's own endpoint, and the leak test — a player's read
// cannot reach another character's effects.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// newEffectsServer boots the stack the effect surface needs: the campaign
// graph, the ledger (whose rests are the world-clock events), and the
// effect engine grounded in an index seeded with the SRD's Poisoned
// entry.
func newEffectsServer(t *testing.T) (*Server, *fixture) {
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
	if _, err := store.DB().Exec(`INSERT INTO docs (corpus, number, title, body, source) VALUES
		('dnd', 'conditions/0001', 'Conditions — Poisoned',
		 'A poisoned creature has disadvantage on attack rolls and ability checks.',
		 'SRD 5.1')`); err != nil {
		t.Fatalf("seed condition doc: %v", err)
	}
	effectEngine, err := effects.New(store.DB(), campaigns, store)
	if err != nil {
		t.Fatalf("open effects store: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).WithEffects(effectEngine)
	f := buildFixture(t, s)
	return s, &f
}

// applyEffect posts one effect as the DM and returns the applied view.
func applyEffect(t *testing.T, s *Server, f fixture, body string) effectView {
	t.Helper()
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/effects", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("apply effect: status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Effect effectView `json:"effect"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode applied: %v", err)
	}
	return out.Effect
}

/* ---------- permissions ---------- */

func TestEffectPermissions(t *testing.T) {
	s, f := newEffectsServer(t)
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "thalia", true) // bound to f.pcID
	onlooker := addPlayerMember(t, s, *f, "onlooker", false)

	// A second pc nobody owns, carrying a secret the bound player must
	// not see.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", `{"kind":"pc","name":"Nyx"}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pc: status %d, body %s", rec.Code, rec.Body)
	}
	nyx := idFrom(t, rec, "entity")
	applyEffect(t, s, *f, `{"target_id":"`+nyx+`","kind":"condition","name":"charmed","duration":{"unit":"until_rest"}}`)

	// The bound player reads their own character's effects.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/effects", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("own effects: status %d, body %s", rec.Code, rec.Body)
	}

	// The leak test: the bound player cannot reach another character's
	// effects, and a party-scoped member cannot reach anyone's. A row
	// outside the player's scope is absent from every player path, not
	// hidden after the fact.
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+nyx+"/effects", "", player); rec.Code != http.StatusForbidden {
		t.Fatalf("another pc's effects: status %d, want 403", rec.Code)
	}
	if rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/effects", "", onlooker); rec.Code != http.StatusForbidden {
		t.Fatalf("party-scoped member read a character's effects: status %d, want 403", rec.Code)
	}

	// Writes and the clock are the DM's alone.
	for _, tc := range []struct {
		method, path, body string
		cookie             *http.Cookie
	}{
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/effects", "", player},
		{http.MethodGet, "/api/campaigns/" + f.campaignID + "/effects/concentrations", "", player},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/effects",
			`{"target_id":"` + f.pcID + `","kind":"condition","name":"prone","duration":{"amount":1,"unit":"round"}}`, player},
		{http.MethodPost, "/api/campaigns/" + f.campaignID + "/effects/advance", `{"rounds":1}`, player},
	} {
		if rec := hit(t, s, tc.method, tc.path, tc.body, tc.cookie); rec.Code != http.StatusForbidden {
			t.Fatalf("player reached %s %s: status %d, want 403", tc.method, tc.path, rec.Code)
		}
	}

	// The vocabulary is the game's own; every member may read it.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/effects/vocabulary", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("vocabulary: status %d", rec.Code)
	}
	var vocab struct {
		Kinds      []string `json:"kinds"`
		Units      []string `json:"units"`
		Conditions []string `json:"conditions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &vocab); err != nil {
		t.Fatalf("decode vocabulary: %v", err)
	}
	if len(vocab.Conditions) != 15 || vocab.Conditions[0] != "blinded" {
		t.Fatalf("the fifteen conditions: %v", vocab.Conditions)
	}
}

func TestEffectsUnavailableWithoutWiring(t *testing.T) {
	store, err := index.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := New(store, llm.New(llm.Config{}), nil, nil, nil, nil, nil, nil, Auth{}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// No WithEffects: every effect endpoint answers 503.
	req := httptest.NewRequest(http.MethodGet, "/api/campaigns/x/effects", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired effects: status %d, want 503", rec.Code)
	}
}

/* ---------- applying, grounding, concentration ---------- */

func TestApplyEffectSurfacesSRDText(t *testing.T) {
	s, f := newEffectsServer(t)
	view := applyEffect(t, s, *f, `{"target_id":"`+f.pcID+`","kind":"condition","name":"POISONED","duration":{"amount":10,"unit":"round"}}`)
	if view.Name != "poisoned" {
		t.Fatalf("condition not canonicalized: %+v", view)
	}
	if view.SRD == nil || view.SRD.Ref == "" || !strings.Contains(view.SRD.Body, "disadvantage on attack rolls") {
		t.Fatalf("the applied condition did not surface the real SRD text: %+v", view.SRD)
	}
	if view.Declared != "10 rounds" || view.Display != "1 minute" {
		t.Fatalf("declared/display: %q / %q", view.Declared, view.Display)
	}

	// The vocabulary is declared, not free text.
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/effects",
		`{"target_id":"`+f.pcID+`","kind":"condition","name":"dizzy","duration":{"unit":"until_rest"}}`, dm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("free-text condition: status %d, want 400", rec.Code)
	}
}

func TestApplyConcentrationBreaksTheOldLink(t *testing.T) {
	s, f := newEffectsServer(t)
	dm := dmSession(t, s)
	applyEffect(t, s, *f, `{"target_id":"`+f.pcID+`","kind":"spell","name":"Bless","source_id":"`+f.pcID+`","concentration":true,"duration":{"amount":1,"unit":"minute"}}`)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/effects",
		`{"target_id":"`+f.pcID+`","kind":"spell","name":"Hex","source_id":"`+f.pcID+`","concentration":true,"duration":{"amount":1,"unit":"hour"}}`, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("apply hex: status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Broke []effectView `json:"broke_concentration"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Broke) != 1 || out.Broke[0].Name != "Bless" {
		t.Fatalf("new concentration did not break bless: %+v", out.Broke)
	}

	// Who is concentrating on what: exactly one link for the source.
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/effects/concentrations", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("concentrations: status %d", rec.Code)
	}
	var links struct {
		Concentrations []effectView `json:"concentrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &links); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(links.Concentrations) != 1 || links.Concentrations[0].Name != "Hex" {
		t.Fatalf("concentration links: %+v", links.Concentrations)
	}
}

/* ---------- the two clocks over HTTP ---------- */

func TestEffectsAdvanceRounds(t *testing.T) {
	s, f := newEffectsServer(t)
	dm := dmSession(t, s)
	applyEffect(t, s, *f, `{"target_id":"`+f.pcID+`","kind":"spell","name":"Bless","source_id":"`+f.pcID+`","concentration":true,"duration":{"amount":10,"unit":"round"}}`)
	applyEffect(t, s, *f, `{"target_id":"`+f.pcID+`","kind":"spell","name":"Guiding Bolt","source_id":"`+f.pcID+`","duration":{"amount":1,"unit":"minute"}}`)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/effects/advance", `{"rounds":9}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("advance 9: status %d, body %s", rec.Code, rec.Body)
	}
	var tick struct {
		Active []effectView `json:"active"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tick); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, v := range tick.Active {
		if v.Display != "1 round" {
			t.Fatalf("%s after nine turns: %q, want \"1 round\" (the boundary holds on the wire too)", v.Name, v.Display)
		}
	}

	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/effects/advance", `{"rounds":1}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("advance 1: status %d", rec.Code)
	}
	var done struct {
		Active  []effectView `json:"active"`
		Expired []effectView `json:"expired"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &done); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(done.Active) != 0 || len(done.Expired) != 2 {
		t.Fatalf("the tenth turn must expire both sixty-second spellings: %d active, %d expired", len(done.Active), len(done.Expired))
	}

	// A player's own read sees the expiries.
	player := addPlayerMember(t, s, *f, "thalia", true)
	rec = hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/effects?ended=1", "", player)
	if rec.Code != http.StatusOK {
		t.Fatalf("own effects after the tick: status %d", rec.Code)
	}
	var history struct {
		Effects []effectView `json:"effects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(history.Effects) != 2 {
		t.Fatalf("history rows: %d", len(history.Effects))
	}
	for _, v := range history.Effects {
		if v.Status != "ended" || v.EndReason != "expired" {
			t.Fatalf("row after expiry: %+v", v)
		}
	}
}

func TestEffectsEndByDispelling(t *testing.T) {
	s, f := newEffectsServer(t)
	dm := dmSession(t, s)
	view := applyEffect(t, s, *f, `{"target_id":"`+f.pcID+`","kind":"spell","name":"Unseen Servant","source_id":"`+f.pcID+`","duration":{"unit":"until_dispelled"}}`)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/effects/"+view.ID+"/end", `{"reason":"dispelled"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("dispel: status %d, body %s", rec.Code, rec.Body)
	}
	var out struct {
		Effect effectView `json:"effect"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Effect.Status != "ended" || out.Effect.EndReason != "dispelled" {
		t.Fatalf("dispelled view: %+v", out.Effect)
	}

	// Ending twice refuses; a bad reason refuses.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/effects/"+view.ID+"/end", `{"reason":"manual"}`, dm); rec.Code != http.StatusNotFound {
		t.Fatalf("double end: status %d, want 404", rec.Code)
	}
}
