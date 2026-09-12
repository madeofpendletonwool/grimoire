package grammar

// The phrasing table: real table talk → expected Action, plus the
// negative cases that must return no-parse rather than a wrong Action —
// "a wrong parse is worse than no parse" is this package's acceptance
// line, and the negatives are half of it. The fixtures are live engine
// states, driven through real Apply calls, because the whole point of
// resolving against the current game state is that it works on the state
// the engine actually produces.

import (
	"context"
	"reflect"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

/* ---------- fixtures ---------- */

type fixture struct {
	st                *engine.State
	forest, soldier   int64
	solRing, solRing2 int64
	atraxa, krenko    int64
}

func apply(t *testing.T, st *engine.State, a engine.Action) {
	t.Helper()
	evs, err := engine.Apply(st, a)
	if err != nil {
		t.Fatalf("apply %s: %v", a.Kind, err)
	}
	st.FoldInto(evs)
}

// newFixture builds the standing pod: Collin(1), Sarah(2), Bob(3),
// Dave(4), mid Collin's first precombat main with priority, Collin's
// Forest and two Sol Rings (one already tapped) and a 1/1 Soldier on
// the field, Sarah's Atraxa across from him, and Collin's commander
// Krenko parked in the command zone for the recast shape.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := engine.NewState()
	apply(t, st, engine.Action{Kind: engine.ActionStartGame, Seats: []engine.SeatConfig{
		{Seat: 1, Name: "Collin", Commander: "Krenko, Mob Boss"},
		{Seat: 2, Name: "Sarah", Commander: "Atraxa, Praetors' Voice"},
		{Seat: 3, Name: "Bob", Commander: "Urza, Lord High Artificer"},
		{Seat: 4, Name: "Dave", Commander: "The Ring tempts you"},
	}})
	apply(t, st, engine.Action{Kind: engine.ActionAdvance}) // upkeep
	apply(t, st, engine.Action{Kind: engine.ActionAdvance}) // precombat main
	f := &fixture{st: st}
	apply(t, st, engine.Action{Kind: engine.ActionPlayLand, Seat: 1, Card: "Forest",
		Base: &engine.BaseChars{Name: "Forest", Types: []string{"Land"}}})
	apply(t, st, engine.Action{Kind: engine.ActionCreateToken, Seat: 1,
		Token: &engine.TokenSpec{Name: "Sol Ring", Types: []string{"Artifact"}}})
	apply(t, st, engine.Action{Kind: engine.ActionCreateToken, Seat: 1,
		Token: &engine.TokenSpec{Name: "Sol Ring", Types: []string{"Artifact"}}})
	apply(t, st, engine.Action{Kind: engine.ActionCreateToken, Seat: 1,
		Token: &engine.TokenSpec{Name: "Soldier", Types: []string{"Creature"},
			Power: ptr(1), Toughness: ptr(1)}})
	apply(t, st, engine.Action{Kind: engine.ActionCreateToken, Seat: 2,
		Token: &engine.TokenSpec{Name: "Atraxa", Types: []string{"Creature"},
			Power: ptr(4), Toughness: ptr(4)}})
	// The second Sol Ring is tapped, so "tap Sol Ring" prefers the first.
	for id, o := range st.Objects {
		if o.Identity.Token == nil {
			continue
		}
		switch o.Identity.Token.Name {
		case "Forest":
			f.forest = id
		case "Sol Ring":
			if f.solRing == 0 {
				f.solRing = id
			} else {
				f.solRing2 = id
			}
		case "Soldier":
			f.soldier = id
		case "Atraxa":
			f.atraxa = id
		}
	}
	apply(t, st, engine.Action{Kind: engine.ActionTap, Seat: 1, Object: f.solRing2})
	// Krenko into the command zone for the recast shape.
	apply(t, st, engine.Action{Kind: engine.ActionCreateToken, Seat: 1,
		Token: &engine.TokenSpec{Name: "Krenko, Mob Boss", Types: []string{"Creature"}}})
	for id, o := range st.Objects {
		if o.Zone == engine.ZoneBattlefield && o.Identity.Token != nil &&
			o.Identity.Token.Name == "Krenko, Mob Boss" {
			f.krenko = id
		}
	}
	apply(t, st, engine.Action{Kind: engine.ActionMoveZone, Seat: 1, Object: f.krenko,
		ToZone: engine.ZoneCommand, Cause: "commander"})
	return f
}

