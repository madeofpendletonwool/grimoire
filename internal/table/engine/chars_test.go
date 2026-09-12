package engine

import (
	"strings"
	"testing"
)

// The model doc's worked example, in rows: a 2/2 with a Glorious Anthem
// out, a +1/+1 counter, and a Giant Growth is a 7/7 — and the answer to
// "why?" is this trace, not a model call.
func TestPTTraceSevenBySeven(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	anthem := land(t, s, 1, "Glorious Anthem")
	bear := summon(t, s, 1, "Grizzly Bears", 2, 2)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: WhileSourcePresent, SourceObj: anthem,
		Delta: Delta{Power: intPtr(1), Toughness: intPtr(1)}}})
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: bear, CounterName: "+1/+1", Delta: 1})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: UntilEOT, SourceCard: "Giant Growth",
		Delta: Delta{Power: intPtr(3), Toughness: intPtr(3)}}})

	c := s.Characteristics(bear)
	if c.Power == nil || c.Toughness == nil || *c.Power != 7 || *c.Toughness != 7 {
		t.Fatalf("bear = %+v", c)
	}

	lines := s.PTTrace(bear)
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l.Kind + ":" + l.Label + " ")
	}
	trace := sb.String()
	for _, want := range []string{"base:Grizzly Bears", "modifier:Glorious Anthem",
		"counter:+1/+1 counter", "modifier:Giant Growth", "total:total"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("trace %q missing %q", trace, want)
		}
	}
	total := lines[len(lines)-1]
	if total.Kind != "total" || total.Power != 7 || total.Toughness != 7 {
		t.Fatalf("total line = %+v", total)
	}
	// Layer order: all pt_modify effects render before counters (CR 613:
	// counters apply after 7b–c effects), counters before the switch.
	if strings.Index(trace, "Glorious Anthem") > strings.Index(trace, "+1/+1 counter") ||
		strings.Index(trace, "Giant Growth") > strings.Index(trace, "+1/+1 counter") {
		t.Fatalf("trace order wrong: %s", trace)
	}
}

func TestControlIsAModifierNotAWrite(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	if s.Objects[bear].Controller != 1 {
		t.Fatal("setup")
	}
	act(t, s, Action{Kind: ActionAddModifier, Seat: 2, Object: bear, Modifier: &Modifier{
		Layer: LayerControl, Duration: PermanentDuration, SourceCard: "Act of Treason",
		Delta: Delta{Controller: intPtr(2)}}})
	// The stored field never moved; the computed controller did.
	if s.Objects[bear].Controller != 1 {
		t.Fatalf("stored controller = %d", s.Objects[bear].Controller)
	}
	if c := s.Characteristics(bear); c.Controller != 2 {
		t.Fatalf("computed controller = %d", c.Controller)
	}
	// The latest control modifier in timestamp order wins.
	act(t, s, Action{Kind: ActionAddModifier, Seat: 3, Object: bear, Modifier: &Modifier{
		Layer: LayerControl, Duration: PermanentDuration, SourceCard: "Betrayal",
		Delta: Delta{Controller: intPtr(3)}}})
	if c := s.Characteristics(bear); c.Controller != 3 {
		t.Fatalf("second control: %d", c.Controller)
	}
}

func TestLayerOrderTypeAbilityColor(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	land1 := land(t, s, 1, "Conspiracy")
	bear := summon(t, s, 1, "Bear", 2, 2)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerType, Duration: WhileSourcePresent, SourceObj: land1,
		Delta: Delta{AddTypes: []string{"Goblin"}}}})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerType, Duration: PermanentDuration, SourceCard: "Amphibious Dance",
		Delta: Delta{RemoveTypes: []string{"Bear"}, AddTypes: []string{"Frog"}}}})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerAbility, Duration: PermanentDuration, SourceCard: "Flight",
		Delta: Delta{AddKeywords: []string{"Flying"}}}})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerColor, Duration: PermanentDuration, SourceCard: "Painter's Servant",
		Delta: Delta{AddColors: []string{"Blue"}}}})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerAbility, Duration: PermanentDuration, SourceCard: "Fear-forgot",
		Delta: Delta{RemoveKeywords: []string{"flying"}}}})
	c := s.Characteristics(bear)
	if !hasString(c.Types, "Creature") || !hasString(c.Types, "Goblin") || !hasString(c.Types, "Frog") {
		t.Fatalf("types = %v", c.Types)
	}
	if hasString(c.Types, "Bear") {
		t.Fatalf("bear type survived removal: %v", c.Types)
	}
	if hasString(c.Keywords, "Flying") {
		t.Fatalf("keywords = %v", c.Keywords)
	}
	if !hasString(c.Colors, "Blue") {
		t.Fatalf("colors = %v", c.Colors)
	}
}

