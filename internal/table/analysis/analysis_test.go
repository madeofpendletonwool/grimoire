package analysis_test

// The post-game coach's deterministic half (MAD-339): pure-function
// tests over a log driven through the engine's own reducer — the same
// rows the store would persist, ordinals and batches stamped the way
// the single writer stamps them. What is asserted is what the model
// gets: turns, draws, the final-turn resource picture, damage
// attribution, and the missed-trigger diff of today's registry against
// what actually fired.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/madeofpendletonwool/grimoire/internal/table/analysis"
	"github.com/madeofpendletonwool/grimoire/internal/table/engine"
)

// driver folds actions into a log the store would recognize: contiguous
// ordinals, one batch per Submit.
type driver struct {
	evs []engine.Event
	st  *engine.State
	ord int64
}

func newDriver(t *testing.T, seats ...engine.SeatConfig) *driver {
	t.Helper()
	if len(seats) == 0 {
		seats = []engine.SeatConfig{
			{Seat: 1, Name: "Collin", StartingLife: 40},
			{Seat: 2, Name: "Bob", StartingLife: 40},
			{Seat: 3, Name: "Alice", StartingLife: 40},
		}
	}
	d := &driver{st: engine.NewState()}
	d.do(t, engine.Action{Kind: engine.ActionStartGame, Seats: seats}, nil)
	return d
}

func (d *driver) do(t *testing.T, a engine.Action, reg engine.TriggerRegistry) []engine.Event {
	t.Helper()
	evs, err := engine.ApplyWithTriggers(d.st, a, reg)
	if err != nil {
		t.Fatalf("apply %s: %v", a.Kind, err)
	}
	batch := fmt.Sprintf("b%d", d.ord+1)
	for i := range evs {
		d.ord++
		evs[i].Ord = d.ord
		evs[i].Batch = batch
	}
	d.st.FoldInto(evs)
	d.evs = append(d.evs, evs...)
	return evs
}

// toMain walks the current turn seat into its precombat main — where
// lands and casts are legal.
func (d *driver) toMain(t *testing.T) {
	t.Helper()
	for i := 0; i < 20 && !(d.st.Phase == "precombat_main" && d.st.Step == "main"); i++ {
		d.do(t, engine.Action{Kind: engine.ActionAdvance, Seat: d.st.TurnSeat}, nil)
	}
	if d.st.Phase != "precombat_main" || d.st.Step != "main" {
		t.Fatalf("toMain: stuck at %s/%s", d.st.Phase, d.st.Step)
	}
}

// resolveStack passes priority in rotation until the stack drains.
func (d *driver) resolveStack(t *testing.T) {
	t.Helper()
	for i := 0; i < 16 && len(d.st.Stack) > 0; i++ {
		if d.st.PrioritySeat == 0 {
			d.do(t, engine.Action{Kind: engine.ActionAdvance, Seat: d.st.TurnSeat}, nil)
			continue
		}
		d.do(t, engine.Action{Kind: engine.ActionPassPriority, Seat: d.st.PrioritySeat}, nil)
	}
	if len(d.st.Stack) > 0 {
		t.Fatal("resolveStack: the stack never drained")
	}
}

// passTurn resolves anything pending and advances until the next seat
// sits in its precombat main.
func (d *driver) passTurn(t *testing.T) {
	t.Helper()
	d.resolveStack(t)
	next := d.st.TurnSeat%3 + 1
	for i := 0; i < 24 && d.st.TurnSeat != next; i++ {
		d.do(t, engine.Action{Kind: engine.ActionAdvance, Seat: d.st.TurnSeat}, nil)
	}
	if d.st.TurnSeat != next {
		t.Fatalf("passTurn: still seat %d's turn", d.st.TurnSeat)
	}
	d.toMain(t)
}

func land(name string) *engine.BaseChars {
	return &engine.BaseChars{Name: name, Types: []string{"Land"}}
}