// combatFixture advances the pod into Collin's declare_attackers step,
// Soldier already attacking Sarah, ready for the declaration shapes.
func combatFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	apply(t, f.st, engine.Action{Kind: engine.ActionAdvance}) // beginning of combat
	apply(t, f.st, engine.Action{Kind: engine.ActionAdvance}) // declare_attackers
	return f
}

// blockFixture: past declarations, Soldier attacking Sarah, in declare
// blockers, where Atraxa can block it.
func blockFixture(t *testing.T) *fixture {
	t.Helper()
	f := combatFixture(t)
	apply(t, f.st, engine.Action{Kind: engine.ActionDeclareAttackers, Seat: 1,
		Attackers: []engine.AttackAssignment{{Object: f.soldier, TargetSeat: 2}}})
	apply(t, f.st, engine.Action{Kind: engine.ActionAdvance}) // declare_blockers
	return f
}

func ptr(n int) *int { return &n }

// fakeNames is the seam for the table: a fixed map of the pod's spoken
// names with the confidences and types the real tiers would produce.
type fakeNames map[string]NameInfo

func (n fakeNames) ResolveName(_ context.Context, _ int, spoken string) NameInfo {
	if r, ok := n[norm(spoken)]; ok {
		return r
	}
	return NameInfo{}
}

func norm(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			out = append(out, r)
		}
	}
	return string(out)
}

func withNames() fakeNames {
	return fakeNames{
		"rhysticstudy":  {Card: "Rhystic Study", Confidence: 1.0, Types: []string{"Enchantment"}},
		"rhystic":       {Card: "Rhystic Study", Confidence: 0.95, Types: []string{"Enchantment"}},
		"study":         {Card: "Rhystic Study", Confidence: 0.95, Types: []string{"Enchantment"}},
		"cultivate":     {Card: "Cultivate", Confidence: 1.0, Types: []string{"Sorcery"}},
		"lightningbolt": {Card: "Lightning Bolt", Confidence: 1.0, Types: []string{"Instant"}},
		"bolt":          {Card: "Lightning Bolt", Confidence: 0.95, Types: []string{"Instant"}},
		"solring":       {Card: "Sol Ring", Confidence: 1.0, Types: []string{"Artifact"}},
		"commandtower":  {Card: "Command Tower", Confidence: 1.0, Types: []string{"Land"}},
		"krenko":        {Card: "Krenko, Mob Boss", Confidence: 0.95, Types: []string{"Creature"}},
		"giantgrowth":   {Card: "Giant Growth", Confidence: 1.0, Types: []string{"Instant"}},
	}
}

// Setup injects the standard name seam.
func init() { defaultNames = withNames() }

var defaultNames Names

// parse runs one utterance for one seat through the standard fixture.
func (f *fixture) parse(seat int, in string) Result {
	return Parse(context.Background(), seat, in, f.st, defaultNames)
}

/* ---------- the phrasing table ---------- */

type pcase struct {
	in     string
	seat   int // speaker; 0 means 1
	actor  int // expected acting seat on the action; 0 means the speaker
	ok     bool
	kind   engine.ActionKind
	check  func(t *testing.T, a engine.Action)
	confLo float64
	confHi float64
}

func TestPhrasingTable(t *testing.T) {
	f := newFixture(t)
	for _, c := range table(f) {
		seat := c.seat
		if seat == 0 {
			seat = 1
		}
		res := f.parse(seat, c.in)
		if res.OK != c.ok {
			t.Errorf("%q: OK = %v, want %v (action %+v)", c.in, res.OK, c.ok, res.Action)
			continue
		}
		if !c.ok {
			continue
		}
		if res.Action.Kind != c.kind {
			t.Errorf("%q: kind = %s, want %s", c.in, res.Action.Kind, c.kind)
			continue
		}
		wantSeat := c.actor
		if wantSeat == 0 {
			wantSeat = seat
		}
		if res.Action.Seat != wantSeat {
			t.Errorf("%q: acting seat = %d, want %d", c.in, res.Action.Seat, wantSeat)
		}
		if res.Action.Source != "grammar" {
			t.Errorf("%q: source = %q, want grammar", c.in, res.Action.Source)
		}
		if c.confLo != 0 && res.Action.Confidence < c.confLo {
			t.Errorf("%q: confidence %v below floor %v", c.in, res.Action.Confidence, c.confLo)
		}
		if c.confHi != 0 && res.Action.Confidence > c.confHi {
			t.Errorf("%q: confidence %v above ceiling %v", c.in, res.Action.Confidence, c.confHi)
		}
		if c.check != nil {
			c.check(t, res.Action)
		}
	}
}

