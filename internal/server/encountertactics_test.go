package server

// The tactical analysis surface over HTTP (MAD-381): the live endpoint's
// degradation and DM-scoping, and the design flow's prose gate — a fake
// client asserting an invented figure in its Tactics section gets that
// prose marked rejected, never silently rendered as derived.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
)

// tacticsBestiary is a small SRD stand-in whose one creature parses into
// real damage: a bow goblin.
const tacticsBestiary = `{"next":"","results":[
	{"key":"srd-2024_goblin","name":"Goblin","document":{"key":"srd-2024"},
	 "challenge_rating":0.25,"type":{"name":"Humanoid"},"size":{"name":"Small"},
	 "armor_class":15,"hit_points":7,"speed_all":{"walk":30},
	 "actions":[{"name":"Shortbow","desc":"Ranged Weapon Attack: +4 to hit, range 80/320 ft., one target. Hit: 5 (1d6+2) piercing damage.","action_type":"ACTION"}]}
]}`

// newTacticsServer wires the campaign graph and a mirrored bestiary whose
// creatures carry parseable damage, with an optional stubbed model.
func newTacticsServer(t *testing.T, llmHandler http.HandlerFunc) (*Server, fixture) {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "tactics.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := migrate.Up(store.DB()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	open5e := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/creatures/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, tacticsBestiary)
	}))
	t.Cleanup(open5e.Close)

	campaigns, err := campaign.New(store.DB())
	if err != nil {
		t.Fatalf("campaign store: %v", err)
	}
	knowledgeStore, err := knowledge.New(store.DB())
	if err != nil {
		t.Fatalf("knowledge store: %v", err)
	}
	encounters, err := encounter.New(store.DB())
	if err != nil {
		t.Fatalf("encounter store: %v", err)
	}
	catalog, err := encounter.NewCatalog(store.DB(), open5e.URL)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	cfg := llm.Config{APIKey: "", Model: "test"}
	if llmHandler != nil {
		up := httptest.NewServer(llmHandler)
		t.Cleanup(up.Close)
		cfg = llm.Config{BaseURL: up.URL, APIKey: "test-key", Model: "test-model"}
	}
	users, err := auth.New(store.DB(), 0, 0)
	if err != nil {
		t.Fatalf("auth store: %v", err)
	}
	s, err := New(store, llm.New(cfg), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := catalog.Sync(context.Background()); err != nil {
		t.Fatalf("sync bestiary: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).
		WithEncounters(encounters, encounter.NewBestiaryWithBase(open5e.URL), catalog)
	return s, buildFixture(t, s)
}

/* ---------- the live endpoint ---------- */

// No campaign, bare levels: the analysis degrades to the monsters' side and
// says so, instead of inventing a party to aim at.
func TestTacticsEndpointDegradesWithoutAPartyBlock(t *testing.T) {
	s, _ := newTacticsServer(t, nil)
	dm := dmSession(t, s)

	rec := hit(t, s, http.MethodPost, "/api/encounters/tactics",
		`{"party":[3,3,3,3],"monsters":[{"name":"Goblin","cr":"1/4","count":4}]}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("tactics: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Tactics encounter.Analysis `json:"tactics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := body.Tactics
	if a.PartyKnown {
		t.Error("bare levels were read as a party block")
	}
	found := false
	for _, c := range a.Caveats {
		if strings.Contains(c, "no usable party block") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no degrade caveat: %v", a.Caveats)
	}
	if len(a.Threat) != 1 || a.Threat[0].Name != "Goblin" {
		t.Fatalf("threat = %+v, want the priced goblin", a.Threat)
	}
	if a.Threat[0].PerRound.Value != 20 { // 4 × 5
		t.Errorf("goblin per-round = %v, want 20", a.Threat[0].PerRound.Value)
	}
	for _, f := range a.Focus {
		if f.Target != "" {
			t.Errorf("a target was invented with no party block: %+v", f)
		}
	}
}

// With a campaign whose pcs declare a sheet, the analysis aims at it — the
// soft pc gets the pack, the hard one does not.
func TestTacticsEndpointReadsTheCampaignBlock(t *testing.T) {
	s, f := newTacticsServer(t, nil)
	dm := dmSession(t, s)
	mk := func(name, payload string) {
		body := `{"kind":"pc","name":` + quote(name) + `,"payload":` + payload + `}`
		if r := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/entities", body, dm); r.Code != http.StatusCreated {
			t.Fatalf("create %s: status %d, body %s", name, r.Code, r.Body)
		}
	}
	mk("Berold", `{"level":5,"class":"fighter","ac":18,"max_hp":44,"saves":{"str":4,"dex":1,"con":3,"int":0,"wis":1,"cha":1}}`)
	mk("Meria", `{"level":5,"class":"wizard","ac":10,"max_hp":20,"saves":{"str":0,"dex":3,"con":1,"int":7,"wis":1,"cha":2}}`)

	rec := hit(t, s, http.MethodPost, "/api/encounters/tactics",
		`{"campaign_id":`+quote(f.campaignID)+`,"monsters":[{"name":"Goblin","cr":"1/4","count":4}]}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("tactics: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Tactics encounter.Analysis `json:"tactics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	a := body.Tactics
	if !a.PartyKnown {
		t.Fatal("the declared party block was not read")
	}
	if len(a.Focus) != 1 || a.Focus[0].Target != "Meria" {
		t.Fatalf("focus = %+v, want the soft pc aimed at", a.Focus)
	}
	// 4 goblins × 5 damage at 75% against AC 10: 15 aimed at Meria.
	if a.Focus[0].PerRound == nil || a.Focus[0].PerRound.Value != 15 {
		t.Errorf("aimed damage = %+v, want 15", a.Focus[0].PerRound)
	}
	if len(a.Spotlight) != 2 {
		t.Fatalf("spotlight rows = %d, want both pcs", len(a.Spotlight))
	}
}

// A player scope is a 403, not a filtered read — the party sheet is DM
// material by definition (ADR 6 on the new surface).
func TestTacticsEndpointIsDMOnlyForCampaigns(t *testing.T) {
	s, f := newTacticsServer(t, nil)
	player := addPlayerMember(t, s, f, "tacplayer", true)

	rec := hit(t, s, http.MethodPost, "/api/encounters/tactics",
		`{"campaign_id":`+quote(f.campaignID)+`,"monsters":[{"name":"Goblin","cr":"1/4","count":1}]}`, player)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("player tactics: status %d, body %s", rec.Code, rec.Body)
	}
}

/* ---------- the design flow's prose gate ---------- */

// designTacticsFixture is a design whose Tactics section asserts a figure:
// "21" traces to nothing the server computed for this roster and party.
const designTacticsFixture = `# Bows on the Trail

## The pitch
Goblins with the range and no patience.

## Roster
4 × Goblin

## Tactics
Round one, all four goblins open from the treeline and their bows put 21 damage on the softest pc.
`

// cleanTacticsFixture is the same design with numberless tactics prose.
const cleanTacticsFixture = `# Bows on the Trail

## The pitch
Goblins with the range and no patience.

## Roster
4 × Goblin

## Tactics
Round one, all four goblins open from the treeline and hold their bows for the softest pc.
`

func TestDesignFlowGatesTheTacticsProse(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		status string
	}{
		{"invented figure rejected", designTacticsFixture, "rejected"},
		{"numberless prose allowed", cleanTacticsFixture, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTacticsServer(t, sseStub(tc.answer))
			keeper := dmSession(t, s) // buildFixture already claimed the install

			rec := call(s, http.MethodPost, "/api/encounter/design",
				`{"idea":"goblins on a trail","party":[3,3,3,3]}`, keeper)
			if rec.Code != http.StatusOK {
				t.Fatalf("design: %d %s", rec.Code, rec.Body)
			}
			var done struct {
				Tactics struct {
					encounter.Analysis
					Prose *struct {
						Prose       string                     `json:"prose"`
						ProseStatus string                     `json:"prose_status"`
						Violations  []encounter.ProseViolation `json:"violations"`
					} `json:"prose"`
				} `json:"tactics"`
			}
			if err := json.Unmarshal([]byte(sseDataFor(t, rec.Body.String(), "done")), &done); err != nil {
				t.Fatalf("decode done: %v", err)
			}
			if done.Tactics.Prose == nil {
				t.Fatal("the gated prose is missing from the done payload")
			}
			if got := done.Tactics.Prose.ProseStatus; got != tc.status {
				t.Fatalf("prose_status = %q, want %q (violations %+v)", got, tc.status, done.Tactics.Prose.Violations)
			}
			// The analysis rides beside the prose either way, and a bare
			// party degrades it honestly.
			if done.Tactics.PartyKnown {
				t.Error("the design flow invented a party block from bare levels")
			}
			if len(done.Tactics.Threat) != 1 {
				t.Fatalf("threat rows = %d, want the goblin", len(done.Tactics.Threat))
			}
			if tc.status == "rejected" && (len(done.Tactics.Prose.Violations) == 0 || done.Tactics.Prose.Violations[0].Token != "21") {
				t.Fatalf("violations = %+v, want the invented 21 named", done.Tactics.Prose.Violations)
			}
		})
	}
}