func TestSummarizeFinalTurnResources(t *testing.T) {
	d := newDriver(t)
	d.toMain(t)
	// Collin's first turn: a forest and a draw with spoken identities.
	d.do(t, engine.Action{Kind: engine.ActionPlayLand, Seat: 1, Card: "Forest", Base: land("Forest")}, nil)
	var firstForest int64
	for _, e := range d.evs {
		if e.Kind == engine.EventLandPlayed && e.Controller == 1 {
			firstForest = e.Object
			break
		}
	}
	d.do(t, engine.Action{Kind: engine.ActionDraw, Seat: 1, Count: 2, Cards: []string{"Cultivate", "Island"}}, nil)
	d.passTurn(t)
	d.passTurn(t)
	d.passTurn(t)

	// Collin's second turn: a second forest, the first tapped — one
	// unspent source when the turn ends.
	d.do(t, engine.Action{Kind: engine.ActionPlayLand, Seat: 1, Card: "Forest", Base: land("Forest")}, nil)
	d.do(t, engine.Action{Kind: engine.ActionTap, Seat: 1, Object: firstForest}, nil)
	d.passTurn(t)

	sum := analysis.Summarize(d.evs, nil, 1)
	f := sum.Seat
	if f.Name != "Collin" || f.Seat != 1 {
		t.Fatalf("seat line = %+v", f)
	}
	if f.TurnsPlayed != 2 {
		t.Fatalf("turns played = %d, want 2", f.TurnsPlayed)
	}
	if f.Drawn != 2 {
		t.Fatalf("drawn = %d, want 2", f.Drawn)
	}
	// Drew 2, then the second forest left the hand it was played from:
	// one card in hand at the end, and the fold knows it.
	if !f.HandKnownAtEnd || f.HandAtEnd != 1 {
		t.Fatalf("hand at end = known %v n %d, want known 1", f.HandKnownAtEnd, f.HandAtEnd)
	}
	if f.LandsPlayed != 2 {
		t.Fatalf("lands played = %d, want 2", f.LandsPlayed)
	}
	// Seat 1's turns are the game's T1 and T4 — the snapshot anchors
	// the global turn number, the only one the log knows.
	if f.FinalTurn.Turn != 4 || !f.FinalTurn.EndedTurn {
		t.Fatalf("final turn = %+v", f.FinalTurn)
	}
	// Two forests, the first tapped on turn 1: the second is the one
	// unspent source at the end of the final turn.
	if !f.FinalTurn.LandsKnown || f.FinalTurn.Lands != 1 {
		t.Fatalf("unspent sources = known %v n %d, want 1", f.FinalTurn.LandsKnown, f.FinalTurn.Lands)
	}
	if f.FinalTurn.LandDrops != 1 {
		t.Fatalf("final-turn land drops = %d, want 1", f.FinalTurn.LandDrops)
	}
	if len(f.MissedTriggers) != 0 {
		t.Fatalf("no registry, no misses: %+v", f.MissedTriggers)
	}
	if sum.Game.Turns < 4 || sum.Game.Status != "active" {
		t.Fatalf("game frame = %+v", sum.Game)
	}
}

func hasLand(o *engine.Object) bool {
	if o == nil {
		return false
	}
	for _, ty := range o.Base.Types {
		if strings.EqualFold(ty, "Land") {
			return true
		}
	}
	return false
}

func TestSummarizeDamageAndElimination(t *testing.T) {
	d := newDriver(t)
	d.toMain(t)
	// Collin's creature hits Bob for 6, then Bob and Alice fall to
	// zero life — the last-standing ending.
	bear := d.do(t, engine.Action{Kind: engine.ActionCreateToken, Seat: 1, Token: &engine.TokenSpec{
		Name: "Grizzly Bears", Types: []string{"Creature"}, Power: ptr(2), Toughness: ptr(2)}}, nil)
	var bearID int64
	for _, e := range bear {
		bearID = e.Object
	}
	d.do(t, engine.Action{Kind: engine.ActionDealDamage, Seat: 1, TargetSeat: 2,
		Amount: 6, SourceObj: bearID}, nil)
	d.do(t, engine.Action{Kind: engine.ActionChangeLife, Seat: 2, TargetSeat: 2, To: ptr(0)}, nil)
	d.do(t, engine.Action{Kind: engine.ActionChangeLife, Seat: 3, TargetSeat: 3, To: ptr(0)}, nil)

	sum := analysis.Summarize(d.evs, nil, 1)
	if sum.Seat.DamageDealt != 6 {
		t.Fatalf("damage dealt = %d, want 6", sum.Seat.DamageDealt)
	}
	if sum.Game.Status != "finished" || sum.Game.Reason != "last_standing" {
		t.Fatalf("game = %+v", sum.Game)
	}
	bySeat := map[int]analysis.SeatLine{}
	for _, s := range sum.Game.Seats {
		bySeat[s.Seat] = s
	}
	if bySeat[2].Alive || bySeat[2].LeftCause != "sba_zero_life" {
		t.Fatalf("bob's line = %+v", bySeat[2])
	}
	if !bySeat[1].Alive {
		t.Fatalf("collin's line = %+v", bySeat[1])
	}

	bob := analysis.Summarize(d.evs, nil, 2)
	if bob.Seat.DamageTaken != 6 || bob.Seat.LifeLost != 6 {
		t.Fatalf("bob damage = taken %d lost %d", bob.Seat.DamageTaken, bob.Seat.LifeLost)
	}
	if !bob.Seat.Eliminated || bob.Seat.LeftCause != "sba_zero_life" {
		t.Fatalf("bob elimination = %+v", bob.Seat)
	}
}