// table builds the full case list; helpers keep the assertions terse.
func table(f *fixture) []pcase {
	return []pcase{
		/* flow */
		{in: "pass", ok: true, kind: engine.ActionPassPriority, confLo: 1.0},
		{in: "go", ok: true, kind: engine.ActionPassPriority, confLo: 1.0},
		{in: "resolves", ok: true, kind: engine.ActionPassPriority},
		{in: "that resolves", ok: true, kind: engine.ActionPassPriority},
		{in: "let it resolve", ok: true, kind: engine.ActionPassPriority},
		{in: "I'll take it", ok: true, kind: engine.ActionPassPriority, confLo: 1.0},
		{in: "no response", ok: true, kind: engine.ActionPassPriority},
		{in: "nope", ok: true, kind: engine.ActionPassPriority},
		{in: "no blocks", ok: true, kind: engine.ActionPassPriority},
		{in: "your turn", ok: true, kind: engine.ActionPassPriority, confHi: 0.9},
		{in: "next", ok: true, kind: engine.ActionAdvance},
		{in: "go to combat", ok: true, kind: engine.ActionAdvance, confHi: 0.9},
		{in: "Bob takes it", ok: true, kind: engine.ActionPassPriority, actor: 3},
		{in: "concede", ok: true, kind: engine.ActionConcede},
		{in: "I'm dead", ok: true, kind: engine.ActionConcede},

		/* counters and flags */
		{in: "+1/+1 counter on Atraxa", ok: true, kind: engine.ActionAdjustCounters,
			check: counterOn(f.atraxa, "+1/+1", 1)},
		{in: "put two +1/+1 counters on Atraxa", ok: true, kind: engine.ActionAdjustCounters,
			check: counterOn(f.atraxa, "+1/+1", 2)},
		{in: "two energy", ok: true, kind: engine.ActionAdjustCounters,
			check: seatCounter(1, "energy", 2), confLo: 1.0},
		{in: "gain two energy", ok: true, kind: engine.ActionAdjustCounters,
			check: seatCounter(1, "energy", 2)},
		{in: "3 poison on Bob", ok: true, kind: engine.ActionAdjustCounters,
			check: seatCounter(3, "poison", 3)},
		{in: "Sarah takes two poison", ok: true, kind: engine.ActionAdjustCounters,
			check: seatCounter(2, "poison", 2)},
		{in: "2 loyalty on Atraxa", ok: true, kind: engine.ActionAdjustCounters,
			check: counterOn(f.atraxa, "loyalty", 2)},
		{in: "+2 on Atraxa", ok: true, kind: engine.ActionAdjustCounters,
			check: counterOn(f.atraxa, "loyalty", 2)},
		{in: "I'm the monarch", ok: true, kind: engine.ActionSetFlag,
			check: flagIs(1, "monarch")},
		{in: "Bob is the monarch", ok: true, kind: engine.ActionSetFlag,
			check: flagIs(3, "monarch")},
		{in: "take the initiative", ok: true, kind: engine.ActionSetFlag,
			check: flagIs(1, "initiative")},
		{in: "I have the city's blessing", ok: true, kind: engine.ActionSetFlag,
			check: flagIs(1, "city's blessing")},

		/* tokens */
		{in: "make two Treasures", ok: true, kind: engine.ActionCreateToken,
			check: token(2, "Treasure", []string{"Artifact"}, nil, nil)},
		{in: "make a Treasure", ok: true, kind: engine.ActionCreateToken,
			check: token(1, "Treasure", []string{"Artifact"}, nil, nil)},
		{in: "three 1/1 soldiers", ok: true, kind: engine.ActionCreateToken,
			check: token(3, "Soldier", []string{"Creature"}, ptr(1), ptr(1)), confHi: 0.9},
		{in: "create a 4/4 Beast with trample", ok: true, kind: engine.ActionCreateToken,
			check: token(1, "Beast", []string{"Creature"}, ptr(4), ptr(4))},
		{in: "make two clues", ok: true, kind: engine.ActionCreateToken,
			check: token(2, "Clue", []string{"Artifact"}, nil, nil)},
		{in: "make three goblin tokens", ok: true, kind: engine.ActionCreateToken,
			check: token(3, "Goblin", nil, nil, nil)},

		/* tap and untap */
		{in: "tap Sol Ring", ok: true, kind: engine.ActionTap,
			check: objectIs(f.solRing), confHi: 0.8}, // two Rings: tie-break
		{in: "untap everything", ok: true, kind: engine.ActionUntap,
			check: allIs(true)},
		{in: "untap", ok: true, kind: engine.ActionUntap,
			check: allIs(true), confHi: 0.9},
		{in: "tap Sol Ring for 2", ok: true, kind: engine.ActionTap,
			check: objectIs(f.solRing)},
		{in: "tap it", ok: true, kind: engine.ActionTap, confHi: 0.7},
		{in: "untap my Sol Ring", ok: true, kind: engine.ActionUntap,
			check: objectIs(f.solRing2)}, // the tapped one is the untappable one

		/* zone moves */
		{in: "sac this", ok: true, kind: engine.ActionMoveZone, confHi: 0.7,
			check: moveIs(engine.ZoneGraveyard, "sacrifice")},
		{in: "sacrifice the Soldier", ok: true, kind: engine.ActionMoveZone,
			check: and(objectIs(f.soldier), moveIs(engine.ZoneGraveyard, "sacrifice"))},
		{in: "exile it", ok: true, kind: engine.ActionMoveZone, confHi: 0.7,
			check: moveIs(engine.ZoneExile, "exile")},
		{in: "bounce it", ok: true, kind: engine.ActionMoveZone, confHi: 0.7,
			check: moveIs(engine.ZoneHand, "bounce")},
		{in: "destroy Atraxa", ok: true, kind: engine.ActionMoveZone,
			check: and(objectIs(f.atraxa), moveIs(engine.ZoneGraveyard, "destroy"))},
		{in: "draw", ok: true, kind: engine.ActionDraw,
			check: drawIs(1, 1), confLo: 1.0},
		{in: "draw two", ok: true, kind: engine.ActionDraw, check: drawIs(1, 2)},
		{in: "draw a card", ok: true, kind: engine.ActionDraw, check: drawIs(1, 1)},
		{in: "draw for turn", ok: true, kind: engine.ActionDraw, check: drawIs(1, 1)},
		{in: "Bob draws 3", ok: true, kind: engine.ActionDraw, actor: 3,
			check: drawIs(3, 3)},
		{in: "mill three", ok: true, kind: engine.ActionMill,
			check: millIs(1, 3), confLo: 1.0},
		{in: "mill Bob for two", ok: true, kind: engine.ActionMill, check: millIs(3, 2)},
		{in: "Sarah mills 5", ok: true, kind: engine.ActionMill, check: millIs(2, 5)},

		/* land and cast */
		{in: "play a Forest", ok: true, kind: engine.ActionPlayLand,
			check: cardIs("Forest"), confLo: 1.0},
		{in: "land: Forest", ok: true, kind: engine.ActionPlayLand, check: cardIs("Forest")},
		{in: "land drop: Forest", ok: true, kind: engine.ActionPlayLand, check: cardIs("Forest")},
		{in: "play a land", ok: true, kind: engine.ActionPlayLand, confLo: 1.0},
		{in: "drop an Island", ok: true, kind: engine.ActionPlayLand, check: cardIs("Island")},
		{in: "cast Rhystic Study", ok: true, kind: engine.ActionCast,
			check: cardIs("Rhystic Study"), confLo: 1.0},
		{in: "Rhystic", ok: true, kind: engine.ActionCast, check: cardIs("Rhystic Study"),
			confHi: 0.95},
		{in: "I'll play my Study", ok: true, kind: engine.ActionCast,
			check: cardIs("Rhystic Study")},
		{in: "play Command Tower", ok: true, kind: engine.ActionPlayLand,
			check: cardIs("Command Tower"), confLo: 1.0},
		{in: "playing Sol Ring", ok: true, kind: engine.ActionCast,
			check: cardIs("Sol Ring")},
		{in: "recast Krenko", ok: true, kind: engine.ActionCast,
			check: and(cardIs("Krenko, Mob Boss"), fromCommand())},
		{in: "cast blorptidious", ok: true, kind: engine.ActionCast,
			check: cardIs("blorptidious"), confHi: 0.3},
		{in: "in response, Rhystic", ok: true, kind: engine.ActionCast,
			check: cardIs("Rhystic Study")},
		{in: "Lightning Bolt Sarah", ok: true, kind: engine.ActionCast,
			check: and(cardIs("Lightning Bolt"), targetsSeat(2))},

		/* damage and life */
		{in: "-3", ok: true, kind: engine.ActionChangeLife,
			check: lifeDelta(1, -3), confLo: 1.0},
		{in: "+2", ok: true, kind: engine.ActionChangeLife, check: lifeDelta(1, 2)},
		{in: "-3 to Sarah", ok: true, kind: engine.ActionChangeLife, check: lifeDelta(2, -3)},
		{in: "Bob takes 3", ok: true, kind: engine.ActionChangeLife, check: lifeDelta(3, -3)},
		{in: "I take 3", ok: true, kind: engine.ActionChangeLife, check: lifeDelta(1, -3)},
		{in: "Sarah loses two life", ok: true, kind: engine.ActionChangeLife,
			check: lifeDelta(2, -2)},
		{in: "gain 5", ok: true, kind: engine.ActionChangeLife, check: lifeDelta(1, 5)},
		{in: "Dave gains 4", ok: true, kind: engine.ActionChangeLife, check: lifeDelta(4, 4)},
		{in: "I'm at 20", ok: true, kind: engine.ActionChangeLife, check: lifeTo(1, 20)},
		{in: "Bob is at 30", ok: true, kind: engine.ActionChangeLife, check: lifeTo(3, 30)},
		{in: "set Sarah to 25", ok: true, kind: engine.ActionChangeLife, check: lifeTo(2, 25)},
		{in: "set my life to 30", ok: true, kind: engine.ActionChangeLife, check: lifeTo(1, 30)},
		{in: "attack Sarah for six", ok: true, kind: engine.ActionDealDamage,
			check: damage(2, 6, true, 0, "")},
		{in: "swing 6 at Bob", ok: true, kind: engine.ActionDealDamage,
			check: damage(3, 6, true, 0, "")},
		{in: "swing at Bob for 6", ok: true, kind: engine.ActionDealDamage,
			check: damage(3, 6, true, 0, "")},
		{in: "Lightning Bolt Sarah for 3", ok: true, kind: engine.ActionDealDamage,
			check: damage(2, 3, false, 0, "Lightning Bolt")},
		{in: "Atraxa attacks Bob for 7", ok: true, kind: engine.ActionDealDamage,
			check: damage(3, 7, true, f.atraxa, "")},
		{in: "Bolt deals 3 to Dave", ok: true, kind: engine.ActionDealDamage,
			check: damage(4, 3, false, 0, "Lightning Bolt")},
		{in: "deals 3 damage to Sarah", ok: true, kind: engine.ActionDealDamage,
			check: damage(2, 3, false, 0, "")},
		{in: "3 damage to Sarah", ok: true, kind: engine.ActionDealDamage,
			check: damage(2, 3, false, 0, "")},
		{in: "hit Bob for 4", ok: true, kind: engine.ActionDealDamage,
			check: damage(3, 4, false, 0, ""), confHi: 0.9},

		/* negatives: no parse, never a guess */
		{in: "does this resolve?"},
		{in: "what resolves next?"},
		{in: "can I respond?"},
		{in: "why is this creature 7/7?"},
		{in: "what's my best out?"},
		{in: "3"},
		{in: "twenty"},
		{in: "sure"},
		{in: "sorry about that"},
		{in: "blorptidious"},
		{in: "play"},
		{in: "cast"},
		{in: "mill"},
		{in: "counter it"},
		{in: "attack Sarah with everything"}, // main step, not declare_attackers
		{in: "attack Sarah"},                 // no target creature said
		{in: "nice"},
		{in: "gg"},
		{in: ""},
		{in: "   "},
	}
}

