package server

// The encounter director's handler tests (MAD-427): permissions and
// availability, the advisory pass over a live battle with citations
// resolved on the wire, the gate dropping what does not trace — and
// the acceptance proof that a director call leaves the battle, its
// journal, the dice feed and the ledger byte-identical: advisory only,
// demonstrated, not promised.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/auth"
	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/canon"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/dice"
	"github.com/madeofpendletonwool/grimoire/internal/director"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/gamesession"
	"github.com/madeofpendletonwool/grimoire/internal/index"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/llm"
	"github.com/madeofpendletonwool/grimoire/internal/migrate"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

// directorShelf is the resolver the director tests resolve through: a
// goblin with real statblock text — a trait and a priced attack — so
// the grounding has statblock lines worth citing.
type directorShelf struct{}

func (directorShelf) ResolveStatblock(_ context.Context, _, _, name string) (encounter.Creature, bool) {
	if name != "Goblin" {
		return encounter.Creature{}, false
	}
	return encounter.Creature{
		Slug: "goblin", Name: "Goblin", CR: "1/4", XP: 50, AC: 15, HP: 7,
		Speeds:    map[string]int{"walk": 30},
		Abilities: &statblock.Abilities{Str: 8, Dex: 14, Con: 10},
		Traits: []encounter.NamedText{{
			Name: "Nimble Escape",
			Desc: "The goblin can take the Disengage or Hide action as a bonus action on each of its turns.",
		}},
		Actions: []encounter.NamedText{
			{
				Name: "Scimitar", Kind: "ACTION",
				Desc: "Melee Weapon Attack: +4 to hit, reach 5 ft., one target. Hit: 5 (1d6 + 2) slashing damage.",
			},
			{
				Name: "Shortbow", Kind: "ACTION",
				Desc: "Ranged Weapon Attack: +4 to hit, range 80/320 ft., one target. Hit: 5 (1d6 + 2) piercing damage.",
			},
		},
	}, true
}

// groundedModel plays a model that grounds itself: it reads the basis
// ids out of the prompt it was handed and cites real ones. The flags
// add deliberately bad suggestions alongside, so the gate has
// something to catch on the wire.
type groundedModel struct {
	uncited   bool
	invented  bool
	breakJSON bool
	calls     []string
}

func (m *groundedModel) ModelName() string { return "fake-director" }

var basisIDRE = regexp.MustCompile(`\[(S\d+|L\d+)\]`)