func TestMissedTriggerDiff(t *testing.T) {
	// The registry knows Avenger's landfall. The log was played before
	// it was registered: the land plays fired nothing, and today's
	// replay of the log says so.
	reg := engine.NewTriggerRegistry([]engine.TriggerSpec{
		{Card: "Avenger of Zendikar", Event: engine.TriggerLandPlayed, Effect: "create a 0/1 Plant"},
	})
	avenger := &engine.BaseChars{Name: "Avenger of Zendikar", Types: []string{"Creature"},
		Power: ptr(2), Toughness: ptr(4)}

	d := newDriver(t)
	d.toMain(t)
	d.do(t, engine.Action{Kind: engine.ActionCast, Seat: 1, Card: "Avenger of Zendikar", Base: avenger}, nil)
	d.passTurn(t)
	d.passTurn(t)
	d.passTurn(t)
	d.do(t, engine.Action{Kind: engine.ActionPlayLand, Seat: 1, Card: "Forest", Base: land("Forest")}, nil)

	sum := analysis.Summarize(d.evs, reg, 1)
	if len(sum.Seat.MissedTriggers) != 1 {
		t.Fatalf("missed = %+v, want one", sum.Seat.MissedTriggers)
	}
	mt := sum.Seat.MissedTriggers[0]
	if mt.Card != "Avenger of Zendikar" || mt.Effect != "create a 0/1 Plant" {
		t.Fatalf("missed row = %+v", mt)
	}
	if mt.At == 0 || mt.Happening == "" {
		t.Fatalf("missed row carries no happening: %+v", mt)
	}

	// The same registry, a log that had it: the land play fires, the
	// diff is empty. The fired TRIGGER_FIRED row rides the same batch.
	d2 := newDriver(t)
	d2.toMain(t)
	d2.do(t, engine.Action{Kind: engine.ActionCast, Seat: 1, Card: "Avenger of Zendikar", Base: avenger}, reg)
	d2.passTurn(t)
	d2.passTurn(t)
	d2.passTurn(t)
	d2.do(t, engine.Action{Kind: engine.ActionPlayLand, Seat: 1, Card: "Forest", Base: land("Forest")}, reg)

	fired := false
	for _, e := range d2.evs {
		if e.Kind == engine.EventTriggerFired && e.Card == "Avenger of Zendikar" {
			fired = true
		}
	}
	if !fired {
		t.Fatal("the registry never fired — the fixture is vacuous")
	}
	sum2 := analysis.Summarize(d2.evs, reg, 1)
	if len(sum2.Seat.MissedTriggers) != 0 {
		t.Fatalf("fired triggers counted missed: %+v", sum2.Seat.MissedTriggers)
	}
}

func TestDigestAndPromptCarryFactsAndOrdinals(t *testing.T) {
	d := newDriver(t)
	d.toMain(t)
	d.do(t, engine.Action{Kind: engine.ActionPlayLand, Seat: 1, Card: "Forest", Base: land("Forest")}, nil)
	d.do(t, engine.Action{Kind: engine.ActionCast, Seat: 1, Card: "Sol Ring",
		Base: &engine.BaseChars{Name: "Sol Ring", Types: []string{"Artifact"}}}, nil)

	sum := analysis.Summarize(d.evs, nil, 1)
	digest := analysis.Digest(d.evs, 1)
	if digest == "" {
		t.Fatal("empty digest")
	}
	for _, want := range []string{"plays Forest (#", "casts Sol Ring (#"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest missing %q:\n%s", want, digest)
		}
	}
	system, user := analysis.Prompt(sum, digest)
	if system == "" || user == "" {
		t.Fatal("empty prompt halves")
	}
	if !strings.Contains(user, "SEAT: Collin") || !strings.Contains(user, digest) {
		t.Fatalf("user half does not carry facts + digest:\n%s", user)
	}
	if !strings.Contains(system, "Never invent state") {
		t.Fatalf("system half lost the contract:\n%s", system)
	}
	// Deterministic end to end: same log in, same bytes out.
	if analysis.RenderFacts(sum) != analysis.RenderFacts(analysis.Summarize(d.evs, nil, 1)) {
		t.Fatal("RenderFacts is not deterministic")
	}
}

func ptr(n int) *int { return &n }