/* ---------- assertion helpers ---------- */

func counterOn(obj int64, name string, delta int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.OnObject != obj || a.CounterName != name || a.Delta != delta {
			t.Errorf("counter = obj %d %s %+d, want obj %d %s %+d",
				a.OnObject, a.CounterName, a.Delta, obj, name, delta)
		}
	}
}

func seatCounter(seat int, name string, delta int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.TargetSeat != seat || a.CounterName != name || a.Delta != delta {
			t.Errorf("counter = seat %d %s %+d, want seat %d %s %+d",
				a.TargetSeat, a.CounterName, a.Delta, seat, name, delta)
		}
	}
}

func flagIs(seat int, flag string) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.TargetSeat != seat || a.Flag != flag || a.Value != "true" {
			t.Errorf("flag = seat %d %s=%q, want seat %d %s=true", a.TargetSeat, a.Flag, a.Value, seat, flag)
		}
	}
}

func token(count int, name string, types []string, power, toughness *int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.Count != count || a.Token == nil {
			t.Errorf("token action = count %d spec %+v, want count %d", a.Count, a.Token, count)
			return
		}
		if a.Token.Name != name {
			t.Errorf("token name = %q, want %q", a.Token.Name, name)
		}
		if types == nil {
			if len(a.Token.Types) != 0 {
				t.Errorf("token types = %v, want none declared", a.Token.Types)
			}
		} else if !reflect.DeepEqual(a.Token.Types, types) {
			t.Errorf("token types = %v, want %v", a.Token.Types, types)
		}
		if power != nil {
			if a.Token.Power == nil || *a.Token.Power != *power ||
				a.Token.Toughness == nil || *a.Token.Toughness != *toughness {
				t.Errorf("token PT = %+v/%+v, want %d/%d", a.Token.Power, a.Token.Toughness, *power, *toughness)
			}
		}
	}
}

