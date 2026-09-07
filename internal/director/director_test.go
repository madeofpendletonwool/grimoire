package director

// The director's own tests (MAD-427): the grounding assembles a
// citable basis from the read windows, the prompt carries it, and the
// gate fails closed — no citation, no suggestion; no traced number, no
// suggestion. The doubles below implement the read windows only,
// which is the point: there is nothing else to implement.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/combat"
	"github.com/madeofpendletonwool/grimoire/internal/effects"
	"github.com/madeofpendletonwool/grimoire/internal/encounter"
	"github.com/madeofpendletonwool/grimoire/internal/ledger"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- the doubles (read windows only) ---------- */

type fakeCombats struct {
	fight *combat.Combat
	order []combat.Combatant
	err   error
}

func (f fakeCombats) Active(context.Context, string) (*combat.Combat, []combat.Combatant, error) {
	return f.fight, f.order, f.err
}

type fakeStatblocks struct {
	shelf map[string]encounter.Creature
}

func (f fakeStatblocks) ResolveStatblock(_ context.Context, _, _, name string) (encounter.Creature, bool) {
	c, ok := f.shelf[name]
	return c, ok
}

type fakeLedger struct {
	balances map[string][]ledger.Balance
}

func (f fakeLedger) Balances(_ context.Context, _, entityID string) ([]ledger.Balance, error) {
	return f.balances[entityID], nil
}

type fakeEffects struct {
	rows  []effects.Row
	concs []effects.Row
}

func (f fakeEffects) List(_ context.Context, _, targetID string, _ bool) ([]effects.Row, error) {
	var out []effects.Row
	for _, r := range f.rows {
		if r.TargetID == targetID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f fakeEffects) Concentrations(context.Context, string) ([]effects.Row, error) {
	return f.concs, nil
}

type fakeModel struct {
	reply string
	calls []string
}

func (m *fakeModel) ModelName() string { return "fake-director" }

func (m *fakeModel) Complete(_ context.Context, system, user string) (Completion, error) {
	m.calls = append(m.calls, system+"\n---\n"+user)
	return Completion{Text: m.reply, InputTokens: 10, OutputTokens: 20}, nil
}

/* ---------- the fixture ---------- */

func goblinCreature() encounter.Creature {
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
				Name: "Nimble Escape Dash", Kind: "BONUS_ACTION",
				Desc: "The goblin may Dash as a bonus action.",
			},
		},
	}
}

// testBattle is the canned fight: a wounded wizard pc with spent
// slots, a bloodied goblin with its reaction gone, and a downed
// healer — the state the roadmap's own paragraph describes.
func testBattle() (*combat.Combat, []combat.Combatant) {
	fight := &combat.Combat{ID: "c1", Name: "Ambush at the ford", Round: 2, TurnIndex: 2, Status: combat.StatusActive}
	wizard := combat.Combatant{
		ID: "w1", EntityID: "ent-w", Name: "Mira", Side: combat.SideParty, Kind: combat.KindPC,
		AC: 12, MaxHP: 20, HP: 8, Snapshot: combat.Snapshot{Label: "wizard 5"},
	}
	cleric := combat.Combatant{
		ID: "c2", EntityID: "ent-c", Name: "Thalia", Side: combat.SideParty, Kind: combat.KindPC,
		AC: 14, MaxHP: 16, HP: 0, Downed: true, DeathFailures: 2, DeathSuccesses: 1,
	}
	goblin := combat.Combatant{
		ID: "g1", CombatID: "c1", Name: "Goblin 1", Side: combat.SideFoe, Kind: combat.KindMonster,
		AC: 15, MaxHP: 7, HP: 3, ReactionSpent: true,
		Snapshot: combat.SnapshotOfCreature(goblinCreature()),
	}
	return fight, []combat.Combatant{wizard, cleric, goblin}
}

func testService(model ModelClient) *Service {
	fight, order := testBattle()
	return New(
		fakeCombats{fight: fight, order: order},
		fakeStatblocks{shelf: map[string]encounter.Creature{"Goblin": goblinCreature()}},
		fakeLedger{balances: map[string][]ledger.Balance{
			"ent-w": {
				{Pool: ledger.Pool{Kind: ledger.KindSlot, Name: "1", Size: 4}, Current: 2},
				{Pool: ledger.Pool{Kind: ledger.KindSlot, Name: "3", Size: 2}, Current: 0},
				{Pool: ledger.Pool{Kind: ledger.KindHP, Name: "hp", Size: 20}, Current: 8},
			},
		}},
		fakeEffects{
			rows: []effects.Row{{
				Effect: effects.Effect{TargetID: "ent-w", Name: "hunter's mark", Kind: "spell"},
				Status: effects.StatusActive,
			}},
			concs: []effects.Row{{
				Effect: effects.Effect{TargetID: "ent-w", SourceID: "ent-w", Name: "witch bolt", Kind: "spell"},
				Status: effects.StatusActive,
			}},
		},
		model,
	)
}