func (m *groundedModel) Complete(_ context.Context, system, user string) (director.Completion, error) {
	m.calls = append(m.calls, system+"\n--\n"+user)
	var picked []string
	seen := map[string]bool{}
	for _, match := range basisIDRE.FindAllStringSubmatch(user, -1) {
		id := match[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		picked = append(picked, id)
		if len(picked) == 2 {
			break
		}
	}
	if len(picked) == 0 {
		return director.Completion{}, fmt.Errorf("no basis ids in the prompt")
	}
	suggestions := []string{fmt.Sprintf(
		`{"actor":"the goblins","action":"Concentrate scimitar work on the caster and Hide after the swing","reasoning":"the caster is nearly spent and Nimble Escape makes the retreat free","basis":[%q,%q]}`,
		picked[0], picked[len(picked)-1])}
	if m.uncited {
		suggestions = append(suggestions,
			`{"actor":"the goblins","action":"Summon hobgoblin reinforcements","reasoning":"no basis at all","basis":[]}`)
	}
	if m.invented {
		suggestions = append(suggestions, fmt.Sprintf(
			`{"actor":"the goblins","action":"Fall back 100 feet and shower the party with arrows","reasoning":"an invented range","basis":[%q]}`,
			picked[0]))
	}
	body := "{\"suggestions\":[" + strings.Join(suggestions, ",") + "]}"
	if m.breakJSON {
		body = "{suggestions: [}"
	}
	return director.Completion{Text: "```json\n" + body + "\n```", InputTokens: 100, OutputTokens: 200}, nil
}

// newDirectorServer boots the full mechanical stack the way runServe
// does, plus the director over it: the tracker, the ledger, the
// effects engine and the statblock resolver, all real stores over a
// migrated database.
func newDirectorServer(t *testing.T, model director.ModelClient) (*Server, *fixture) {
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
	sessions, err := gamesession.New(store.DB())
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	offlineCanon, err := canon.NewOffline(store.DB())
	if err != nil {
		t.Fatalf("open canon store: %v", err)
	}
	diceStore, err := dice.New(store.DB(), campaigns, sessions)
	if err != nil {
		t.Fatalf("open dice store: %v", err)
	}
	effectEngine, err := effects.New(store.DB(), campaigns, nil)
	if err != nil {
		t.Fatalf("open effects store: %v", err)
	}
	ledgerEngine, err := ledger.New(store.DB(), campaigns, offlineCanon)
	if err != nil {
		t.Fatalf("open ledger store: %v", err)
	}
	combatEngine, err := combat.New(store.DB(), campaigns, sessions, diceStore)
	if err != nil {
		t.Fatalf("open combat store: %v", err)
	}
	combatEngine = combatEngine.WithEffects(effectEngine).WithHitPoints(ledgerEngine).WithResolver(directorShelf{})

	key := ""
	if model != nil {
		key = "test"
	}
	s, err := New(store, llm.New(llm.Config{APIKey: key}), nil, nil, nil, nil, nil, nil, Auth{Users: users}, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s = s.WithCampaigns(campaigns, knowledgeStore).
		WithDice(diceStore).WithEffects(effectEngine).
		WithLedger(ledgerEngine).WithCombat(combatEngine).
		WithDirector(director.New(combatEngine, directorShelf{}, ledgerEngine, effectEngine, model))
	f := buildFixture(t, s)

	// A typed sheet on the fixture pc through the real surface — the
	// ledger's pools seed from it on the write.
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodPut, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/sheet",
		`{"classes":[{"class":"wizard","level":5}],"ac":13,"max_hp":32,`+
			`"spellcasting":{"slots":{"1":4,"2":3,"3":2}}}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sheet: status %d, body %s", rec.Code, rec.Body)
	}
	return s, &f
}

// startDirectorBattle starts the fixture fight — the wizard and two
// goblins — and spends the wizard's third-level slots dry, the exact
// state the roadmap says the director must know.
func startDirectorBattle(t *testing.T, s *Server, f fixture) string {
	t.Helper()
	dm := dmSession(t, s)

	// Both 3rd-level slots spent: "out of 3rd levels" becomes a basis
	// line a suggestion can cite.
	var res struct {
		Balances []struct {
			Pool struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"pool"`
		} `json:"balances"`
	}
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("resources: status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode resources: %v", err)
	}
	for _, b := range res.Balances {
		if b.Pool.Kind == "slot" && b.Pool.Name == "3" {
			for i := 0; i < 2; i++ {
				r := hit(t, s, http.MethodPost,
					"/api/campaigns/"+f.campaignID+"/characters/"+f.pcID+"/resources/"+b.Pool.ID+"/transactions",
					`{"kind":"spend","amount":1,"note":"fireball"}`, dm)
				if r.Code != http.StatusOK && r.Code != http.StatusCreated {
					t.Fatalf("spend slot: status %d, body %s", r.Code, r.Body)
				}
			}
		}
	}

	body := `{"pcs":["` + f.pcID + `"],"monsters":[{"name":"Goblin","count":2}]}`
	rec = hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat", body, dm)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: status %d, body %s", rec.Code, rec.Body)
	}
	var started struct {
		Combat struct {
			ID string `json:"id"`
		} `json:"combat"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode started: %v", err)
	}
	return started.Combat.ID
}

/* ---------- permissions and availability ---------- */

func TestDirectorPermissions(t *testing.T) {
	s, f := newDirectorServer(t, &groundedModel{})
	dm := dmSession(t, s)
	player := addPlayerMember(t, s, *f, "thalia", true)
	startDirectorBattle(t, s, *f)

	// The director reads the DM's screen: a player reaches it never.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat/director", `{}`, player); rec.Code != http.StatusForbidden {
		t.Fatalf("player reached the director: status %d, body %s", rec.Code, rec.Body)
	}
	// The DM does.
	if rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat/director", `{}`, dm); rec.Code != http.StatusOK {
		t.Fatalf("dm: status %d, body %s", rec.Code, rec.Body)
	}
}

func TestDirectorAvailability(t *testing.T) {
	s, f := newDirectorServer(t, &groundedModel{})
	dm := dmSession(t, s)
	startDirectorBattle(t, s, *f)
	path := "/api/campaigns/" + f.campaignID + "/combat/director"

	// Unwired engine: 503.
	saved := s.director
	s.director = nil
	if rec := hit(t, s, http.MethodPost, path, `{}`, dm); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired director: status %d", rec.Code)
	}
	s.director = saved

	// Unwired combat store: 503 through the shared gate.
	savedCombats := s.combats
	s.combats = nil
	if rec := hit(t, s, http.MethodPost, path, `{}`, dm); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired combat: status %d", rec.Code)
	}
	s.combats = savedCombats

	// Unconfigured model: 503 with the env hint, npc-ask style.
	savedLLM := s.llm
	s.llm = llm.New(llm.Config{})
	if rec := hit(t, s, http.MethodPost, path, `{}`, dm); rec.Code != http.StatusServiceUnavailable ||
		!strings.Contains(rec.Body.String(), "ANTHROPIC_API_KEY") {
		t.Fatalf("unconfigured llm: status %d, body %s", rec.Code, rec.Body)
	}
	s.llm = savedLLM

	// No active battle: 400 — there is nothing to direct.
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combats/"+startDirectorBattleCombatID(t, s, *f)+"/end", `{"reason":"done"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("end battle: status %d, body %s", rec.Code, rec.Body)
	}
	if rec := hit(t, s, http.MethodPost, path, `{}`, dm); rec.Code != http.StatusBadRequest {
		t.Fatalf("no active battle: status %d, body %s", rec.Code, rec.Body)
	}
}