func objectIs(id int64) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.Object != id {
			t.Errorf("object = %d, want %d", a.Object, id)
		}
	}
}

func allIs(v bool) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.All != v {
			t.Errorf("all = %v, want %v", a.All, v)
		}
	}
}

func moveIs(zone engine.Zone, cause string) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.ToZone != zone || a.Cause != cause {
			t.Errorf("move = %s/%s, want %s/%s", a.ToZone, a.Cause, zone, cause)
		}
	}
}

func and(fns ...func(*testing.T, engine.Action)) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		for _, fn := range fns {
			fn(t, a)
		}
	}
}

func drawIs(seat, count int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.Seat != seat || a.Count != count {
			t.Errorf("draw = seat %d count %d, want seat %d count %d", a.Seat, a.Count, seat, count)
		}
	}
}

func millIs(seat, count int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.TargetSeat != seat || a.Count != count {
			t.Errorf("mill = seat %d count %d, want seat %d count %d", a.TargetSeat, a.Count, seat, count)
		}
	}
}

func cardIs(card string) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.Card != card {
			t.Errorf("card = %q, want %q", a.Card, card)
		}
	}
}

func fromCommand() func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.FromZone != engine.ZoneCommand {
			t.Errorf("from zone = %q, want command", a.FromZone)
		}
	}
}