/* ---------- the grounding ---------- */

func TestGroundAssemblesBothHalves(t *testing.T) {
	g, err := testService(nil).Ground(context.Background(), "u1", "camp")
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	if g.CombatID != "c1" || g.Round != 2 || g.Turn != "Goblin 1" {
		t.Fatalf("frame: %+v", g)
	}
	var stat, state int
	sawScimitar, sawNimble, sawSlots, sawConc, sawDying, sawReaction := false, false, false, false, false, false
	for _, b := range g.Basis {
		switch b.Kind {
		case KindStatblock:
			stat++
			switch {
			case strings.Contains(b.Text, "Scimitar"):
				sawScimitar = true
			case strings.Contains(b.Text, "Nimble Escape"):
				sawNimble = true
			}
		case KindState:
			state++
			switch {
			case strings.Contains(b.Text, "3rd-level spell slots: 0 of 2 left"):
				sawSlots = true
			case strings.Contains(b.Text, "concentrating on witch bolt"):
				sawConc = true
			case strings.Contains(b.Text, "downed and dying (2 death save failures, 1 successes)"):
				sawDying = true
			case strings.Contains(b.Text, "reaction already spent this round"):
				sawReaction = true
			}
		default:
			t.Fatalf("unknown basis kind %q", b.Kind)
		}
	}
	if !sawScimitar || !sawNimble {
		t.Fatalf("statblock half missing actions/traits: %+v", g.Basis)
	}
	if !sawSlots || !sawConc || !sawDying || !sawReaction {
		t.Fatalf("state half missing the live facts: %+v", g.Basis)
	}
	if stat == 0 || state == 0 {
		t.Fatalf("empty half: %d statblock, %d state", stat, state)
	}

	// IDs are numbered per kind, sequential from 1.
	si, li := 0, 0
	for _, b := range g.Basis {
		if b.Kind == KindStatblock {
			si++
			if b.ID != fmt.Sprintf("S%d", si) {
				t.Fatalf("statblock id %q want S%d", b.ID, si)
			}
		} else {
			li++
			if b.ID != fmt.Sprintf("L%d", li) {
				t.Fatalf("state id %q want L%d", b.ID, li)
			}
		}
	}

	// The hp pool stays out of the basis — the tracker owns hp in a
	// fight.
	for _, b := range g.Basis {
		if strings.Contains(b.Text, "hp:") || strings.Contains(b.Text, "hit points:") {
			t.Fatalf("hp pool leaked into the basis: %s", b.Text)
		}
	}
}

func TestGroundWithoutActiveBattle(t *testing.T) {
	svc := New(fakeCombats{}, nil, nil, nil, nil)
	if _, err := svc.Ground(context.Background(), "u1", "camp"); err == nil ||
		!strings.Contains(err.Error(), "no active battle") {
		t.Fatalf("want invalid-no-active error, got %v", err)
	}
}

func TestGroundDegradationsCarryCaveats(t *testing.T) {
	fight, order := testBattle()
	// The shelf resolves nothing: every statblock name misses.
	svc := New(fakeCombats{fight: fight, order: order}, fakeStatblocks{}, nil, nil, nil)
	g, err := svc.Ground(context.Background(), "u1", "camp")
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	if len(g.Caveats) == 0 || !strings.Contains(strings.Join(g.Caveats, "; "), "no longer resolves") {
		t.Fatalf("want unresolved-name caveat, got %v", g.Caveats)
	}
	for _, b := range g.Basis {
		if b.Kind == KindStatblock {
			t.Fatalf("unresolved name produced a statblock line: %+v", b)
		}
	}

	// No statblock window at all: the caveat says live-state-only.
	svc = New(fakeCombats{fight: fight, order: order}, nil, nil, nil, nil)
	if g, err = svc.Ground(context.Background(), "u1", "camp"); err != nil {
		t.Fatalf("ground: %v", err)
	}
	if len(g.Caveats) == 0 || !strings.Contains(strings.Join(g.Caveats, "; "), "no statblock window") {
		t.Fatalf("want no-window caveat, got %v", g.Caveats)
	}
}

