package engine

import (
	"strings"
	"testing"
)

// Provenance answers (MAD-334): every trace is a pure function over
// stored rows. The tests drive real reducer output — collecting the
// events into a log the store would have persisted, with the ordinals
// the writer assigns — so the traces are exercised against exactly the
// rows a game's log holds.

// logGame drives actions and returns the persisted-shaped log: one
// contiguous ordinal per row, causes on every row — the shape
// mtg_events stores and DeathTrace/TurnSlice read.
type logGame struct {
	t   *testing.T
	s   *State
	log []Event
}

func newLogGame(t *testing.T) *logGame {
	t.Helper()
	g := &logGame{t: t, s: NewState()}
	g.act(Action{Kind: ActionStartGame, Seats: threeSeats(), Source: "system"})
	return g
}

func (g *logGame) act(a Action) {
	g.t.Helper()
	evs, err := Apply(g.s, a)
	if err != nil {
		g.t.Fatalf("apply %s: %v", a.Kind, err)
	}
	for _, e := range evs {
		g.log = append(g.log, e)
	}
	g.s.FoldInto(evs)
}

// persist shape: ordinals contiguous from 1, system default source.
func (g *logGame) finalize() []Event {
	g.t.Helper()
	out := make([]Event, len(g.log))
	for i := range g.log {
		e := g.log[i]
		e.Ord = int64(i + 1)
		if e.Source == "" {
			e.Source = "system"
		}
		out[i] = e
	}
	return out
}

func (g *logGame) summon(seat int, name string, power, toughness int) int64 {
	g.t.Helper()
	id := g.s.NextObject
	g.act(Action{Kind: ActionCreateToken, Seat: seat, Source: "tap", Token: &TokenSpec{
		Name: name, Types: []string{"Creature"}, Power: &power, Toughness: &toughness}})
	return id
}

// TestCharTraceSpellsEveryComputedCharacteristic walks the model doc's
// worked example — a 2/2 with an Anthem out, a +1/+1 counter and a
// Giant Growth — plus one modifier in each of the non-P/T layers, and
// asserts every computed characteristic has a row: the P/T stack, the
// control change, the type and the granted keyword.
func TestCharTraceSpellsEveryComputedCharacteristic(t *testing.T) {
	g := newLogGame(t)
	g.act(Action{Kind: ActionAdvance, Seat: 1, Source: "tap"})
	g.act(Action{Kind: ActionAdvance, Seat: 1, Source: "tap"})
	anthem := g.s.NextObject
	g.act(Action{Kind: ActionPlayLand, Seat: 1, Source: "tap", Card: "Glorious Anthem",
		Base: &BaseChars{Name: "Glorious Anthem", Types: []string{"Enchantment"}}})
	bear := g.summon(1, "Grizzly Bears", 2, 2)
	g.act(Action{Kind: ActionAddModifier, Seat: 1, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: WhileSourcePresent, SourceObj: anthem,
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	g.act(Action{Kind: ActionAdjustCounters, Seat: 1, Source: "tap", OnObject: bear, CounterName: "+1/+1", Delta: 1})
	g.act(Action{Kind: ActionAddModifier, Seat: 1, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Giant Growth",
		Delta: Delta{Power: intPtr(3), Toughness: intPtr(3)}}})
	g.act(Action{Kind: ActionAddModifier, Seat: 2, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerControl, Duration: UntilEOT, SourceCard: "Act of Treason",
		Delta: Delta{Controller: intPtr(2)}}})
	g.act(Action{Kind: ActionAddModifier, Seat: 1, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerType, Duration: WhileSourcePresent, SourceObj: anthem,
		Delta: Delta{AddTypes: []string{"Goblin"}}}})
	g.act(Action{Kind: ActionAddModifier, Seat: 1, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerAbility, Duration: UntilEOT, SourceCard: "Flight",
		Delta: Delta{AddKeywords: []string{"Flying"}}}})

	trace := g.s.CharTrace(bear)
	if trace == nil {
		t.Fatal("no trace for a live object")
	}
	if trace.Name != "Grizzly Bears" {
		t.Fatalf("name = %q", trace.Name)
	}
	// The P/T stack: base, anthem, counter, growth, total 7/7.
	var got strings.Builder
	for _, l := range trace.PT {
		got.WriteString(l.Kind + ":" + l.Label + " ")
	}
	pt := got.String()
	for _, want := range []string{"base:Grizzly Bears", "modifier:Glorious Anthem",
		"counter:+1/+1 counter", "modifier:Giant Growth", "total:total"} {
		if !strings.Contains(pt, want) {
			t.Fatalf("PT trace %q missing %q", pt, want)
		}
	}
	total := trace.PT[len(trace.PT)-1]
	if total.Kind != "total" || total.Power != 7 || total.Toughness != 7 {
		t.Fatalf("total = %+v", total)
	}
	// Every other computed characteristic has its row too.
	if len(trace.Changes) != 3 {
		t.Fatalf("changes = %+v, want control, type and ability rows", trace.Changes)
	}
	joined := make([]string, len(trace.Changes))
	for i, c := range trace.Changes {
		joined[i] = c.Layer + "|" + c.Change
	}
	all := strings.Join(joined, " ; ")
	for _, want := range []string{"control|controller → Bob", "type|gains Goblin", "ability|gains Flying"} {
		if !strings.Contains(all, want) {
			t.Fatalf("changes %q missing %q", all, want)
		}
	}
	// The trace is the same SELECT refolded from the log alone.
	refolded := Fold(g.finalize())
	if again := refolded.CharTrace(bear); again == nil || len(again.PT) != len(trace.PT) || len(again.Changes) != len(trace.Changes) {
		t.Fatalf("refolded trace = %+v", again)
	}
	// An unknown object answers nil, not an empty fiction.
	if s := g.s.CharTrace(999); s != nil {
		t.Fatalf("trace for absent object = %+v", s)
	}
}