func targetsSeat(seat int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if len(a.Targets) != 1 || a.Targets[0].Seat != seat {
			t.Errorf("targets = %+v, want seat %d", a.Targets, seat)
		}
	}
}

func lifeDelta(seat, delta int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.TargetSeat != seat || a.Delta != delta {
			t.Errorf("life = seat %d %+d, want seat %d %+d", a.TargetSeat, a.Delta, seat, delta)
		}
	}
}

func lifeTo(seat, to int) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.TargetSeat != seat || a.To == nil || *a.To != to {
			t.Errorf("life = seat %d to %+v, want seat %d to %d", a.TargetSeat, a.To, seat, to)
		}
	}
}

func damage(seat, amount int, combat bool, srcObj int64, srcCard string) func(*testing.T, engine.Action) {
	return func(t *testing.T, a engine.Action) {
		if a.TargetSeat != seat || a.Amount != amount || a.CombatDmg != combat ||
			a.SourceObj != srcObj || a.SourceCard != srcCard {
			t.Errorf("damage = seat %d amount %d combat %v srcObj %d srcCard %q, want seat %d amount %d combat %v srcObj %d srcCard %q",
				a.TargetSeat, a.Amount, a.CombatDmg, a.SourceObj, a.SourceCard,
				seat, amount, combat, srcObj, srcCard)
		}
	}
}