// startDirectorBattleCombatID returns the id of the campaign's one
// battle without touching it.
func startDirectorBattleCombatID(t *testing.T, s *Server, f fixture) string {
	t.Helper()
	dm := dmSession(t, s)
	rec := hit(t, s, http.MethodGet, "/api/campaigns/"+f.campaignID+"/combat", "", dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("active combat: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Combat *struct {
			ID string `json:"id"`
		} `json:"combat"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Combat == nil {
		t.Fatalf("decode active combat: %v %s", err, rec.Body)
	}
	return body.Combat.ID
}

/* ---------- the advisory pass ---------- */

func TestDirectorAdvisesWithCitations(t *testing.T) {
	model := &groundedModel{}
	s, f := newDirectorServer(t, model)
	dm := dmSession(t, s)
	startDirectorBattle(t, s, *f)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat/director",
		`{"question":"who do the goblins press?"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("director: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Combat struct {
			ID    string `json:"id"`
			Round int    `json:"round"`
		} `json:"combat"`
		Suggestions []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Basis  []struct {
				ID     string `json:"id"`
				Kind   string `json:"kind"`
				Source string `json:"source"`
				Text   string `json:"text"`
			} `json:"basis"`
		} `json:"suggestions"`
		Basis []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Text string `json:"text"`
		} `json:"basis"`
		Dropped int    `json:"dropped"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Model != "fake-director" || body.Combat.ID == "" || body.Combat.Round != 1 {
		t.Fatalf("frame: %+v", body)
	}
	if len(body.Suggestions) != 1 || body.Dropped != 0 {
		t.Fatalf("want one gated suggestion, got %d (dropped %d): %s", len(body.Suggestions), body.Dropped, rec.Body)
	}
	sg := body.Suggestions[0]
	if len(sg.Basis) == 0 {
		t.Fatalf("suggestion reached the API without a citation: %+v", sg)
	}
	for _, c := range sg.Basis {
		if c.Kind != "statblock" && c.Kind != "state" {
			t.Fatalf("citation kind %q", c.Kind)
		}
		if c.Text == "" || c.Source == "" {
			t.Fatalf("citation not resolved: %+v", c)
		}
	}
	// The full basis rides so the surface can show the state beside
	// the suggestion: both halves, with the wizard's dry third-level
	// slots among the state lines.
	var sawSlots, sawStatblock bool
	for _, b := range body.Basis {
		if strings.Contains(b.Text, "3rd-level spell slots: 0 of 2 left") {
			sawSlots = true
		}
		if b.Kind == "statblock" {
			sawStatblock = true
		}
	}
	if !sawSlots || !sawStatblock {
		t.Fatalf("basis missing halves: %+v", body.Basis)
	}

	// The prompt the model received carried the grounded basis and
	// the DM's question — and the advisory clause.
	prompt := model.calls[0]
	for _, want := range []string{
		"Scimitar", "Nimble Escape", "3rd-level spell slots: 0 of 2 left",
		"who do the goblins press?", "you never roll, never take a turn, never change state",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
}

func TestDirectorGateDropsOnTheWire(t *testing.T) {
	s, f := newDirectorServer(t, &groundedModel{uncited: true, invented: true})
	dm := dmSession(t, s)
	startDirectorBattle(t, s, *f)

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat/director", `{}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("director: status %d, body %s", rec.Code, rec.Body)
	}
	var body struct {
		Suggestions []struct {
			Action string `json:"action"`
			Basis  []struct {
				ID string `json:"id"`
			} `json:"basis"`
		} `json:"suggestions"`
		Dropped int `json:"dropped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Suggestions) != 1 || body.Dropped != 2 {
		t.Fatalf("want 1 kept 2 dropped, got %d kept %d dropped: %s",
			len(body.Suggestions), body.Dropped, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "reinforcements") || strings.Contains(rec.Body.String(), "100 feet") {
		t.Fatalf("a dropped suggestion leaked to the wire: %s", rec.Body)
	}
	if len(body.Suggestions[0].Basis) == 0 {
		t.Fatalf("kept suggestion has no citation")
	}
}

func TestDirectorBadReplyIsBadGateway(t *testing.T) {
	s, f := newDirectorServer(t, &groundedModel{breakJSON: true})
	dm := dmSession(t, s)
	startDirectorBattle(t, s, *f)
	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat/director", `{}`, dm)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 for an unparseable reply, got %d: %s", rec.Code, rec.Body)
	}
}

/* ---------- advisory only, demonstrated ---------- */

// TestDirectorMutatesNothing is the acceptance proof: one advisory
// pass over a live battle, and every write surface the mechanics own —
// the battle's rows, its append-only journal, the dice feed (the
// director never rolls), the ledger — comes back byte-identical.
func TestDirectorMutatesNothing(t *testing.T) {
	s, f := newDirectorServer(t, &groundedModel{uncited: true})
	dm := dmSession(t, s)
	cid := startDirectorBattle(t, s, *f)

	get := func(path string) string {
		rec := hit(t, s, http.MethodGet, path, "", dm)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	before := map[string]string{
		"combat":     get("/api/campaigns/" + f.campaignID + "/combat"),
		"battle+log": get("/api/campaigns/" + f.campaignID + "/combats/" + cid),
		"rolls":      get("/api/campaigns/" + f.campaignID + "/rolls"),
		"resources":  get("/api/campaigns/" + f.campaignID + "/characters/" + f.pcID + "/resources"),
	}

	rec := hit(t, s, http.MethodPost, "/api/campaigns/"+f.campaignID+"/combat/director",
		`{"question":"anything at all"}`, dm)
	if rec.Code != http.StatusOK {
		t.Fatalf("director: status %d, body %s", rec.Code, rec.Body)
	}

	for name, want := range before {
		if got := get(map[string]string{
			"combat":     "/api/campaigns/" + f.campaignID + "/combat",
			"battle+log": "/api/campaigns/" + f.campaignID + "/combats/" + cid,
			"rolls":      "/api/campaigns/" + f.campaignID + "/rolls",
			"resources":  "/api/campaigns/" + f.campaignID + "/characters/" + f.pcID + "/resources",
		}[name]); got != want {
			t.Fatalf("%s changed across a director call:\nbefore %s\nafter  %s", name, want, got)
		}
	}
}