func TestPTSetAndSwitch(t *testing.T) {
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 4, 1)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTSet, Duration: PermanentDuration, SourceCard: "Turn to Frog",
		Delta: Delta{SetPower: intPtr(1), SetToughness: intPtr(1)}}})
	// A +2/+2 on top of the set: 3/3.
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: PermanentDuration, SourceCard: "Blessing",
		Delta: Delta{Power: intPtr(2), Toughness: intPtr(2)}}})
	if c := s.Characteristics(bear); *c.Power != 3 || *c.Toughness != 3 {
		t.Fatalf("frog = %d/%d", *c.Power, *c.Toughness)
	}
	// The switch is last, after counters.
	act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: bear, CounterName: "+1/+1", Delta: 1})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: bear, Modifier: &Modifier{
		Layer: LayerPTSwitch, Duration: PermanentDuration, SourceCard: "About Face",
		Delta: Delta{Swap: true}}})
	if c := s.Characteristics(bear); *c.Power != 4 || *c.Toughness != 4 {
		t.Fatalf("swapped frog = %d/%d", *c.Power, *c.Toughness)
	}
	// An asymmetric case proves the swap itself: 2/5 becomes 5/2.
	thing := summon(t, s, 1, "Wall", 2, 5)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: thing, Modifier: &Modifier{
		Layer: LayerPTSwitch, Duration: PermanentDuration, SourceCard: "About Face",
		Delta: Delta{Swap: true}}})
	if c := s.Characteristics(thing); *c.Power != 5 || *c.Toughness != 2 {
		t.Fatalf("wall = %d/%d", *c.Power, *c.Toughness)
	}
}

func TestUnknownPTStaysUnknown(t *testing.T) {
	s := start(t, nil)
	// An object whose base was never declared — "a creature, we don't
	// know which" — stays honest under modifiers.
	id := s.NextObject
	act(t, s, Action{Kind: ActionCreateToken, Seat: 1, Token: &TokenSpec{Name: "Morph"}})
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: LayerPTModify, Duration: PermanentDuration, SourceCard: "Blessing",
		Delta: Delta{Power: intPtr(2), Toughness: intPtr(2)}}})
	c := s.Characteristics(id)
	if c.Power != nil || c.Toughness != nil {
		t.Fatalf("unknown PT became %v/%v", c.Power, c.Toughness)
	}
	total := s.PTTrace(id)[len(s.PTTrace(id))-1]
	if total.Note != "P/T unknown" {
		t.Fatalf("total = %+v", total)
	}
	// Declaring the base makes it known from then on.
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: id, Modifier: &Modifier{
		Layer: LayerPTSet, Duration: PermanentDuration, SourceCard: "Reveal",
		Delta: Delta{SetPower: intPtr(3), SetToughness: intPtr(3)}}})
	if c := s.Characteristics(id); *c.Power != 5 || *c.Toughness != 5 {
		t.Fatalf("revealed = %d/%d", *c.Power, *c.Toughness)
	}
}

func TestCopyLayerTakesSourceBase(t *testing.T) {
	s := start(t, nil)
	toMain(t, s)
	vesuva := land(t, s, 1, "Vesuva")
	target := summon(t, s, 2, "Darksteel Colossus", 11, 11)
	act(t, s, Action{Kind: ActionAddModifier, Seat: 1, Object: vesuva, Modifier: &Modifier{
		Layer: LayerCopy, Duration: PermanentDuration, SourceCard: "Vesuva",
		Delta: Delta{CopyOf: target}}})
	c := s.Characteristics(vesuva)
	if c.Power == nil || *c.Power != 11 || c.Name != "Darksteel Colossus" {
		t.Fatalf("copy = %+v", c)
	}
}

func TestCounterAnnihilation(t *testing.T) {
	// Counter arithmetic is honest arithmetic; the +1/+1 / -1/-1
	// annihilation state-based action (CR 704.5r) fires the moment both
	// are present, so 2×(+1/+1) and 1×(-1/-1) annihilate one pair and
	// leave the bear standing at the base plus what survived.
	s := start(t, nil)
	bear := summon(t, s, 1, "Bear", 2, 2)
	evs := act(t, s, Action{Kind: ActionAdjustCounters, Seat: 1, OnObject: bear, CounterName: "+1/+1", Delta: 2})
	for _, e := range evs {
		if e.Kind == EventCounterChanged && e.Name == "-1/-1" {
			t.Fatal("annihilated a counter that was never there")
		}
	}
	evs = act(t, s, Action{Kind: ActionAdjustCounters, Seat: 2, OnObject: bear, CounterName: "-1/-1", Delta: 1})
	annihilated := 0
	for _, e := range evs {
		if e.Kind == EventCounterChanged && e.Name == "-1/-1" && e.Delta == -1 {
			annihilated++
		}
	}
	if annihilated != 1 {
		t.Fatalf("annihilation rows = %d in %+v", annihilated, evs)
	}
	if o := s.Objects[bear]; o.Counters["+1/+1"] != 1 || o.Counters["-1/-1"] != 0 {
		t.Fatalf("counters = %v", o.Counters)
	}
	if c := s.Characteristics(bear); *c.Power != 3 || *c.Toughness != 3 {
		t.Fatalf("bear = %d/%d", *c.Power, *c.Toughness)
	}
}