/* ---------- combat declarations, against the step ---------- */

func TestCombatDeclarations(t *testing.T) {
	f := combatFixture(t)
	// "attack Sarah with everything": the Speaker's untapped creatures —
	// the Soldier — and nothing else (Sol Rings are not creatures).
	res := f.parse(1, "attack Sarah with everything")
	if !res.OK || res.Action.Kind != engine.ActionDeclareAttackers {
		t.Fatalf("with everything: OK %v kind %s", res.OK, res.Action.Kind)
	}
	want := []engine.AttackAssignment{{Object: f.soldier, TargetSeat: 2}}
	if !reflect.DeepEqual(res.Action.Attackers, want) {
		t.Errorf("attackers = %+v, want %+v", res.Action.Attackers, want)
	}
	// The single named form, and the leading-name form.
	for _, in := range []string{"attack Sarah with my Soldier", "Soldier attacks Sarah"} {
		res := f.parse(1, in)
		if !res.OK || res.Action.Kind != engine.ActionDeclareAttackers {
			t.Fatalf("%q: OK %v kind %s", in, res.OK, res.Action.Kind)
		}
		if !reflect.DeepEqual(res.Action.Attackers, want) {
			t.Errorf("%q: attackers = %+v, want %+v", in, res.Action.Attackers, want)
		}
	}
	// An opponent's creature is not mine to attack with.
	if res := f.parse(1, "attack Sarah with Atraxa"); res.OK {
		t.Errorf("attacking with an opponent's creature parsed: %+v", res.Action)
	}
}

func TestBlockDeclaration(t *testing.T) {
	f := blockFixture(t)
	res := f.parse(2, "Atraxa blocks the Soldier")
	if !res.OK || res.Action.Kind != engine.ActionDeclareBlockers {
		t.Fatalf("block: OK %v kind %s", res.OK, res.Action.Kind)
	}
	want := []engine.BlockAssignment{{Blocker: f.atraxa, Attackers: []int64{f.soldier}}}
	if !reflect.DeepEqual(res.Action.Blockers, want) {
		t.Errorf("blockers = %+v, want %+v", res.Action.Blockers, want)
	}
	// Blocking something that is not attacking is not a block.
	if res := f.parse(2, "Atraxa blocks the Sol Ring"); res.OK {
		t.Errorf("blocking a non-attacker parsed: %+v", res.Action)
	}
}

/* ---------- behaviour properties ---------- */

func TestDeterministic(t *testing.T) {
	f := newFixture(t)
	for _, in := range []string{"tap Sol Ring", "attack Sarah for six", "make two Treasures", "Rhystic"} {
		first := f.parse(1, in)
		for i := 0; i < 3; i++ {
			again := f.parse(1, in)
			if again.OK != first.OK {
				t.Fatalf("%q: OK flipped to %v", in, again.OK)
			}
			if !reflect.DeepEqual(again.Action, first.Action) {
				t.Fatalf("%q: parse not deterministic:\n%+v\n%+v", in, again.Action, first.Action)
			}
		}
	}
}

func TestNilNames(t *testing.T) {
	f := newFixture(t)
	p := func(in string) Result { return Parse(context.Background(), 1, in, f.st, nil) }
	// Structural shapes never needed the seam.
	for _, in := range []string{"pass", "-3", "draw two", "I'm the monarch", "play a Forest"} {
		if res := p(in); !res.OK {
			t.Errorf("%q with nil names: no parse", in)
		}
	}
	// Bare names and targeted casts refuse rather than guess.
	for _, in := range []string{"Rhystic", "Lightning Bolt Sarah", "play Command Tower"} {
		if res := p(in); res.OK {
			t.Errorf("%q with nil names: parsed %+v, want no-parse", in, res.Action)
		}
	}
	// An explicit cast of an unresolved name still parses, recorded as
	// spoken at the unresolved band.
	res := p("cast Rhystic Study")
	if !res.OK || res.Action.Card != "rhystic study" || res.Action.Confidence > ConfUnresolved {
		t.Errorf("cast with nil names = %+v, want spoken name at the unresolved band", res.Action)
	}
}