// TestTraceSourceLeftTheBattlefield is the graded case: a
// while_source_present modifier whose source has since left. Phased
// out, the source suspends its effect — the computed board stops
// counting it and the trace spells the row as history, not arithmetic.
func TestTraceSourceLeftTheBattlefield(t *testing.T) {
	g := newLogGame(t)
	anthem := g.s.NextObject
	g.act(Action{Kind: ActionCreateToken, Seat: 1, Source: "tap", Token: &TokenSpec{
		Name: "Glorious Anthem", Types: []string{"Enchantment"}}})
	bear := g.summon(1, "Grizzly Bears", 2, 2)
	g.act(Action{Kind: ActionAddModifier, Seat: 1, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: WhileSourcePresent, SourceObj: anthem,
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	if c := g.s.Characteristics(bear); *c.Power != 3 {
		t.Fatalf("powered bear = %d/%d", *c.Power, *c.Toughness)
	}
	g.act(Action{Kind: ActionSetPhased, Seat: 1, Source: "tap", Object: anthem, Phased: true})

	if c := g.s.Characteristics(bear); *c.Power != 2 || *c.Toughness != 2 {
		t.Fatalf("phased anthem still counting: %d/%d", *c.Power, *c.Toughness)
	}
	trace := g.s.CharTrace(bear)
	var anthemRow *PTLine
	for i, l := range trace.PT {
		if l.Label == "Glorious Anthem" {
			anthemRow = &trace.PT[i]
		}
	}
	if anthemRow == nil {
		t.Fatalf("trace dropped the anthem row: %+v", trace.PT)
	}
	if !anthemRow.SourceGone || anthemRow.Note == "" {
		t.Fatalf("anthem row = %+v, want source_gone with its note", anthemRow)
	}
	total := trace.PT[len(trace.PT)-1]
	if total.Power != 2 || total.Toughness != 2 {
		t.Fatalf("total = %d/%d, the gone row must not count", total.Power, total.Toughness)
	}

	// Leaving the battlefield entirely asserts MODIFIER_REMOVED — the
	// row the log should show — so the trace on a live object retires
	// the row instead of flagging it.
	g.act(Action{Kind: ActionSetPhased, Seat: 1, Source: "tap", Object: anthem})
	g.act(Action{Kind: ActionMoveZone, Seat: 1, Source: "tap", Object: anthem,
		ToZone: ZoneExile, Cause: "exile"})
	trace = g.s.CharTrace(bear)
	for _, l := range trace.PT {
		if l.Label == "Glorious Anthem" {
			t.Fatalf("exiled anthem still traced: %+v", trace.PT)
		}
	}
	if total := trace.PT[len(trace.PT)-1]; total.Power != 2 {
		t.Fatalf("total after exile = %+v", total)
	}
}

// TestPlayerLeavingEndsSourcedEffects pins the CR 800.4a fix the trace
// surfaced: a conceding player's permanents leave the game, and the
// while_source_present effects they were the source of end — asserted
// as MODIFIER_REMOVED rows, and gone from any refold.
func TestPlayerLeavingEndsSourcedEffects(t *testing.T) {
	g := newLogGame(t)
	anthem := g.s.NextObject
	g.act(Action{Kind: ActionCreateToken, Seat: 2, Source: "tap", Token: &TokenSpec{
		Name: "Glorious Anthem", Types: []string{"Enchantment"}}})
	bear := g.summon(1, "Grizzly Bears", 2, 2)
	g.act(Action{Kind: ActionAddModifier, Seat: 2, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: WhileSourcePresent, SourceObj: anthem,
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	// A Giant Growth from the leaving player's own hand outlives them
	// until end of turn — only source-bound effects end (CR 800.4a).
	g.act(Action{Kind: ActionAddModifier, Seat: 2, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Giant Growth",
		Delta: Delta{Power: intPtr(3), Toughness: intPtr(3)}}})

	g.act(Action{Kind: ActionConcede, Seat: 2, Source: "tap"})
	if c := g.s.Characteristics(bear); *c.Power != 5 || *c.Toughness != 5 {
		t.Fatalf("bear = %d/%d, want 5/5 — growth survives the concession, anthem does not",
			*c.Power, *c.Toughness)
	}
	trace := g.s.CharTrace(bear)
	for _, l := range trace.PT {
		if l.Label == "Glorious Anthem" {
			t.Fatalf("conceded anthem still traced: %+v", trace.PT)
		}
	}
	// The refold agrees without the asserted rows: the fold's safety
	// net drops what the sweep asserted.
	refolded := Fold(g.finalize())
	if c := refolded.Characteristics(bear); *c.Power != 5 {
		t.Fatalf("refolded bear = %d/%d", *c.Power, *c.Toughness)
	}
}

// TestDeathTraceZeroToughness walks a death backwards: a 2/2 with a
// +1/+1 counter eats a -3/-3, the sweep asserts DIED, and the report
// shows the cause with its rule, the exact modifier stack in force the
// moment before, and the table action that set it up.
func TestDeathTraceZeroToughness(t *testing.T) {
	g := newLogGame(t)
	bear := g.summon(1, "Grizzly Bears", 2, 2)
	g.act(Action{Kind: ActionAdjustCounters, Seat: 1, Source: "tap", OnObject: bear, CounterName: "+1/+1", Delta: 1})
	logBefore := len(g.log)
	// Last Gasp: the -3/-3 that puts computed toughness at 0.
	g.act(Action{Kind: ActionAddModifier, Seat: 2, Source: "tap", Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Last Gasp",
		Delta: Delta{Power: intPtr(-3), Toughness: intPtr(-3)}}})
	// Ordinals are contiguous from 1 in the persisted shape, so the
	// DIED row's ordinal is its index plus one.
	var diedOrd int64
	for i, e := range g.log[logBefore:] {
		if e.Kind == EventDied {
			diedOrd = int64(logBefore + i + 1)
		}
	}
	if diedOrd == 0 {
		t.Fatalf("no DIED row: %+v", g.log[logBefore:])
	}
	// Assign the persisted shape before tracing.
	log := g.finalize()
	if log[diedOrd-1].Kind != EventDied {
		t.Fatalf("ordinal %d is %s", diedOrd, log[diedOrd-1].Kind)
	}
	rep, err := DeathTrace(log, diedOrd)
	if err != nil {
		t.Fatalf("death trace: %v", err)
	}
	if rep.Name != "Grizzly Bears" || rep.Cause != causeZeroToughness {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(rep.CauseNote, "704.5f") {
		t.Fatalf("cause note = %q", rep.CauseNote)
	}
	// The stack in force the moment before: base 2/2, the counter, and
	// the -3/-3 — totaling the 0 the check read.
	total := rep.PT[len(rep.PT)-1]
	if total.Kind != "total" || total.Toughness != 0 {
		t.Fatalf("death-time total = %+v, want 0 toughness", total)
	}
	labels := make([]string, len(rep.PT))
	for i, l := range rep.PT {
		labels[i] = l.Label
	}
	joined := strings.Join(labels, "|")
	for _, want := range []string{"Grizzly Bears", "+1/+1 counter", "Last Gasp"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("death PT %q missing %q", joined, want)
		}
	}
	// The trigger is the table's act, not the sweep's own rows.
	if rep.TriggeredBy == nil || rep.TriggeredBy.Kind != EventModifierAdded {
		t.Fatalf("triggered_by = %+v, want the Last Gasp row", rep.TriggeredBy)
	}
}

// TestDeathTraceLethalDamageWithSource covers the damage half: lethal
// marked damage with the source named, and the deathtouch exception a
// 1/1 deathtouch blocker makes real.
func TestDeathTraceLethalDamageWithSource(t *testing.T) {
	g := newLogGame(t)
	bear := g.summon(1, "Grizzly Bears", 2, 2)
	asp := g.summon(2, "King Cheetah", 1, 1)
	g.act(Action{Kind: ActionAddModifier, Seat: 2, Source: "tap", Object: asp, Modifier: &Modifier{
		Layer: LayerAbility, Duration: PermanentDuration, SourceCard: "Deathtouch",
		Delta: Delta{AddKeywords: []string{"Deathtouch"}}}})
	logBefore := len(g.log)
	g.act(Action{Kind: ActionDealDamage, Seat: 2, Source: "tap", SourceObj: asp,
		TargetObject: bear, Amount: 1})
	var diedOrd int64
	for i, e := range g.log[logBefore:] {
		if e.Kind == EventDied {
			diedOrd = int64(logBefore + i + 1)
		}
	}
	if diedOrd == 0 {
		t.Fatalf("no DIED row in %+v", g.log[logBefore:])
	}
	log := g.finalize()
	rep, err := DeathTrace(log, diedOrd)
	if err != nil {
		t.Fatalf("death trace: %v", err)
	}
	if rep.Cause != causeLethalDamage || !strings.Contains(rep.CauseNote, "702.2c") {
		t.Fatalf("cause = %q note = %q", rep.Cause, rep.CauseNote)
	}
	if len(rep.Damage) != 1 || rep.Damage[0].Source != "King Cheetah" || !rep.Damage[0].Deathtouch {
		t.Fatalf("damage lines = %+v", rep.Damage)
	}
	if rep.DamageTotal != 1 || rep.Toughness != 2 {
		t.Fatalf("damage %d vs toughness %d", rep.DamageTotal, rep.Toughness)
	}
	if rep.TriggeredBy == nil || rep.TriggeredBy.Kind != EventDamageMarked {
		t.Fatalf("triggered_by = %+v", rep.TriggeredBy)
	}
}

// TestDeathTraceRejectsNonDeathRows: the walk is anchored to a DIED
// row — a missing ordinal and a non-death ordinal are both rejected,
// because a provenance answer must never be invented for a row that
// did not happen.
func TestDeathTraceRejectsNonDeathRows(t *testing.T) {
	g := newLogGame(t)
	bear := g.summon(1, "Grizzly Bears", 2, 2)
	g.act(Action{Kind: ActionAdjustCounters, Seat: 1, Source: "tap", OnObject: bear, CounterName: "+1/+1", Delta: 1})
	log := g.finalize()
	if _, err := DeathTrace(log, 999); err == nil {
		t.Fatal("absent ordinal traced")
	}
	if _, err := DeathTrace(log, 1); err == nil {
		t.Fatal("GAME_STARTED traced as a death")
	}
}

// TestTurnSlice covers "what happened on turn 5?": one turn's rows run
// from its TURN_STARTED anchor to the next one's, and an absent turn is
// empty rather than invented.
func TestTurnSlice(t *testing.T) {
	g := newLogGame(t)
	g.summon(1, "Grizzly Bears", 2, 2)
	// Walk into turn 2: the engine's turn structure ends turn 1 when
	// ADVANCE runs off cleanup.
	for i := 0; i < len(turnStructure)+2 && g.s.Turn == 1; i++ {
		g.act(Action{Kind: ActionAdvance, Seat: 1, Source: "tap"})
	}
	if g.s.Turn != 2 {
		t.Fatalf("never reached turn 2 (at turn %d)", g.s.Turn)
	}
	g.act(Action{Kind: ActionDraw, Seat: 2, Source: "tap", Count: 1})
	log := g.finalize()

	one := TurnSlice(log, 1)
	if len(one) == 0 || one[0].Kind != EventTurnStarted || one[0].Turn != 1 {
		t.Fatalf("turn 1 slice starts %+v", one[0])
	}
	for i, e := range one {
		if e.Kind == EventTurnStarted && i > 0 {
			t.Fatalf("turn 1 slice leaked a second anchor: %+v", e)
		}
	}
	// Turn 1 created the bear; turn 2 drew.
	var sawSummon, sawDraw bool
	for _, e := range one {
		if e.Kind == EventObjectCreated {
			sawSummon = true
		}
	}
	for _, e := range TurnSlice(log, 2) {
		if e.Kind == EventCardDrawn {
			sawDraw = true
		}
	}
	if !sawSummon || !sawDraw {
		t.Fatalf("slices misplaced events: summon %v draw %v", sawSummon, sawDraw)
	}
	if got := TurnSlice(log, 9); got != nil {
		t.Fatalf("turn 9 slice = %d rows, want none", len(got))
	}
	// Slices are exhaustive and non-overlapping: every row after the
	// first anchor is in exactly one turn's slice.
	total := len(one) + len(TurnSlice(log, 2))
	anchors := 0
	for _, e := range log {
		if e.Kind == EventTurnStarted {
			anchors++
		}
	}
	if total != len(log)-1 { // everything except GAME_STARTED
		t.Fatalf("slices cover %d rows of %d (anchors %d)", total, len(log), anchors)
	}
}