func TestUserMessageCarriesBasisAndQuestion(t *testing.T) {
	g, err := testService(nil).Ground(context.Background(), "u1", "camp")
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	msg := UserMessage(g, "should the goblins run?")
	for _, want := range []string{
		"[S1]", "Scimitar", "[L1]", "Round 2", "The DM asks: should the goblins run?",
		"STATBLOCK FACTS", "LIVE STATE",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("prompt missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "[S0]") || strings.Contains(msg, "[L0]") {
		t.Fatalf("prompt carries a zero id:\n%s", msg)
	}
}

/* ---------- the gate ---------- */

func gateFixture(t *testing.T) *Grounding {
	t.Helper()
	g, err := testService(nil).Ground(context.Background(), "u1", "camp")
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	return g
}

// findID resolves the basis id of the first line containing substr, so
// the gate tests cite by content instead of by guessed position.
func findID(t *testing.T, g *Grounding, substr string) string {
	t.Helper()
	for _, b := range g.Basis {
		if strings.Contains(b.Text, substr) {
			return b.ID
		}
	}
	t.Fatalf("no basis line contains %q", substr)
	return ""
}

func TestGateKeepsCitedSuggestions(t *testing.T) {
	g := gateFixture(t)
	scimitar := findID(t, g, "Scimitar")
	wizard := findID(t, g, "Mira (pc")
	raw := "```json\n{\"suggestions\":[{\"actor\":\"Goblin 1\",\"action\":\"Swing the scimitar at Mira (she has 8 hp left)\",\"reasoning\":\"Mira is the only caster standing\",\"basis\":[\"" + scimitar + "\",\"" + wizard + "\",\"S99\"]}]}\n```"
	out, dropped, err := Gate(g, raw)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if dropped != 0 || len(out) != 1 {
		t.Fatalf("want 1 kept 0 dropped, got %d kept %d dropped", len(out), dropped)
	}
	// The unknown id S99 is stripped; the valid ones ride deduped.
	if len(out[0].BasisIDs) != 2 || out[0].BasisIDs[0] != scimitar || out[0].BasisIDs[1] != wizard {
		t.Fatalf("citations not resolved/deduped: %v", out[0].BasisIDs)
	}
}

func TestGateDropsUncitedSuggestions(t *testing.T) {
	g := gateFixture(t)
	raw := "```json\n{\"suggestions\":[" +
		`{"actor":"Goblin 1","action":"Flee east","reasoning":"no basis at all","basis":[]},` +
		`{"actor":"Goblin 1","action":"Flee west","reasoning":"only unknown ids","basis":["X1","S999"]},` +
		`{"actor":"Goblin 1","action":"","reasoning":"empty action","basis":["` + findID(t, g, "Goblin:") + `"]}` +
		"]}\n```"
	out, dropped, err := Gate(g, raw)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(out) != 0 || dropped != 3 {
		t.Fatalf("want all three dropped, got %d kept %d dropped", len(out), dropped)
	}
}

func TestGateDropsInventedNumbers(t *testing.T) {
	g := gateFixture(t)
	scimitar := findID(t, g, "Scimitar")
	raw := "```json\n{\"suggestions\":[" +
		// 17 appears in no cited line.
		`{"actor":"Goblin 1","action":"Hit Mira for 17 damage","reasoning":"big swing","basis":["` + scimitar + `"]},` +
		// The same figure traced: the scimitar line carries +4 to hit.
		`{"actor":"Goblin 1","action":"Swing at +4 to hit","reasoning":"per its statblock","basis":["` + scimitar + `"]},` +
		// Dice grammar is vocabulary, not a figure: 1d6 needs no trace.
		`{"actor":"Goblin 1","action":"Roll the scimitar's 1d6 + 2 slashing","reasoning":"per its statblock","basis":["` + scimitar + `"]}` +
		"]}\n```"
	out, dropped, err := Gate(g, raw)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if dropped != 1 || len(out) != 2 {
		t.Fatalf("want the invented 17 dropped and two kept, got %d kept %d dropped", len(out), dropped)
	}
	for _, s := range out {
		if strings.Contains(s.Action, "17") {
			t.Fatalf("invented figure survived: %+v", s)
		}
	}
}

func TestGateNumbersMustComeFromCitedLinesOnly(t *testing.T) {
	g := gateFixture(t)
	// 20 appears in the basis (Mira's max hp) but NOT in the lines
	// this suggestion cites — citing the scimitar alone does not
	// license the wizard's numbers.
	raw := "```json\n{\"suggestions\":[{\"actor\":\"Goblin 1\",\"action\":\"Mira has 20 hp, finish her\",\"reasoning\":\"wrong citation\",\"basis\":[\"" + findID(t, g, "Scimitar") + "\"]}]}\n```"
	out, dropped, err := Gate(g, raw)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(out) != 0 || dropped != 1 {
		t.Fatalf("want dropped, got %d kept %d dropped", len(out), dropped)
	}
}

func TestGateMalformedRepliesAreErrors(t *testing.T) {
	g := gateFixture(t)
	for name, raw := range map[string]string{
		"no block":       "the goblins should flee, obviously",
		"unparseable":    "```json\n{suggestions: [}\n```",
		"trailing table": "```json\n{\"suggestions\":[]}\n```\nactually, also...",
	} {
		if _, _, err := Gate(g, raw); err == nil {
			t.Fatalf("%s: want error, got none", name)
		}
	}
}

func TestGateCapsSuggestions(t *testing.T) {
	g := gateFixture(t)
	var items []string
	for i := 0; i < maxSuggestions+3; i++ {
		items = append(items, fmt.Sprintf(
			`{"actor":"Goblin %d","action":"swing the scimitar","reasoning":"r","basis":["%s"]}`, i+1, findID(t, g, "Scimitar")))
	}
	raw := "```json\n{\"suggestions\":[" + strings.Join(items, ",") + "]}\n```"
	out, dropped, err := Gate(g, raw)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if len(out) != maxSuggestions || dropped != 3 {
		t.Fatalf("want %d kept 3 dropped, got %d kept %d dropped", maxSuggestions, len(out), dropped)
	}
}

/* ---------- the advisory pass ---------- */

func TestAdviseFailClosed(t *testing.T) {
	model := &fakeModel{reply: "```json\n{\"suggestions\":[" +
		`{"actor":"Goblin 1","action":"Hide behind the rocks","reasoning":"nimble escape","basis":["S3","L2"]},` +
		`{"actor":"Goblin 1","action":"Conjure a demon lord","reasoning":"nope","basis":[]}` +
		"]}\n```"}
	svc := testService(model)
	g, err := svc.Ground(context.Background(), "u1", "camp")
	if err != nil {
		t.Fatalf("ground: %v", err)
	}
	res, err := svc.Advise(context.Background(), g, "")
	if err != nil {
		t.Fatalf("advise: %v", err)
	}
	if len(res.Suggestions) != 1 || res.Dropped != 1 {
		t.Fatalf("want 1 kept 1 dropped, got %+v dropped=%d", res.Suggestions, res.Dropped)
	}
	if len(model.calls) != 1 {
		t.Fatalf("want one model call, got %d", len(model.calls))
	}
	if !strings.Contains(model.calls[0], "you never roll, never take a turn, never change state") {
		t.Fatalf("system prompt lost its advisory clause")
	}
	if !strings.Contains(model.calls[0], "Nimble Escape") {
		t.Fatalf("user prompt lost the basis")
	}
}

func TestAdviseWithoutModel(t *testing.T) {
	svc := testService(nil)
	g, _ := svc.Ground(context.Background(), "u1", "camp")
	if _, err := svc.Advise(context.Background(), g, ""); err == nil {
		t.Fatalf("want error with no model wired")
	}
}

func TestGroundRejectsMissingCombatWindow(t *testing.T) {
	svc := New(nil, nil, nil, nil, nil)
	if _, err := svc.Ground(context.Background(), "u1", "camp"); err == nil ||
		!strings.Contains(err.Error(), "no combat window") {
		t.Fatalf("want no-window error, got %v", err)
	}
}

func TestNoActiveBattleIsInvalidShape(t *testing.T) {
	svc := New(fakeCombats{}, nil, nil, nil, nil)
	_, err := svc.Ground(context.Background(), "u1", "camp")
	if !isInvalid(err) {
		t.Fatalf("want campaign.ErrInvalid, got %v", err)
	}
}

func isInvalid(err error) bool {
	return err != nil && strings.Contains(err.Error(), campaign.ErrInvalid.Error())
}