func TestNilState(t *testing.T) {
	names := withNames()
	// No game at all: the shapes that need no state still work.
	for _, in := range []string{"pass", "-3", "draw two", "Rhystic"} {
		if res := Parse(context.Background(), 1, in, nil, names); !res.OK {
			t.Errorf("%q with nil state: no parse", in)
		}
	}
	// Referents need a board; no board, no parse.
	if res := Parse(context.Background(), 1, "sac this", nil, names); res.OK {
		t.Errorf("sac this with nil state parsed: %+v", res.Action)
	}
}

func TestSeatPrefixAmbiguity(t *testing.T) {
	st := engine.NewState()
	apply(t, st, engine.Action{Kind: engine.ActionStartGame, Seats: []engine.SeatConfig{
		{Seat: 1, Name: "Collin"}, {Seat: 2, Name: "Bob"}, {Seat: 3, Name: "Bobby"},
	}})
	// "bo" answers to two seats: refuse. "bob" is exact: take it.
	if res := Parse(context.Background(), 1, "bo takes 3", st, nil); res.OK {
		t.Errorf("ambiguous seat prefix parsed: %+v", res.Action)
	}
	res := Parse(context.Background(), 1, "bob takes 3", st, nil)
	if !res.OK || res.Action.TargetSeat != 2 {
		t.Errorf("exact seat name = %+v, want seat 2", res.Action)
	}
}

// TestEngineRoundTrip is the property the whole package exists for: a
// parsed action applies. Every utterance here goes through Parse and
// then engine.Apply on the live fixture, and every one must land.
func TestEngineRoundTrip(t *testing.T) {
	f := newFixture(t)
	st := f.st
	// A fresh state for actions the fixture's own history would reject
	// (the land drop is spent, the Sol Rings are tapped already, and so
	// on), driven exactly as a live game would be.
	fresh := engine.NewState()
	apply(t, fresh, engine.Action{Kind: engine.ActionStartGame, Seats: []engine.SeatConfig{
		{Seat: 1, Name: "Collin"}, {Seat: 2, Name: "Sarah"}, {Seat: 3, Name: "Bob"},
	}})
	apply(t, fresh, engine.Action{Kind: engine.ActionAdvance})
	apply(t, fresh, engine.Action{Kind: engine.ActionAdvance}) // Collin's main
	apply(t, fresh, engine.Action{Kind: engine.ActionCreateToken, Seat: 1,
		Token: &engine.TokenSpec{Name: "Sol Ring", Types: []string{"Artifact"}}})
	p := func(seat int, in string) engine.Action {
		res := Parse(context.Background(), seat, in, fresh, defaultNames)
		if !res.OK {
			t.Fatalf("%q: no parse", in)
		}
		return res.Action
	}
	for _, in := range []string{
		"play a Forest", "cast Cultivate", "tap Sol Ring", "make two Treasures",
		"-3", "Sarah takes 3", "I'm the monarch", "draw two", "mill three",
		"+1/+1 counter on Sol Ring", "two energy", "pass",
	} {
		a := p(1, in)
		evs, err := engine.Apply(fresh, a)
		if err != nil {
			t.Fatalf("%q → %s: apply: %v", in, a.Kind, err)
		}
		if len(evs) == 0 {
			t.Fatalf("%q → %s: applied with zero events", in, a.Kind)
		}
		fresh.FoldInto(evs)
	}
	// The stack from "cast Cultivate" is still open; "resolves" passes
	// around the table until it lands.
	for seat := 2; seat <= 3; seat++ {
		a := p(seat, "resolves")
		evs, err := engine.Apply(fresh, a)
		if err != nil {
			t.Fatalf("resolve pass seat %d: %v", seat, err)
		}
		fresh.FoldInto(evs)
	}
	res := p(1, "resolves")
	evs, err := engine.Apply(fresh, res)
	if err != nil {
		t.Fatalf("final resolve: %v", err)
	}
	fresh.FoldInto(evs)
	if len(fresh.Stack) != 0 {
		t.Errorf("stack not empty after resolving: %+v", fresh.Stack)
	}
	_ = st
	_ = f
}
